package blob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lesomnus/flob"
)

// S3CredentialsFile is the S3 credentials in the file at path, re-read when it
// changes: the JSON an AWS `credential_process` prints, which is a set and not
// three values.
//
//	{"Version": 1, "AccessKeyId": "...", "SecretAccessKey": "...",
//	 "SessionToken": "...", "Expiration": "2026-10-01T12:00:00Z"}
//
// # Why one file
//
// Temporary credentials are replaced WHOLE: a new key id, its secret and its
// session token. Read as three files, a request signed between two of the
// renames would pair a key with another key's secret and be refused for a
// reason that names neither. One file is one rename, and every signature has
// one set from it.
//
// # Why this format
//
// It is the one that says when the set stops working. `Expiration` becomes
// [flob.Credentials.Expires]: a redirect's presigned URL does not outlive it,
// and a set that has expired fails a request before S3 is asked, naming the
// time. The shared credentials file has no such field. `SessionToken` and
// `Expiration` are optional, for a key that does not expire.
//
// The file follows [newSecretFile]'s rules: compared by identity, size and
// time; a read that fails, or does not parse, after a good one keeps the set it
// has; a first one that fails is an error.
func S3CredentialsFile(path string) flob.CredentialsProvider {
	f := &watchedFile[flob.Credentials]{path: path, what: "credentials file", parse: parseS3Credentials}
	return flob.CredentialsFunc(func(context.Context) (flob.Credentials, error) {
		return f.Value()
	})
}

func parseS3Credentials(b []byte) (flob.Credentials, error) {
	var v struct {
		Version         *int
		AccessKeyId     string
		SecretAccessKey string
		SessionToken    string
		Expiration      string
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return flob.Credentials{}, err
	}
	switch {
	case v.Version != nil && *v.Version != 1:
		return flob.Credentials{}, fmt.Errorf("Version %d; only 1 is known", *v.Version)
	case v.AccessKeyId == "":
		return flob.Credentials{}, errors.New("AccessKeyId: not set")
	case v.SecretAccessKey == "":
		return flob.Credentials{}, errors.New("SecretAccessKey: not set")
	}
	c := flob.Credentials{
		AccessKeyID:     v.AccessKeyId,
		SecretAccessKey: v.SecretAccessKey,
		SessionToken:    v.SessionToken,
	}
	if v.Expiration != "" {
		t, err := time.Parse(time.RFC3339, v.Expiration)
		if err != nil {
			return flob.Credentials{}, fmt.Errorf("Expiration: %w", err)
		}
		c.Expires = t
	}
	return c, nil
}
