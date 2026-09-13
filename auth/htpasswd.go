package auth

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Htpasswd authenticates against a bcrypt htpasswd file, read again when it
// changes.
//
// A bcrypt comparison is tens of milliseconds by design, and a client sending
// Basic sends it on every request, so a password that checked is remembered
// for a few minutes, keyed by a hash of it; editing the file forgets them all.
type Htpasswd struct {
	path   string
	groups map[string][]string

	mu      sync.Mutex
	users   map[string][]byte
	mtime   time.Time
	size    int64
	checked time.Time
	good    map[[32]byte]time.Time

	now func() time.Time
}

const (
	htpasswdStatEvery = time.Second
	htpasswdRemember  = 5 * time.Minute
)

// NewHtpasswd reads path now, so a file that is not there is a deployment that
// does not start rather than one that refuses everybody.
func NewHtpasswd(path string, groups map[string][]string) (*Htpasswd, error) {
	h := &Htpasswd{path: path, groups: groups, now: time.Now}
	if err := h.load(); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *Htpasswd) load() error {
	fi, err := os.Stat(h.path)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(h.path)
	if err != nil {
		return err
	}
	users := map[string][]byte{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, hash, ok := strings.Cut(line, ":")
		if !ok || name == "" {
			return fmt.Errorf("%s:%d: want user:hash", h.path, n)
		}
		if !strings.HasPrefix(hash, "$2a$") && !strings.HasPrefix(hash, "$2b$") && !strings.HasPrefix(hash, "$2y$") {
			return fmt.Errorf("%s:%d: %s: only bcrypt is supported (htpasswd -B)", h.path, n, name)
		}
		users[name] = []byte(hash)
	}
	h.users = users
	h.mtime = fi.ModTime()
	h.size = fi.Size()
	h.good = map[[32]byte]time.Time{}
	return nil
}

// reload reads the file again when it changed, at most once a second. A file
// that went missing or broke keeps the users it had.
func (h *Htpasswd) reload(now time.Time) {
	if now.Sub(h.checked) < htpasswdStatEvery {
		return
	}
	h.checked = now
	fi, err := os.Stat(h.path)
	if err != nil || (fi.ModTime().Equal(h.mtime) && fi.Size() == h.size) {
		return
	}
	h.load()
}

func (h *Htpasswd) Authenticate(ctx context.Context, username, password string) (Subject, error) {
	now := h.now()
	h.mu.Lock()
	h.reload(now)
	hash, ok := h.users[username]
	key := sha256.Sum256([]byte(username + "\x00" + password + "\x00" + string(hash)))
	until, remembered := h.good[key]
	h.mu.Unlock()

	if !ok {
		return Subject{}, ErrNotMine
	}
	if !remembered || now.After(until) {
		if err := bcrypt.CompareHashAndPassword(hash, []byte(password)); err != nil {
			return Subject{}, ErrUnauthenticated
		}
		h.mu.Lock()
		h.good[key] = now.Add(htpasswdRemember)
		h.mu.Unlock()
	}
	return Subject{ID: username, Groups: h.groups[username]}, nil
}

// StaticToken is a long-lived token from the configuration, for CI where
// there is no roster to hold robots.
type StaticToken struct {
	Name string

	// Token is the secret itself, or TokenSHA256 its hex SHA-256, which is
	// what a configuration file that is itself not secret should carry.
	Token       string
	TokenSHA256 string

	Groups []string
}

// Tokens authenticates a password that is one of the static tokens, whatever
// the username; `docker login` insists on one and it names nobody here.
type Tokens struct {
	entries []tokenEntry
}

type tokenEntry struct {
	sum    [32]byte
	name   string
	groups []string
}

func NewTokens(ts []StaticToken) (*Tokens, error) {
	out := &Tokens{}
	for _, t := range ts {
		if t.Name == "" {
			return nil, fmt.Errorf("static token: no name")
		}
		e := tokenEntry{name: t.Name, groups: t.Groups}
		switch {
		case t.TokenSHA256 != "":
			var b []byte
			if _, err := fmt.Sscanf(t.TokenSHA256, "%x", &b); err != nil || len(b) != 32 {
				return nil, fmt.Errorf("static token %q: token_sha256 is not a SHA-256 in hex", t.Name)
			}
			copy(e.sum[:], b)
		case t.Token != "":
			e.sum = sha256.Sum256([]byte(t.Token))
		default:
			return nil, fmt.Errorf("static token %q: no token", t.Name)
		}
		out.entries = append(out.entries, e)
	}
	return out, nil
}

func (t *Tokens) Authenticate(ctx context.Context, username, password string) (Subject, error) {
	sum := sha256.Sum256([]byte(password))
	for _, e := range t.entries {
		// Comparing sums of fixed length leaks nothing about the token.
		if sum == e.sum {
			return Subject{ID: e.name, Groups: e.groups}, nil
		}
	}
	return Subject{}, ErrNotMine
}
