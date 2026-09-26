package blob

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// write publishes a token the way one is published: a temporary name and a
// rename, so a reader never sees half of it.
func write(t *testing.T, path, token string) {
	t.Helper()

	tmp := path + ".tmp"
	require.NoError(t, os.WriteFile(tmp, []byte(token), 0o600))
	require.NoError(t, os.Rename(tmp, path))
}

// bearers is an upstream that records what it was asked with.
func bearers(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	return srv
}

// The credential is sent from the FIRST request, not after a challenge. The
// upstream never issued this token and has nothing to exchange; waiting for a
// 401 would refuse a request for every manifest and blob before the one that
// worked.
func TestUpstreamSendsTheTokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	write(t, path, "first")

	var seen []string
	srv := bearers(t, &seen)
	up, err := NewUpstream(srv.URL, "", "", WithTokenFile(path))
	require.NoError(t, err)

	_, err = up.do(context.Background(), http.MethodHead, "team/app", "/v2/team/app/manifests/1", nil)
	require.NoError(t, err)
	require.Equal(t, []string{"Bearer first"}, seen)
}

// And the rotation, which is the reason it is a file: the token is replaced on
// disk and the next request carries the new one, with nothing restarted.
func TestUpstreamPicksUpARotatedToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	write(t, path, "first")

	var seen []string
	srv := bearers(t, &seen)
	up, err := NewUpstream(srv.URL, "", "", WithTokenFile(path))
	require.NoError(t, err)

	ask := func() {
		t.Helper()
		_, err := up.do(context.Background(), http.MethodHead, "team/app", "/v2/team/app/manifests/1", nil)
		require.NoError(t, err)
	}

	ask()
	was, err := os.Stat(path)
	require.NoError(t, err)

	// Same length, and the same instant: what a coarse filesystem leaves behind
	// when two writes land in one tick. The file is a different one, which is
	// what a rename means and what this compares.
	write(t, path, "secnd")
	require.NoError(t, os.Chtimes(path, was.ModTime(), was.ModTime()))

	ask()
	require.Equal(t, []string{"Bearer first", "Bearer secnd"}, seen)
}

// Rename is atomic for content and not for permissions: between the rename and
// the chown that follows it, the file is there and unreadable. Refusing to
// serve then would be refusing BECAUSE the credential was being renewed, and
// the token in hand is still valid — it is replaced at half its life.
func TestUpstreamKeepsTheTokenWhileTheFileIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0000 file is still readable")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	write(t, path, "first")

	var seen []string
	srv := bearers(t, &seen)
	up, err := NewUpstream(srv.URL, "", "", WithTokenFile(path))
	require.NoError(t, err)

	_, err = up.do(context.Background(), http.MethodHead, "team/app", "/v2/team/app/manifests/1", nil)
	require.NoError(t, err)

	write(t, path, "secnd")
	require.NoError(t, os.Chmod(path, 0o000))

	_, err = up.do(context.Background(), http.MethodHead, "team/app", "/v2/team/app/manifests/1", nil)
	require.NoError(t, err)
	require.Equal(t, []string{"Bearer first", "Bearer first"}, seen)

	// And the window closes.
	require.NoError(t, os.Chmod(path, 0o600))
	_, err = up.do(context.Background(), http.MethodHead, "team/app", "/v2/team/app/manifests/1", nil)
	require.NoError(t, err)
	require.Equal(t, "Bearer secnd", seen[len(seen)-1])
}

// A file that was never readable has nothing to fall back on, and a request
// sent anonymously would be refused by the upstream for a reason naming
// neither this file nor the fact that it could not be read.
func TestUpstreamFailsWhenTheTokenWasNeverRead(t *testing.T) {
	var seen []string
	srv := bearers(t, &seen)
	up, err := NewUpstream(srv.URL, "", "", WithTokenFile(filepath.Join(t.TempDir(), "absent")))
	require.NoError(t, err)

	_, err = up.do(context.Background(), http.MethodHead, "team/app", "/v2/team/app/manifests/1", nil)
	require.ErrorIs(t, err, ErrUpstreamUnauthorized)
	require.Empty(t, seen, "nothing should have been sent without a credential")
}

// An empty file is a failed read and not a good one: it is what a provisioning
// unit leaves behind before anything has been minted into it.
func TestUpstreamRefusesAnEmptyTokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	write(t, path, "\n")

	var seen []string
	srv := bearers(t, &seen)
	up, err := NewUpstream(srv.URL, "", "", WithTokenFile(path))
	require.NoError(t, err)

	_, err = up.do(context.Background(), http.MethodHead, "team/app", "/v2/team/app/manifests/1", nil)
	require.ErrorIs(t, err, ErrUpstreamUnauthorized)
	require.Empty(t, seen)
}

// The whitespace a token picks up from `echo` or a heredoc is not part of it; a
// header value carrying a newline is rejected by the server for a reason nobody
// guesses.
func TestTokenFileTrimsWhatAToolLeaves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	write(t, path, "  first\n")

	got, err := newTokenFile(path).Token()
	require.NoError(t, err)
	require.Equal(t, "first", got)
}

// What is NOT noticed, held to on purpose: a writer that rewrites the file in
// place, same length, inside one tick. Nothing about it moved. That is the
// contract saying `rename` — which is how a credential is published — rather
// than a reason to read the file on every request.
func TestTokenFileDoesNotSeeAnInPlaceRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	write(t, path, "first")

	tf := newTokenFile(path)
	got, err := tf.Token()
	require.NoError(t, err)
	require.Equal(t, "first", got)
	was, err := os.Stat(path)
	require.NoError(t, err)

	f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString("secnd")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, os.Chtimes(path, was.ModTime(), was.ModTime()))

	got, err = tf.Token()
	require.NoError(t, err)
	require.Equal(t, "first", got)
}
