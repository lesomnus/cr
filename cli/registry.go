package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/lesomnus/flob"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/blob"
	"github.com/lesomnus/cr/cmd"
	"github.com/lesomnus/cr/gc"
	"github.com/lesomnus/cr/gc/entruns"
	"github.com/lesomnus/cr/httpx"
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
	stores, proxies, cache, err := Proxies(c.Registry, stores)
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

	var policy func() *auth.Policy
	if guard != nil {
		policy = guard.Policy.Current
	}
	collector := gc.New(gc.Config{
		Stores:    stores,
		Index:     ix,
		Policy:    policy,
		Untagged:  c.Registry.Gc.Untagged,
		Delay:     c.Registry.Gc.Delay,
		Every:     c.Registry.Gc.Every,
		FullEvery: c.Registry.Gc.FullEvery,
		Leader:    ix,
		Runs:      entruns.New(s.Ent),
		Cache:     cache,
	})

	reg := registry.New(registry.Config{
		Stores:           stores,
		Index:            ix,
		MaxManifestSize:  c.Registry.MaxManifestSize,
		DisableWellKnown: c.Registry.DisableWellKnown,
		Guard:            guard,
		Redirect:         c.Registry.Storage.Redirect.Enabled,
		RedirectTTL:      c.Registry.Storage.Redirect.Ttl,
		Collector:        collector,
		Proxies:          proxies,
	})

	instrument := func(h http.Handler) http.Handler {
		return httpx.Instrument(ctx, registry.RouteOf, h)
	}
	if s.Routes == nil {
		s.Routes = map[string]http.Handler{}
	}
	s.Routes["/v2/"] = instrument(reg)
	s.Routes["/v1/"] = instrument(reg.V1())
	s.Routes["/admin/"] = instrument(reg.Admin())
	if guard != nil {
		s.Routes["/token"] = instrument(http.HandlerFunc(guard.ServeToken))
		if guard.Exchange > 0 {
			s.Routes["/token/exchange"] = instrument(http.HandlerFunc(guard.ServeExchange))
		}
		s.Routes["/.well-known/jwks.json"] = instrument(http.HandlerFunc(guard.ServeJWKS))
	}
	s.Routes["/healthz"] = httpx.Live()
	s.Routes["/readyz"] = httpx.Ready(s.Stopping, s.Db.PingContext)

	s.Spin = append(s.Spin, ix, collector)
	return nil
}

// Stores opens the blob stores the configuration names: one, or one per route
// with the first holding whatever no route covers.
func Stores(c cmd.StorageConfig) (flob.Stores, error) {
	stage := flob.StageConfig{TTL: c.Upload.TTL, Retention: c.Upload.Retention}
	base, err := backend("registry.storage", c.Driver, c.Os, c.S3, stage)
	if err != nil {
		return nil, err
	}
	if len(c.Routes) == 0 {
		return base, nil
	}

	routes := []blob.Route{{Prefix: "", Stores: base}}
	for i, r := range c.Routes {
		at := fmt.Sprintf("registry.storage.routes[%d]", i)
		if r.Prefix == "" {
			return nil, fmt.Errorf("%s.prefix is empty; the store above is the one for everything else", at)
		}
		s, err := backend(at, r.Driver, r.Os, r.S3, stage)
		if err != nil {
			return nil, err
		}
		routes = append(routes, blob.Route{Prefix: r.Prefix, Stores: s})
	}
	return blob.NewRouter(routes...)
}

func backend(at string, driver string, o cmd.OsStorageConfig, s cmd.S3StorageConfig, stage flob.StageConfig) (flob.Stores, error) {
	switch driver {
	case "", "os":
		if o.Root == "" {
			return nil, fmt.Errorf("%s.os.root is not set", at)
		}
		if err := os.MkdirAll(o.Root, 0o755); err != nil {
			return nil, fmt.Errorf("%s.os.root: %w", at, err)
		}
		return flob.NewOsStores(o.Root, stage), nil
	case "s3":
		stores, err := flob.NewS3Stores(flob.S3Config{
			Client:         s3Client(),
			Stage:          stage,
			StagePartSize:  s.PartSize,
			Endpoint:       s.Endpoint,
			PublicEndpoint: s.PublicEndpoint,
			Region:         s.Region,
			Bucket:         s.Bucket,
			Prefix:         s.Prefix,
			UsePathStyle:   s.PathStyle,
			Credentials: flob.Credentials{
				AccessKeyID:     s.AccessKeyId,
				SecretAccessKey: s.SecretAccessKey,
				SessionToken:    s.SessionToken,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("%s.s3: %w", at, err)
		}
		return stores, nil
	case "memory":
		return flob.NewMemStores(stage), nil
	default:
		return nil, fmt.Errorf("%s.driver: unknown driver %q", at, driver)
	}
}

// s3Client is what the S3 store sends its requests with. Go's default
// transport keeps two idle connections per host and opens a new one for
// every request past them, which at the registry's concurrency left a
// bucket's host with thousands of connections in TIME_WAIT and no port to
// open the next; this keeps enough for the requests in flight to reuse.
func s3Client() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 256
	t.MaxIdleConnsPerHost = 256
	t.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: t}
}

// Proxies makes the repositories each `registry.proxies` entry covers read
// through to its upstream, and answers the stores to use, the registry's
// proxies, and how long each cache keeps what nobody pulls.
func Proxies(c cmd.RegistryConfig, base flob.Stores) (flob.Stores, []*registry.Proxy, func(string) time.Duration, error) {
	if len(c.Proxies) == 0 {
		return base, nil, nil, nil
	}
	var (
		ps     []*registry.Proxy
		routes []blob.CacheRoute
		keep   = map[string]time.Duration{}
	)
	for i, pc := range c.Proxies {
		up, err := blob.NewUpstream(pc.Upstream, pc.Username, pc.Password)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("registry.proxies[%d].upstream: %w", i, err)
		}
		if _, ok := keep[pc.Prefix]; ok {
			return nil, nil, nil, fmt.Errorf("registry.proxies[%d].prefix: %q is already a cache", i, pc.Prefix)
		}
		p := &registry.Proxy{Prefix: pc.Prefix, Upstream: up, Remote: pc.Remote, TagTTL: pc.TagTtl}
		ps = append(ps, p)
		routes = append(routes, blob.CacheRoute{Prefix: pc.Prefix, Origin: up.Stores(p.Name)})
		keep[pc.Prefix] = pc.Retention
	}
	cache := func(repo string) time.Duration {
		best, d := -1, time.Duration(0)
		for prefix, k := range keep {
			if blob.Covers(prefix, repo) && len(prefix) > best {
				best, d = len(prefix), k
			}
		}
		return d
	}
	return blob.NewCache(base, routes...), ps, cache, nil
}
