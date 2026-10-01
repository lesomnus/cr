package blob

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"
)

func TestS3CredentialsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	write(t, path, `{"Version": 1, "AccessKeyId": "AKIA1", "SecretAccessKey": "s1", "SessionToken": "t1", "Expiration": "2026-10-01T12:00:00Z"}`)

	p := S3CredentialsFile(path)
	c, err := p.Retrieve(context.Background())
	require.NoError(t, err)
	require.Equal(t, flob.Credentials{
		AccessKeyID: "AKIA1", SecretAccessKey: "s1", SessionToken: "t1",
		Expires: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	}, c)

	// Replaced as a set, by a rename.
	write(t, path, `{"AccessKeyId": "AKIA2", "SecretAccessKey": "s2"}`)
	c, err = p.Retrieve(context.Background())
	require.NoError(t, err)
	require.Equal(t, flob.Credentials{AccessKeyID: "AKIA2", SecretAccessKey: "s2"}, c)

	// A set that does not parse after a good one keeps the good one: it is a
	// failed read, like a file caught between its rename and its chown.
	write(t, path, `{"AccessKeyId": "AKIA3"`)
	c, err = p.Retrieve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "AKIA2", c.AccessKeyID)
}

func TestS3CredentialsFileRefusesWhatIsNotASet(t *testing.T) {
	for body, want := range map[string]string{
		``: "unexpected end of JSON input",
		`{"Version": 2, "AccessKeyId": "a", "SecretAccessKey": "s"}`:             "Version 2",
		`{"SecretAccessKey": "s"}`:                                               "AccessKeyId: not set",
		`{"AccessKeyId": "a"}`:                                                   "SecretAccessKey: not set",
		`{"AccessKeyId": "a", "SecretAccessKey": "s", "Expiration": "tomorrow"}`: "Expiration",
	} {
		path := filepath.Join(t.TempDir(), "credentials.json")
		write(t, path, body)
		_, err := S3CredentialsFile(path).Retrieve(context.Background())
		require.ErrorContains(t, err, "credentials file "+path, body)
		require.ErrorContains(t, err, want, body)
	}

	_, err := S3CredentialsFile(filepath.Join(t.TempDir(), "absent")).Retrieve(context.Background())
	require.ErrorContains(t, err, "absent")
}

// The store signs each request with the set the file holds at that moment.
func TestS3StoresSignWithARotatedCredentialsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	write(t, path, `{"AccessKeyId": "AKIA1", "SecretAccessKey": "s1"}`)

	keyID := regexp.MustCompile(`Credential=([^/]+)/`)
	var signed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m := keyID.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
			signed = append(signed, m[1])
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	stores, err := flob.NewS3Stores(flob.S3Config{
		Endpoint:     srv.URL,
		Region:       "us-east-1",
		Bucket:       "cr",
		UsePathStyle: true,
		Credentials:  S3CredentialsFile(path),
	})
	require.NoError(t, err)
	store := stores.Use("app")

	ask := func() string {
		t.Helper()
		before := len(signed)
		_, _ = store.Stat(context.Background(), flob.Digest(digest.FromString("x")))
		require.Greater(t, len(signed), before, "nothing was signed")
		return signed[len(signed)-1]
	}

	require.Equal(t, "AKIA1", ask())
	write(t, path, `{"AccessKeyId": "AKIA2", "SecretAccessKey": "s2"}`)
	require.Equal(t, "AKIA2", ask())
}
