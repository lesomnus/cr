package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/otx"
	"go.opentelemetry.io/otel/metric"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/blob"
	"github.com/lesomnus/cr/cmd"
	"github.com/lesomnus/cr/gc"
	"github.com/lesomnus/cr/gc/entruns"
	"github.com/lesomnus/cr/httpx"
	"github.com/lesomnus/cr/index/entindex"
	"github.com/lesomnus/cr/registry"
	"github.com/lesomnus/cr/trust"
)

// Registry builds the distribution API over s and puts it on s's routes, with
// the background work it needs on s's spin.
//
// It is here and not in `cmd` because the stores it opens are a disk or a
// bucket, and the sandbox that imports `cmd` has neither.
func Registry(ctx context.Context, c *cmd.Config, s *cmd.Server) error {
	stores, err := Stores(c.Registry.Storage, meterOf(ctx))
	if err != nil {
		return err
	}
	stores, proxies, cache, err := Proxies(c.Registry, stores, meterOf(ctx))
	if err != nil {
		return err
	}

	opts := []entindex.Option{entindex.WithMeter(meterOf(ctx))}
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
		Meter:     meterOf(ctx),
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
		Meter:            otx.From(ctx).Meter(),
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
		guard.Name = reg.Name
		s.Routes["/token"] = instrument(http.HandlerFunc(guard.ServeToken))
		s.Routes["/token/exchange"] = instrument(http.HandlerFunc(guard.ServeExchange))
		s.Routes["/.well-known/jwks.json"] = instrument(http.HandlerFunc(guard.ServeJWKS))
	}
	s.Routes["/healthz"] = httpx.Live()
	s.Routes["/readyz"] = httpx.Ready(s.Stopping, s.Db.PingContext)

	s.Spin = append(s.Spin, ix, collector)
	return nil
}

// meterOf is what a process measures with: the meter on ctx, which
// [Telemetry] put there.
func meterOf(ctx context.Context) metric.Meter { return otx.From(ctx).Meter() }

// Stores opens the blob stores the configuration names: one, or one per route
// with the first holding whatever no route covers. Each is measured with
// meter, by its driver.
func Stores(c cmd.StorageConfig, meter metric.Meter) (flob.Stores, error) {
	stage := flob.StageConfig{TTL: c.Upload.TTL, Retention: c.Upload.Retention}
	base, err := backend("registry.storage", c.Driver, c.Os, c.S3, stage, meter)
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
		s, err := backend(at, r.Driver, r.Os, r.S3, stage, meter)
		if err != nil {
			return nil, err
		}
		routes = append(routes, blob.Route{Prefix: r.Prefix, Stores: s})
	}
	return blob.NewRouter(routes...)
}

func backend(at string, driver string, o cmd.OsStorageConfig, s cmd.S3StorageConfig, stage flob.StageConfig, meter metric.Meter) (flob.Stores, error) {
	switch driver {
	case "", "os":
		if o.Root == "" {
			return nil, fmt.Errorf("%s.os.root is not set", at)
		}
		if err := os.MkdirAll(o.Root, 0o755); err != nil {
			return nil, fmt.Errorf("%s.os.root: %w", at, err)
		}
		return blob.Measured(flob.NewOsStores(o.Root, stage), "os", meter), nil
	case "s3":
		creds, err := s3Credentials(s)
		if err != nil {
			return nil, fmt.Errorf("%s.s3.%w", at, err)
		}
		stores, err := flob.NewS3Stores(flob.S3Config{
			Client:         s3Client(),
			Stage:          stage,
			StagePartSize:  s.PartSize,
			SpoolDir:       s.SpoolDir,
			Endpoint:       s.Endpoint,
			PublicEndpoint: s.PublicEndpoint,
			Region:         s.Region,
			Bucket:         s.Bucket,
			Prefix:         s.Prefix,
			UsePathStyle:   s.PathStyle,
			Credentials:    creds,
		})
		if err != nil {
			return nil, fmt.Errorf("%s.s3: %w", at, err)
		}
		return blob.Measured(stores, "s3", meter), nil
	case "memory":
		return blob.Measured(flob.NewMemStores(stage), "memory", meter), nil
	default:
		return nil, fmt.Errorf("%s.driver: unknown driver %q", at, driver)
	}
}

// proxyAuth is the credential a proxy's `auth` configures. The error names the
// field, for the caller to put the proxy in front of.
func proxyAuth(pc cmd.ProxyConfig) ([]blob.UpstreamOption, error) {
	switch {
	case pc.Username != "", pc.Password != "":
		return nil, errors.New("username, password: moved to auth, with kind: password")
	case pc.TokenFile != "":
		return nil, errors.New("token_file: moved to auth, with kind: bearer and token: ${file:...}")
	}

	// A cmd.Secret is a blob.Secret as it is: `Value` answers the credential,
	// read again when its file changed.
	a := pc.Auth
	switch a.Kind {
	case "":
		if a.Username != "" || !a.Password.IsZero() || !a.Token.IsZero() {
			return nil, errors.New("auth.kind: not set, so the credential beside it would not be sent; say password or bearer")
		}
		return nil, nil
	case "password":
		switch {
		case !a.Token.IsZero():
			return nil, errors.New("auth.token: is bearer's; kind is password")
		case a.Username == "":
			return nil, errors.New("auth.username: not set")
		case a.Password.IsZero():
			return nil, errors.New("auth.password: not set")
		}
		return []blob.UpstreamOption{blob.WithPassword(a.Username, a.Password)}, nil
	case "bearer":
		switch {
		case a.Username != "" || !a.Password.IsZero():
			return nil, errors.New("auth.username, auth.password: are password's; kind is bearer")
		case a.Token.IsZero():
			return nil, errors.New("auth.token: not set")
		}
		return []blob.UpstreamOption{blob.WithBearer(a.Token)}, nil
	default:
		return nil, fmt.Errorf("auth.kind: %q is not password or bearer", a.Kind)
	}
}

// s3Credentials is what an S3 store signs with: the file, re-read when it
// changes, or the keys as they are written. The error names the field, for the
// caller to put the store in front of.
func s3Credentials(s cmd.S3StorageConfig) (flob.CredentialsProvider, error) {
	for _, k := range []struct{ name, v string }{
		{"access_key_id", s.AccessKeyId},
		{"secret_access_key", s.SecretAccessKey},
		{"session_token", s.SessionToken},
	} {
		switch {
		case s.CredentialsFile != "" && k.v != "":
			return nil, fmt.Errorf("%s: credentials_file is the whole set; set one or the other", k.name)
		case strings.HasPrefix(k.v, "${file:"):
			// Three files would be read apart, and a rotation caught between
			// two of them signs with a key and another key's secret.
			return nil, fmt.Errorf("%s: ${file:...} is not read here; put the set in credentials_file", k.name)
		}
	}
	if s.CredentialsFile != "" {
		return blob.S3CredentialsFile(s.CredentialsFile), nil
	}
	return flob.Credentials{
		AccessKeyID:     s.AccessKeyId,
		SecretAccessKey: s.SecretAccessKey,
		SessionToken:    s.SessionToken,
	}, nil
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
func Proxies(c cmd.RegistryConfig, base flob.Stores, meter metric.Meter) (flob.Stores, []*registry.Proxy, func(string) time.Duration, error) {
	if len(c.Proxies) == 0 {
		return base, nil, nil, nil
	}
	var (
		ps     []*registry.Proxy
		routes []blob.CacheRoute
		keep   = map[string]time.Duration{}
		hosts  = map[string]string{}
	)
	for i, pc := range c.Proxies {
		hs := make([]string, len(pc.Hosts))
		for j, h := range pc.Hosts {
			h = strings.ToLower(h)
			hs[j] = h
			switch {
			case pc.Prefix == "":
				return nil, nil, nil, fmt.Errorf("registry.proxies[%d].hosts: the empty prefix already takes every name", i)
			case h == "" || strings.ContainsAny(h, ":/"):
				return nil, nil, nil, fmt.Errorf("registry.proxies[%d].hosts[%d]: %q is not a host name", i, j, h)
			}
			if p, ok := hosts[h]; ok {
				return nil, nil, nil, fmt.Errorf("registry.proxies[%d].hosts[%d]: %q is already %q's", i, j, h, p)
			}
			hosts[h] = pc.Prefix
		}
		auth, err := proxyAuth(pc)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("registry.proxies[%d].%w", i, err)
		}
		up, err := blob.NewUpstream(pc.Upstream, append(auth, blob.WithMeter(meter))...)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("registry.proxies[%d].upstream: %w", i, err)
		}
		verify, err := proxyVerify(pc.Verify)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("registry.proxies[%d].verify.%w", i, err)
		}
		if _, ok := keep[pc.Prefix]; ok {
			return nil, nil, nil, fmt.Errorf("registry.proxies[%d].prefix: %q is already a cache", i, pc.Prefix)
		}
		p := &registry.Proxy{
			Prefix:            pc.Prefix,
			Upstream:          up,
			Remote:            pc.Remote,
			Hosts:             hs,
			TagTTL:            pc.TagTtl,
			TagMaxStale:       pc.TagMaxStale,
			ReferrersTTL:      pc.ReferrersTtl,
			ReferrersMaxStale: pc.ReferrersMaxStale,
			Verify:            verify,
		}
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

// proxyVerify is the verifier a proxy's `verify` makes, or nil when it says
// nothing.
func proxyVerify(c cmd.ProxyVerifyConfig) (*trust.Verifier, error) {
	if !c.IsSet() {
		return nil, nil
	}
	return trust.New(trust.Config{
		Mode:       trust.Mode(c.Mode),
		Roots:      c.Roots,
		Identities: c.Identities,
		TSARoots:   c.TSARoots,
	})
}
