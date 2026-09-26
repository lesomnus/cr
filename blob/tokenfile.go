package blob

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// maxTokenFile is what will be read from a token file. A JWS is a few kilobytes
// at most; a file approaching this names the wrong thing, and reading it into
// memory is not how to find that out.
const maxTokenFile = 64 << 10

// tokenFile is a bearer token that lives in a file and is re-read when it
// changes.
//
// # Why a file
//
// The credential it exists for EXPIRES. Something on the host mints it — a
// device certificate presented to an authority, on a schedule of its own — and
// drops the new one where this can see it. A value in the configuration is
// right for a password that does not change and useless for one that does: the
// process would hold a dead token until somebody restarted it, and the restart
// would be whatever the token was needed for.
//
// So the file is the interface, and the only thing cr has to do is notice.
//
// # Noticing is a stat
//
// The credential is wanted per request, so there is already a moment to check
// at: one `stat` of a small file beside a request to another registry, and no
// staleness window to tune.
//
// # What is compared
//
// The FILE, and then its size and modification time. Identity first because it
// is the one that cannot be fooled: a token is published by writing a temporary
// name and renaming it into place — which is what makes the replacement atomic
// for a reader — and a rename always puts a different file at the path. Size
// and time are the second look, for a writer that appends to the one already
// there.
//
// Time alone is not enough: every token a given issuer mints is the same
// length, filesystems record modification times coarsely, and two writes inside
// one tick then leave a rotation invisible until something else about the file
// moves. It is the same comparison gantry and bosun settled on after one of
// them failed a rotation test about one run in three.
//
// # A failed read after a good one keeps the token
//
// Rename is atomic for content and not for permissions: a writer renames the
// new token into place and something else then sets its owner and mode. In
// between, the file is there and unreadable, and refusing to serve a request
// for that would be refusing it BECAUSE the credential was being renewed. The
// token in hand is still valid — it is replaced at half its life, not at
// expiry. A first read that fails has nothing to fall back on and is an error.
type tokenFile struct {
	path string

	mu    sync.Mutex
	token string
	// seen is the file the held token was read from, kept whole so os.SameFile
	// can be asked whether the path still names it.
	seen os.FileInfo
	read bool
}

func newTokenFile(path string) *tokenFile { return &tokenFile{path: path} }

// Token is what the file says now.
func (t *tokenFile) Token() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	fi, err := os.Stat(t.path)
	switch {
	case err != nil && !t.read:
		return "", fmt.Errorf("token file %s: %w", t.path, err)
	case err != nil:
		return t.token, nil
	case t.read && t.unchanged(fi):
		return t.token, nil
	case fi.Size() > maxTokenFile:
		if t.read {
			return t.token, nil
		}
		return "", fmt.Errorf("token file %s: %d bytes, over the %d cap", t.path, fi.Size(), maxTokenFile)
	}

	b, err := os.ReadFile(t.path)
	if err != nil {
		if t.read {
			return t.token, nil
		}
		return "", fmt.Errorf("token file %s: %w", t.path, err)
	}

	// Trailing whitespace goes: a token written by `echo` or a heredoc carries a
	// newline, and a header value with one in it is rejected by the server for a
	// reason nobody guesses.
	//
	// An empty result is a failed read and not a good one. A zero-byte file is
	// what a provisioning unit leaves behind before anything has been minted
	// into it; keeping it would send every request anonymously, behind a 401
	// that names neither this file nor the fact that it was empty.
	token := strings.TrimSpace(string(b))
	if token == "" {
		if t.read {
			return t.token, nil
		}
		return "", fmt.Errorf("token file %s: empty", t.path)
	}

	t.token, t.seen, t.read = token, fi, true

	return t.token, nil
}

// unchanged reports whether the path still names the file the held token was
// read from, unmodified since.
func (t *tokenFile) unchanged(fi os.FileInfo) bool {
	return os.SameFile(t.seen, fi) &&
		fi.Size() == t.seen.Size() &&
		fi.ModTime().Equal(t.seen.ModTime())
}
