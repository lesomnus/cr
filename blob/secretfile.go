package blob

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// maxSecretFile is what will be read from a secret file. A JWS is a few
// kilobytes at most; a file approaching this names the wrong thing, and reading
// it into memory is not how to find that out.
const maxSecretFile = 64 << 10

// Secret is a credential as it is NOW: asked for each time it is used, so one
// that lives in a file can change under a running process.
type Secret interface {
	Value() (string, error)
}

// Literal is a credential written into the configuration, which is what it
// stays.
type Literal string

func (l Literal) Value() (string, error) { return string(l), nil }

// SecretFile is a credential that lives in the file at path, re-read when it
// changes.
func SecretFile(path string) Secret { return newSecretFile(path) }

// secretFile is a credential that lives in a file and is re-read when it
// changes.
//
// # Why a file
//
// A credential gets REPLACED. A bearer minted on the host — a device
// certificate presented to an authority, on a schedule of its own — expires,
// and a password is rotated; either way something drops the new one where this
// can see it. A value in the configuration holds the old one until somebody
// restarts the process, and the restart would be whatever the credential was
// needed for. A file is also how Kubernetes hands a Secret to a process.
//
// So the file is the interface, and the only thing cr has to do is notice.
//
// # Noticing is a stat
//
// The credential is wanted when it is used, so there is already a moment to
// check at: one `stat` of a small file beside a request to another registry,
// and no staleness window to tune.
//
// # What is compared
//
// The FILE, and then its size and modification time. Identity first because it
// is the one that cannot be fooled: a credential is published by writing a
// temporary name and renaming it into place — which is what makes the
// replacement atomic for a reader; it is also what Kubernetes does to a mounted
// Secret — and a rename always puts a different file at the path. Size and time
// are the second look, for a writer that appends to the one already there.
//
// Time alone is not enough: every token a given issuer mints is the same
// length, filesystems record modification times coarsely, and two writes inside
// one tick then leave a rotation invisible until something else about the file
// moves. It is the same comparison gantry and bosun settled on after one of
// them failed a rotation test about one run in three.
//
// # A failed read after a good one keeps the value
//
// Rename is atomic for content and not for permissions: a writer renames the
// new credential into place and something else then sets its owner and mode.
// In between, the file is there and unreadable, and refusing to serve a request
// for that would be refusing it BECAUSE the credential was being renewed. The
// one in hand is still valid — a token is replaced at half its life, not at
// expiry, and a password is revoked after its successor is in place. A first
// read that fails has nothing to fall back on and is an error.
type secretFile struct {
	path string

	mu    sync.Mutex
	value string
	// seen is the file the held value was read from, kept whole so os.SameFile
	// can be asked whether the path still names it.
	seen os.FileInfo
	read bool
}

func newSecretFile(path string) *secretFile { return &secretFile{path: path} }

// Value is what the file says now.
func (t *secretFile) Value() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	fi, err := os.Stat(t.path)
	switch {
	case err != nil && !t.read:
		return "", fmt.Errorf("secret file %s: %w", t.path, err)
	case err != nil:
		return t.value, nil
	case t.read && t.unchanged(fi):
		return t.value, nil
	case fi.Size() > maxSecretFile:
		if t.read {
			return t.value, nil
		}
		return "", fmt.Errorf("secret file %s: %d bytes, over the %d cap", t.path, fi.Size(), maxSecretFile)
	}

	b, err := os.ReadFile(t.path)
	if err != nil {
		if t.read {
			return t.value, nil
		}
		return "", fmt.Errorf("secret file %s: %w", t.path, err)
	}

	// Surrounding whitespace goes: a credential written by `echo` or a heredoc
	// carries a newline, and a header value with one in it is rejected by the
	// server for a reason nobody guesses.
	//
	// An empty result is a failed read and not a good one. A zero-byte file is
	// what a provisioning unit leaves behind before anything has been minted
	// into it; keeping it would send every request anonymously, behind a 401
	// that names neither this file nor the fact that it was empty.
	value := strings.TrimSpace(string(b))
	if value == "" {
		if t.read {
			return t.value, nil
		}
		return "", fmt.Errorf("secret file %s: empty", t.path)
	}

	t.value, t.seen, t.read = value, fi, true

	return t.value, nil
}

// unchanged reports whether the path still names the file the held value was
// read from, unmodified since.
func (t *secretFile) unchanged(fi os.FileInfo) bool {
	return os.SameFile(t.seen, fi) &&
		fi.Size() == t.seen.Size() &&
		fi.ModTime().Equal(t.seen.ModTime())
}
