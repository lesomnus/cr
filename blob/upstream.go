package blob

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/lesomnus/cr/telemetry"
)

// ManifestAccept is every manifest type cr stores, as an `Accept` header: a
// cache asks upstream for what it can keep, whatever the client asked for, so
// one tag does not become different manifests for different clients.
const ManifestAccept = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

// Upstream is a remote registry read on behalf of a pull-through cache: its
// manifests and blobs, through whatever token flow it challenges with.
type Upstream struct {
	base     *url.URL
	username string
	password string
	client   *http.Client

	mu     sync.Mutex
	tokens map[string]upstreamToken

	now func() time.Time

	// What every request to it costs, and what comes back: the histogram
	// `cr.cache.upstream.duration` and the counter `cr.cache.upstream.bytes`,
	// by the upstream's host and the operation. See [WithMeter].
	host     attribute.KeyValue
	duration metric.Float64Histogram
	bytes    metric.Int64Counter
}

// UpstreamOption is what [NewUpstream] takes besides its address.
type UpstreamOption func(*Upstream)

// WithMeter measures the requests to the upstream with m.
func WithMeter(m metric.Meter) UpstreamOption {
	return func(u *Upstream) {
		u.duration = telemetry.Seconds(m, "cr.cache.upstream.duration", "Duration of requests a pull-through cache made to its upstream.")
		u.bytes = telemetry.Counter(m, "cr.cache.upstream.bytes", "By", "Bytes a pull-through cache read from its upstream.")
	}
}

type upstreamToken struct {
	auth    string
	expires time.Time
}

// NewUpstream is the registry at rawURL, `https://registry-1.docker.io`,
// read with username and password when it asks for them, or anonymously.
func NewUpstream(rawURL, username, password string, opts ...UpstreamOption) (*Upstream, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("upstream %q: want an http or https URL", rawURL)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	up := &Upstream{
		base:     u,
		username: username,
		password: password,
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConnsPerHost:   16,
		}},
		tokens: map[string]upstreamToken{},
		now:    time.Now,
		host:   attribute.String("cr.cache.upstream", u.Host),
	}
	WithMeter(nil)(up)
	for _, opt := range opts {
		opt(up)
	}
	return up, nil
}

func (u *Upstream) String() string { return u.base.String() }

// do is request, timed: under the upstream's host, the operation -- `manifest
// head`, `manifest get`, `blob head`, `blob get` -- and the status, or 0 when
// nothing answered. A challenge answered on the way is part of the time.
func (u *Upstream) do(ctx context.Context, method, repo, path string, header http.Header) (*http.Response, error) {
	start := time.Now()
	res, err := u.request(ctx, method, repo, path, header)
	status := 0
	if err == nil {
		status = res.StatusCode
	}
	u.duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(
		u.host,
		attribute.String("cr.cache.operation", operationOf(method, path)),
		attribute.Int("http.response.status_code", status),
	))
	return res, err
}

func operationOf(method, path string) string {
	kind := "blob"
	if strings.Contains(path, "/manifests/") {
		kind = "manifest"
	}
	return kind + " " + strings.ToLower(method)
}

// request sends a request about repo, answering a challenge once: a bearer
// token from the realm the upstream names, kept until it expires, or Basic.
func (u *Upstream) request(ctx context.Context, method, repo, path string, header http.Header) (*http.Response, error) {
	scope := "repository:" + repo + ":pull"
	send := func(auth string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, u.base.String()+path, nil)
		if err != nil {
			return nil, err
		}
		for k, vs := range header {
			req.Header[k] = vs
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		return u.client.Do(req)
	}

	res, err := send(u.cached(scope))
	if err != nil || res.StatusCode != http.StatusUnauthorized {
		return res, err
	}
	challenge := res.Header.Get("WWW-Authenticate")
	io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	res.Body.Close()

	auth, err := u.authorize(ctx, challenge, scope)
	if err != nil {
		return nil, err
	}
	return send(auth)
}

func (u *Upstream) cached(scope string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	t, ok := u.tokens[scope]
	if !ok || !u.now().Before(t.expires) {
		return ""
	}
	return t.auth
}

// ErrUpstreamUnauthorized is an upstream that refused the credentials, or
// asked for some and was configured with none.
var ErrUpstreamUnauthorized = errors.New("upstream: unauthorized")

func (u *Upstream) authorize(ctx context.Context, challenge, scope string) (string, error) {
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "basic":
		if u.username == "" {
			return "", ErrUpstreamUnauthorized
		}
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(u.username+":"+u.password)), nil
	case "bearer":
	default:
		return "", fmt.Errorf("%w: challenge %q", ErrUpstreamUnauthorized, challenge)
	}

	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Host == "" {
		return "", fmt.Errorf("%w: realm %q", ErrUpstreamUnauthorized, params["realm"])
	}
	q := realm.Query()
	if v := params["service"]; v != "" {
		q.Set("service", v)
	}
	q.Set("scope", scope)
	realm.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	if u.username != "" {
		req.SetBasicAuth(u.username, u.password)
	}
	res, err := u.client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: token endpoint answered %s", ErrUpstreamUnauthorized, res.Status)
	}
	var v struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&v); err != nil {
		return "", err
	}
	token := v.Token
	if token == "" {
		token = v.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("%w: the token endpoint answered no token", ErrUpstreamUnauthorized)
	}
	ttl := time.Duration(v.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	auth := "Bearer " + token
	u.mu.Lock()
	u.tokens[scope] = upstreamToken{auth: auth, expires: u.now().Add(ttl - min(ttl/10, 10*time.Second))}
	u.mu.Unlock()
	return auth, nil
}

// parseChallenge reads `Bearer realm="...",service="...",scope="..."`.
func parseChallenge(v string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(v), " ")
	params := map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; {
		k, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		var val string
		if strings.HasPrefix(after, `"`) {
			end := strings.IndexByte(after[1:], '"')
			if end < 0 {
				val, rest = after[1:], ""
			} else {
				val, rest = after[1:1+end], after[2+end:]
			}
		} else {
			val, rest, _ = strings.Cut(after, ",")
		}
		params[k] = strings.TrimSpace(val)
		rest = strings.TrimLeft(strings.TrimSpace(rest), ",")
	}
	return scheme, params
}

func upstreamErr(what string, res *http.Response) error {
	if res.StatusCode == http.StatusNotFound {
		return flob.ErrNotExist
	}
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: %s answered %s", ErrUpstreamUnauthorized, what, res.Status)
	}
	return fmt.Errorf("upstream: %s answered %s", what, res.Status)
}

// HeadManifest answers the digest the upstream has for reference in repo, or
// nothing when it does not say.
func (u *Upstream) HeadManifest(ctx context.Context, repo, reference string) (digest.Digest, error) {
	res, err := u.do(ctx, http.MethodHead, repo, "/v2/"+repo+"/manifests/"+reference, http.Header{"Accept": {ManifestAccept}})
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", upstreamErr("manifest HEAD", res)
	}
	d, err := digest.Parse(res.Header.Get("Docker-Content-Digest"))
	if err != nil {
		return "", nil
	}
	return d, nil
}

// GetManifest answers the manifest reference names in repo: its bytes, its
// media type, and its digest, checked against reference when that is one.
func (u *Upstream) GetManifest(ctx context.Context, repo, reference string, max int64) ([]byte, string, digest.Digest, error) {
	res, err := u.do(ctx, http.MethodGet, repo, "/v2/"+repo+"/manifests/"+reference, http.Header{"Accept": {ManifestAccept}})
	if err != nil {
		return nil, "", "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, "", "", upstreamErr("manifest GET", res)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, max+1))
	if err != nil {
		return nil, "", "", err
	}
	u.bytes.Add(ctx, int64(len(b)), metric.WithAttributes(u.host, attribute.String("cr.cache.operation", "manifest get")))
	if int64(len(b)) > max {
		return nil, "", "", fmt.Errorf("upstream: the manifest is larger than %d bytes", max)
	}
	mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))

	// A tag has no colon and a digest does; `digest.Parse` answers its input
	// even when that is not a digest, so it is not the test.
	algo := digest.Canonical
	var want digest.Digest
	if strings.Contains(reference, ":") {
		if want, err = digest.Parse(reference); err != nil {
			return nil, "", "", fmt.Errorf("upstream: reference %q: %w", reference, err)
		}
		algo = want.Algorithm()
	}
	d := algo.FromBytes(b)
	if want != "" && want != d {
		return nil, "", "", fmt.Errorf("upstream: %s hashes to %s", reference, d)
	}
	return b, mt, d, nil
}

// Stores is the upstream's blobs as flob stores, read-only, whose namespaces
// are cr's repository names and name maps to the upstream's.
func (u *Upstream) Stores(name func(repo string) string) flob.Stores {
	return upstreamStores{u: u, name: name}
}

type upstreamStores struct {
	u    *Upstream
	name func(string) string
}

func (s upstreamStores) Use(id string) flob.Store {
	return &upstreamStore{u: s.u, repo: s.name(id)}
}

type upstreamStore struct {
	u    *Upstream
	repo string
}

func noLabels(context.Context) (flob.Labels, error) { return nil, nil }

func (s *upstreamStore) Add(context.Context, flob.Meta, io.Reader) (flob.Meta, error) {
	return flob.Meta{}, flob.ErrUnimplemented
}

func (s *upstreamStore) Label(context.Context, flob.Digest, flob.Labels) error {
	return flob.ErrUnimplemented
}

func (s *upstreamStore) Erase(context.Context, flob.Digest) error {
	return flob.ErrUnimplemented
}

func (s *upstreamStore) path(d flob.Digest) string {
	return "/v2/" + s.repo + "/blobs/" + string(d)
}

func (s *upstreamStore) Stat(ctx context.Context, d flob.Digest) (flob.Info, error) {
	res, err := s.u.do(ctx, http.MethodHead, s.repo, s.path(d), nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, upstreamErr("blob HEAD", res)
	}
	size := res.ContentLength
	if size < 0 {
		size, _ = strconv.ParseInt(res.Header.Get("Content-Length"), 10, 64)
	}
	return flob.NewInfo(d, size, time.Time{}, noLabels), nil
}

func (s *upstreamStore) Open(ctx context.Context, d flob.Digest) (io.ReadSeekCloser, flob.Info, error) {
	res, err := s.u.do(ctx, http.MethodGet, s.repo, s.path(d), nil)
	if err != nil {
		return nil, nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, nil, upstreamErr("blob GET", res)
	}
	r := &upstreamReader{ctx: ctx, s: s, path: s.path(d), body: res.Body, size: res.ContentLength}
	return r, flob.NewInfo(d, res.ContentLength, time.Time{}, noLabels), nil
}

// upstreamReader reads a blob from the upstream, and seeks by asking again
// from the new offset.
type upstreamReader struct {
	ctx  context.Context
	s    *upstreamStore
	path string
	body io.ReadCloser
	size int64
	pos  int64
}

func (r *upstreamReader) Read(p []byte) (int, error) {
	if r.size >= 0 && r.pos >= r.size {
		return 0, io.EOF
	}
	if r.body == nil {
		res, err := r.s.u.do(r.ctx, http.MethodGet, r.s.repo, r.path, http.Header{"Range": {"bytes=" + strconv.FormatInt(r.pos, 10) + "-"}})
		if err != nil {
			return 0, err
		}
		switch res.StatusCode {
		case http.StatusPartialContent:
		case http.StatusOK:
			// A range the upstream ignored: skip to where the reader is.
			if _, err := io.CopyN(io.Discard, res.Body, r.pos); err != nil {
				res.Body.Close()
				return 0, err
			}
		default:
			res.Body.Close()
			return 0, upstreamErr("blob GET", res)
		}
		r.body = res.Body
	}
	n, err := r.body.Read(p)
	r.pos += int64(n)
	r.s.u.bytes.Add(r.ctx, int64(n), metric.WithAttributes(r.s.u.host, attribute.String("cr.cache.operation", "blob get")))
	return n, err
}

func (r *upstreamReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		if r.size < 0 {
			return 0, errors.New("upstream: size unknown")
		}
		abs = r.size + offset
	default:
		return 0, errors.New("upstream: bad whence")
	}
	if abs < 0 {
		return 0, errors.New("upstream: negative position")
	}
	if abs != r.pos && r.body != nil {
		r.body.Close()
		r.body = nil
	}
	r.pos = abs
	return abs, nil
}

func (r *upstreamReader) Close() error {
	if r.body == nil {
		return nil
	}
	err := r.body.Close()
	r.body = nil
	return err
}
