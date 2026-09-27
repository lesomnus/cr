// Package roster is how the management API reads a token roster issued: the
// holder and the tenant it names, by alias as well as by identifier.
//
// It speaks Connect's JSON over HTTP to roster's data plane -- the listener
// its people and apps call, `server.http` in roster's configuration -- with
// the key `roster key add --service cr` made. That key is a row of roster's
// control plane, but the control plane's own listener holds none of the
// holders and tenants cr asks about. cr carries none of roster's generated
// code and needs none of its versions to agree with its own.
package roster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/lesomnus/payday/pdpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Client calls roster's data plane.
type Client struct {
	base string
	key  string
	http *http.Client
	now  func() time.Time

	mu    sync.Mutex
	names map[string]named
}

func NewClient(baseURL, key string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("roster: url %q: want an http or https URL", baseURL)
	}
	if key == "" {
		return nil, errors.New("roster: no key")
	}
	return &Client{
		base:  strings.TrimSuffix(baseURL, "/"),
		key:   key,
		http:  &http.Client{},
		now:   time.Now,
		names: map[string]named{},
	}, nil
}

var (
	// ErrNotFound is roster answering `not_found`, which is also its answer
	// to a token it will say nothing about.
	ErrNotFound = errors.New("roster: not found")

	errUnauthorized = errors.New("roster: refused cr's key")
)

type connectError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e connectError) err() error {
	switch e.Code {
	case "not_found":
		return fmt.Errorf("%w: %s", ErrNotFound, e.Message)
	case "unauthenticated", "permission_denied":
		return fmt.Errorf("%w: %s: %s", errUnauthorized, e.Code, e.Message)
	}
	return fmt.Errorf("roster: %s: %s", e.Code, e.Message)
}

func marshal(v any) ([]byte, error) {
	if m, ok := v.(proto.Message); ok {
		return protojson.Marshal(m)
	}
	return json.Marshal(v)
}

func unmarshal(b []byte, v any) error {
	if m, ok := v.(proto.Message); ok {
		return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, m)
	}
	return json.Unmarshal(b, v)
}

func (c *Client) request(ctx context.Context, procedure, contentType string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+procedure, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Authorization", "Bearer "+c.key)
	return req, nil
}

// call is one unary procedure, `package.Service/Method`.
func (c *Client) call(ctx context.Context, procedure string, in, out any) error {
	body, err := marshal(in)
	if err != nil {
		return err
	}
	req, err := c.request(ctx, procedure, "application/json", body)
	if err != nil {
		return err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		var e connectError
		if json.Unmarshal(b, &e) != nil || e.Code == "" {
			return fmt.Errorf("roster: %s answered %s", procedure, res.Status)
		}
		return e.err()
	}
	return unmarshal(b, out)
}

// NameFor is how long a name read by identifier is used without asking again,
// so an alias changed in roster is followed within it.
const NameFor = time.Minute

// row is what cr reads of a holder, a tenant, a team or a site: its alias, and
// the identifiers of the rows it is in.
type row struct {
	Alias  string `json:"alias"`
	Tenant *ref   `json:"tenant"`
	Site   *ref   `json:"site"`
}

type ref struct {
	Id []byte `json:"id"`
}

type named struct {
	row   row
	until time.Time
}

// get reads the row with identifier id through procedure,
// `roster.TenantService/Get` and the like. One roster does not have is
// [ErrNotFound].
func (c *Client) get(ctx context.Context, procedure string, id []byte) (row, error) {
	if len(id) == 0 {
		return row{}, fmt.Errorf("%w: %s: no identifier", ErrNotFound, procedure)
	}
	k := procedure + "\x00" + string(id)
	c.mu.Lock()
	n, ok := c.names[k]
	c.mu.Unlock()
	if ok && c.now().Before(n.until) {
		return n.row, nil
	}

	var v row
	if err := c.call(ctx, procedure, map[string]any{"ref": ref{Id: id}}, &v); err != nil {
		return row{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.names) > 10_000 {
		clear(c.names)
	}
	c.names[k] = named{row: v, until: c.now().Add(NameFor)}
	return v, nil
}

// introspect is `payday.TokenService/Introspect` with the names filled in.
// roster's data plane answers with the holder's and its tenant's identifiers
// and leaves their aliases empty, and the aliases are what the management
// API's mirror puts the rows up under.
func (c *Client) introspect(ctx context.Context, token string) (*pdpb.TokenIntrospectResponse, error) {
	res := &pdpb.TokenIntrospectResponse{}
	if err := c.call(ctx, "payday.TokenService/Introspect", pdpb.TokenIntrospectRequest_builder{Token: token}.Build(), res); err != nil {
		return nil, err
	}
	if res.GetAlias() != "" && res.GetTenant() != "" && len(res.GetTenantId()) > 0 {
		return res, nil
	}

	h, err := c.get(ctx, "roster.HolderService/Get", res.GetId())
	if err != nil {
		return nil, fmt.Errorf("holder: %w", err)
	}
	tenant := res.GetTenantId()
	if len(tenant) == 0 && h.Tenant != nil {
		tenant = h.Tenant.Id
	}
	t, err := c.get(ctx, "roster.TenantService/Get", tenant)
	if err != nil {
		return nil, fmt.Errorf("tenant: %w", err)
	}
	if h.Alias == "" || t.Alias == "" {
		return nil, fmt.Errorf("%w: a holder or a tenant with no alias", ErrNotFound)
	}
	res.SetAlias(h.Alias)
	res.SetTenant(t.Alias)
	res.SetTenantId(tenant)
	return res, nil
}

// TokenService is payday's TokenService over this client, for
// `auth.Remote`: how the management API reads a token roster issued. Its
// answers name the holder and the tenant by alias as well as by identifier.
func (c *Client) TokenService() pdpb.TokenServiceClient { return tokenService{c} }

type tokenService struct{ c *Client }

func (t tokenService) Introspect(ctx context.Context, in *pdpb.TokenIntrospectRequest, _ ...grpc.CallOption) (*pdpb.TokenIntrospectResponse, error) {
	res, err := t.c.introspect(ctx, in.GetToken())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, status.Error(codes.NotFound, "no such token")
		}
		return nil, err
	}
	return res, nil
}
