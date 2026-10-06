package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"

	"github.com/lesomnus/otx/log"
)

// SecretValue is a credential as the configuration holds it: read when asked,
// so that one in a file that was replaced is the new one.
type SecretValue interface {
	Value() (string, error)
}

// Secret vouches for one caller, by a username and a password: a credential
// kept somewhere other than the policy, for when nothing else can be asked --
// an administrator's way in while the CI provider is down, say.
//
// The caller is `<provider>:<username>`, and its one claim is `username`. It
// is one caller, so a match for it needs no `when`.
//
// Every login it accepts is logged as a warning, and every one it refuses: a
// credential like this is used rarely, and each use is worth seeing.
type Secret struct {
	name     string
	username string
	password SecretValue
}

func NewSecret(name, username string, password SecretValue) *Secret {
	return &Secret{name: name, username: username, password: password}
}

func (s *Secret) Kind() string { return "secret" }

// singular is an authenticator that vouches for exactly one caller, whose
// matches need no `when` to narrow it.
func (s *Secret) singular() {}

// Authenticate is the caller when username is this provider's and password
// is its password. Another username is not this provider's to answer for. A
// password that cannot be read -- its file is not there yet -- lets nobody in.
func (s *Secret) Authenticate(ctx context.Context, username, password string) (Subject, error) {
	if username != s.username {
		return Subject{}, ErrNotMine
	}
	l := log.From(ctx).With(slog.String("provider", s.name), slog.String("username", username))
	want, err := s.password.Value()
	if err != nil {
		l.ErrorContext(ctx, "auth: secret: the password cannot be read; nobody logs in by it", slog.String("err", err.Error()))
		return Subject{}, ErrUnauthenticated
	}
	// Compared as digests, so the time taken says nothing of either length.
	a, b := sha256.Sum256([]byte(password)), sha256.Sum256([]byte(want))
	if subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
		l.WarnContext(ctx, "auth: secret: a wrong password")
		return Subject{}, ErrUnauthenticated
	}
	l.WarnContext(ctx, "auth: secret: logged in")
	return Subject{
		ID:       s.name + ":" + username,
		Provider: s.name,
		Claims:   map[string]any{"username": username},
	}, nil
}
