package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/xli/cfg"
	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/cmd"
)

// publish writes a file the way a credential is published: a temporary name
// and a rename, so a reader never sees half of it.
func publish(t *testing.T, path, body string) {
	t.Helper()

	tmp := path + ".tmp"
	require.NoError(t, os.WriteFile(tmp, []byte(body), 0o600))
	require.NoError(t, os.Rename(tmp, path))
}

// s3Set is `credentials` as the configuration gives it.
func s3Set(t *testing.T, v string) cmd.S3Credentials {
	t.Helper()

	var s cmd.S3Credentials
	require.NoError(t, s.UnmarshalText([]byte(v)))
	return s
}

func TestS3CredentialsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	publish(t, path, `{"Version": 1, "AccessKeyId": "AKIA1", "SecretAccessKey": "s1", "SessionToken": "t1", "Expiration": "2026-10-01T12:00:00Z"}`)

	p, err := s3Credentials(cmd.S3StorageConfig{Credentials: s3Set(t, "${file:"+path+"}")})
	require.NoError(t, err)
	c, err := p.Retrieve(context.Background())
	require.NoError(t, err)
	require.Equal(t, flob.Credentials{
		AccessKeyID: "AKIA1", SecretAccessKey: "s1", SessionToken: "t1",
		Expires: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	}, c)

	// Replaced as a set, by a rename.
	publish(t, path, `{"AccessKeyId": "AKIA2", "SecretAccessKey": "s2"}`)
	c, err = p.Retrieve(context.Background())
	require.NoError(t, err)
	require.Equal(t, flob.Credentials{AccessKeyID: "AKIA2", SecretAccessKey: "s2"}, c)

	// A set that does not parse after a good one keeps the good one: it is a
	// failed read, like a file caught between its rename and its chown.
	publish(t, path, `{"AccessKeyId": "AKIA3"`)
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
		publish(t, path, body)
		p, err := s3Credentials(cmd.S3StorageConfig{Credentials: s3Set(t, "${file:"+path+"}")})
		require.NoError(t, err)
		_, err = p.Retrieve(context.Background())
		require.ErrorContains(t, err, "secret file "+path, body)
		require.ErrorContains(t, err, want, body)
	}

	p, err := s3Credentials(cmd.S3StorageConfig{Credentials: s3Set(t, "${file:"+filepath.Join(t.TempDir(), "absent")+"}")})
	require.NoError(t, err)
	_, err = p.Retrieve(context.Background())
	require.ErrorContains(t, err, "absent")

	// Written out in place, it is read as it is loaded, and refused there.
	var s cmd.S3Credentials
	require.ErrorContains(t, s.UnmarshalText([]byte(`{"AccessKeyId": "a"}`)), "SecretAccessKey: not set")
}

// TestS3CredentialsAreReadFromTheConfiguration is the set as a deployment
// writes it, through the loader `cr serve` reads its configuration with:
// `credentials` takes `${file:...}`, and a file not there yet is a warning
// rather than a failure, since temporary credentials may be minted after cr
// starts. `config` prints where the set is and not what it is.
func TestS3CredentialsAreReadFromTheConfiguration(t *testing.T) {
	x := require.New(t)

	dir := t.TempDir()
	set := filepath.Join(dir, "credentials.json")
	conf := filepath.Join(dir, "cr.yaml")
	x.NoError(os.WriteFile(conf, []byte("registry:\n  storage:\n    driver: s3\n    s3:\n      bucket: cr\n      credentials: ${file:"+set+"}\n"), 0o600))

	var c cmd.Config
	s, err := cfg.New(cmd.Name, &c).Load(conf, nil)
	x.NoError(err)
	x.Len(s.Warnings, 1)
	x.ErrorContains(s.Warnings[0], set)

	publish(t, set, `{"AccessKeyId": "AKIA1", "SecretAccessKey": "s1"}`)
	p, err := s3Credentials(c.Registry.Storage.S3)
	x.NoError(err)
	got, err := p.Retrieve(context.Background())
	x.NoError(err)
	x.Equal("AKIA1", got.AccessKeyID)

	b := &strings.Builder{}
	x.NoError(s.Print(b))
	x.Contains(b.String(), "credentials: ${file:"+set+"}")
	x.NotContains(b.String(), "s1")
}

// The store signs each request with the set the file holds at that moment.
func TestS3StoresSignWithARotatedCredentialsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	publish(t, path, `{"AccessKeyId": "AKIA1", "SecretAccessKey": "s1"}`)

	keyID := regexp.MustCompile(`Credential=([^/]+)/`)
	var signed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m := keyID.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
			signed = append(signed, m[1])
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	creds, err := s3Credentials(cmd.S3StorageConfig{Credentials: s3Set(t, "${file:"+path+"}")})
	require.NoError(t, err)
	stores, err := flob.NewS3Stores(flob.S3Config{
		Endpoint:     srv.URL,
		Region:       "us-east-1",
		Bucket:       "cr",
		UsePathStyle: true,
		Credentials:  creds,
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
	publish(t, path, `{"AccessKeyId": "AKIA2", "SecretAccessKey": "s2"}`)
	require.Equal(t, "AKIA2", ask())
}
