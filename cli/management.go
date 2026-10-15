package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/lesomnus/cr/cmd"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/payday/auth"
	"github.com/lesomnus/payday/frame"
	"github.com/lesomnus/payday/rbac"
	"github.com/lesomnus/xli/cfg"
)

// managementDocument replaces credentials and their policy together. A token
// must never authenticate from one revision and receive roles from another.
type managementDocument struct {
	Tokens []cmd.ManagementTokenConfig `yaml:"tokens"`
	Policy *rbac.Rules                 `yaml:"policy"`
}

func (d *managementDocument) Validate() error { _, err := managementIdentities(*d); return err }

func managementIdentities(d managementDocument) (map[[32]byte]auth.Identity, error) {
	var policy *rbac.Snapshot
	if d.Policy != nil {
		var err error
		policy, err = rbac.New(*d.Policy)
		if err != nil {
			return nil, fmt.Errorf("management.policy: %w", err)
		}
	}
	ids := map[[32]byte]auth.Identity{}
	for _, t := range d.Tokens {
		id, err := auth.ParseName(t.Holder)
		if err != nil {
			return nil, fmt.Errorf("management.tokens: holder %q: %w", t.Holder, err)
		}
		id.Authority = cmd.ConfigurationAuthority
		id.Grant = frame.Whole()
		for _, m := range t.Allow {
			if err := rbac.ValidateMethod(m); err != nil {
				return nil, fmt.Errorf("management.tokens: holder %q: %w", t.Holder, err)
			}
		}
		if policy != nil {
			gs, err := policy.Grants(context.Background(), id.Name())
			if err != nil {
				return nil, fmt.Errorf("management.tokens: holder %q: %w", t.Holder, err)
			}
			methods := rbac.Methods(gs)
			if t.Allow != nil {
				var limited []string
				for _, a := range methods {
					for _, b := range t.Allow {
						if m, ok := frame.IntersectMethod(a, b); ok {
							limited = append(limited, m)
						}
					}
				}
				methods = limited
			}
			id.Grant = id.Grant.To(methods...)
		} else if t.Allow != nil {
			id.Grant = id.Grant.To(t.Allow...)
		}
		var sum [32]byte
		switch {
		case t.TokenSha256 != "":
			if t.Token != "" {
				return nil, fmt.Errorf("management.tokens: holder %q: choose token or token_sha256", t.Holder)
			}
			b, err := hex.DecodeString(t.TokenSha256)
			if err != nil || len(b) != 32 {
				return nil, fmt.Errorf("management.tokens: holder %q: token_sha256 is not a SHA-256 in hex", t.Holder)
			}
			copy(sum[:], b)
		case t.Token != "":
			sum = sha256.Sum256([]byte(t.Token))
		default:
			return nil, fmt.Errorf("management.tokens: holder %q: no token", t.Holder)
		}
		if _, ok := ids[sum]; ok {
			return nil, fmt.Errorf("management.tokens: holder %q: duplicate token", t.Holder)
		}
		ids[sum] = id
	}
	return ids, nil
}

type managementStore struct {
	mu        sync.RWMutex
	refreshMu sync.Mutex
	ids       map[[32]byte]auth.Identity
	changed   chan struct{}
	file      *cfg.File[managementDocument]
	revision  string
	every     time.Duration
}

func newManagementStore(c *cmd.Config) (*managementStore, error) {
	m := c.Management
	s := &managementStore{changed: make(chan struct{}), every: m.Refresh}
	if s.every <= 0 {
		s.every = 5 * time.Second
	}
	d := managementDocument{Tokens: m.Tokens, Policy: m.Policy}
	if m.File != "" {
		if len(m.Tokens) > 0 || m.Policy != nil {
			return nil, fmt.Errorf("management.file cannot accompany inline tokens or policy")
		}
		path := m.File
		if !filepath.IsAbs(path) && c.From != "" {
			path = filepath.Join(filepath.Dir(c.From), path)
		}
		s.file = cfg.NewFile[managementDocument](path)
		v, err := s.file.Load()
		if err != nil {
			return nil, fmt.Errorf("management.file: %w", err)
		}
		d = *v.Config
		s.revision = v.Revision
	}
	ids, err := managementIdentities(d)
	if err != nil {
		return nil, err
	}
	s.ids = ids
	return s, nil
}

func (s *managementStore) lookup(token string) (auth.Identity, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.ids[sha256.Sum256([]byte(token))]
	id.Changed = s.changed
	return id, ok
}

func (s *managementStore) refresh() error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	v, _, err := s.file.Reload()
	if err != nil {
		return err
	}
	s.mu.RLock()
	same := v.Revision == s.revision
	s.mu.RUnlock()
	if same {
		return nil
	}
	ids, err := managementIdentities(*v.Config)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revision == v.Revision {
		return nil
	}
	s.ids, s.revision = ids, v.Revision
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}

func (s *managementStore) SpinName() string { return "management-policy" }
func (s *managementStore) SpinEverywhere()  {}

func (s *managementStore) Spin(ctx context.Context) error {
	t := time.NewTicker(s.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := s.refresh(); err != nil {
				log.From(ctx).WarnContext(ctx, "management reload failed; keeping previous credentials and policy", slog.Any("error", err))
			}
		}
	}
}
