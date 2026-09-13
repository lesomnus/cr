package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/lesomnus/flob"

	"github.com/lesomnus/cr/cmd"
	"github.com/lesomnus/cr/index/entindex"
	"github.com/lesomnus/cr/registry"
)

// Registry builds the distribution API over s and puts it on s's routes, with
// the background work it needs on s's spin.
//
// It is here and not in `cmd` because the stores it opens are a disk or a
// bucket, and the sandbox that imports `cmd` has neither.
func Registry(ctx context.Context, c *cmd.Config, s *cmd.Server) error {
	stores, err := Stores(c.Registry.Storage)
	if err != nil {
		return err
	}

	var opts []entindex.Option
	if c.Registry.LockWait > 0 {
		opts = append(opts, entindex.WithWait(c.Registry.LockWait))
	}
	ix := entindex.New(s.Ent, opts...)

	guard, err := Guard(ctx, c, s)
	if err != nil {
		return err
	}

	reg := registry.New(registry.Config{
		Stores:           stores,
		Index:            ix,
		MaxManifestSize:  c.Registry.MaxManifestSize,
		DisableWellKnown: c.Registry.DisableWellKnown,
		Guard:            guard,
	})

	if s.Routes == nil {
		s.Routes = map[string]http.Handler{}
	}
	s.Routes["/v2/"] = reg
	if guard != nil {
		s.Routes["/token"] = http.HandlerFunc(guard.ServeToken)
		s.Routes["/.well-known/jwks.json"] = http.HandlerFunc(guard.ServeJWKS)
	}
	s.Spin = append(s.Spin, ix)
	return nil
}

// Stores opens the blob stores the configuration names.
func Stores(c cmd.StorageConfig) (flob.Stores, error) {
	stage := flob.StageConfig{TTL: c.Upload.TTL, Retention: c.Upload.Retention}
	switch c.Driver {
	case "", "os":
		root := c.Os.Root
		if root == "" {
			return nil, fmt.Errorf("registry.storage.os.root is not set")
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, fmt.Errorf("registry.storage.os.root: %w", err)
		}
		return flob.NewOsStores(root, stage), nil
	case "memory":
		return flob.NewMemStores(stage), nil
	default:
		return nil, fmt.Errorf("registry.storage.driver: unknown driver %q", c.Driver)
	}
}
