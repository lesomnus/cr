package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lesomnus/xli/cfg"
)

// S3Credentials is a set of S3 credentials in the configuration, read
// together: the JSON an AWS `credential_process` prints, which is a set and
// not three values.
//
//	{"Version": 1, "AccessKeyId": "...", "SecretAccessKey": "...",
//	 "SessionToken": "...", "Expiration": "2026-10-01T12:00:00Z"}
//
// It is a secret like [Secret], so it is usually `${file:/path}`, and the
// file is RE-READ WHEN IT CHANGES, by the same rules: a set that does not
// parse after a good one is a failed read, and keeps the good one.
//
// # Why one value
//
// Temporary credentials are replaced WHOLE: a new key id, its secret and its
// session token. Read as three files, a request signed between two of the
// renames would pair a key with another key's secret and be refused for a
// reason that names neither. One file is one rename, and every signature has
// one set from it.
//
// # Why this format
//
// It is the one that says when the set stops working. `Expiration` is
// [S3CredentialSet.Expires]: a redirect's presigned URL does not outlive it,
// and a set that has expired fails a request before S3 is asked, naming the
// time. The shared credentials file has no such field. `SessionToken` and
// `Expiration` are optional, for a key that does not expire.
type S3Credentials = cfg.SecretOf[S3CredentialSet, S3CredentialsJSON]

// S3CredentialSet is what [S3Credentials] holds.
type S3CredentialSet struct {
	AccessKeyId     string
	SecretAccessKey string
	SessionToken    string

	// Expires is when the set stops working; zero for one that does not.
	Expires time.Time
}

// S3CredentialsJSON reads an [S3CredentialSet] from the JSON an AWS
// `credential_process` prints.
type S3CredentialsJSON struct{}

func (S3CredentialsJSON) Decode(b []byte) (S3CredentialSet, error) {
	var v struct {
		Version         *int
		AccessKeyId     string
		SecretAccessKey string
		SessionToken    string
		Expiration      string
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return S3CredentialSet{}, err
	}
	switch {
	case v.Version != nil && *v.Version != 1:
		return S3CredentialSet{}, fmt.Errorf("Version %d; only 1 is known", *v.Version)
	case v.AccessKeyId == "":
		return S3CredentialSet{}, errors.New("AccessKeyId: not set")
	case v.SecretAccessKey == "":
		return S3CredentialSet{}, errors.New("SecretAccessKey: not set")
	}
	c := S3CredentialSet{
		AccessKeyId:     v.AccessKeyId,
		SecretAccessKey: v.SecretAccessKey,
		SessionToken:    v.SessionToken,
	}
	if v.Expiration != "" {
		t, err := time.Parse(time.RFC3339, v.Expiration)
		if err != nil {
			return S3CredentialSet{}, fmt.Errorf("Expiration: %w", err)
		}
		c.Expires = t
	}
	return c, nil
}
