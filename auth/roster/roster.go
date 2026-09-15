// Package roster is roster as cr's authenticator: `rt_` keys and passwords
// checked by roster, a holder's teams as groups, and roster's word that a
// holder changed dropping what cr remembered about them.
//
// It speaks Connect's JSON over HTTP to roster's data plane -- the listener
// its people and apps call, `server.http` in roster's configuration -- with
// the key `roster key add --service cr` made. That key is a row of roster's
// control plane, but the control plane's own listener holds none of the
// holders, keys and teams cr asks about. cr carries none of roster's generated
// code and needs none of its versions to agree with its own.
package roster

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/payday/frame"
	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/lesomnus/cr/auth"
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

// stream is one server-streaming procedure, each message handed to each until
// the stream ends.
func (c *Client) stream(ctx context.Context, procedure string, in any, each func(json.RawMessage) error) error {
	msg, err := marshal(in)
	if err != nil {
		return err
	}
	body := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(body[1:5], uint32(len(msg)))
	copy(body[5:], msg)

	req, err := c.request(ctx, procedure, "application/connect+json", body)
	if err != nil {
		return err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		var e connectError
		b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		if json.Unmarshal(b, &e) != nil || e.Code == "" {
			return fmt.Errorf("roster: %s answered %s", procedure, res.Status)
		}
		return e.err()
	}

	r := bufio.NewReader(res.Body)
	head := make([]byte, 5)
	for {
		if _, err := io.ReadFull(r, head); err != nil {
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		n := binary.BigEndian.Uint32(head[1:])
		if n > 4<<20 {
			return errors.New("roster: stream message too large")
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			return err
		}
		if head[0]&0x02 != 0 {
			var end struct {
				Error *connectError `json:"error"`
			}
			if json.Unmarshal(b, &end) == nil && end.Error != nil {
				return end.Error.err()
			}
			return nil
		}
		if err := each(b); err != nil {
			return err
		}
	}
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

func (c *Client) forgetNames() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.names)
}

// introspect is `payday.TokenService/Introspect` with the names filled in.
// roster's data plane answers with the holder's and its tenant's identifiers
// and leaves their aliases empty, and the aliases are what a binding names a
// holder by, and what the management API's mirror puts the rows up under.
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

// Authenticator authenticates against roster: an `rt_` key given as the
// password is introspected, anything else is a password for the holder the
// username names, `acme/alice` or `@acme/alice`.
//
// The subject is the holder's identifier, with `@tenant/alias` as an alias.
// Its groups are `@tenant` and one for each team it is in: `@tenant/site/team`
// for a team in a site, which is where roster names a team, and the team's
// identifier for a team in none, which roster names by identifier alone.
//
// A key is used for what it was made for: the actions whose methods its grant
// covers, `/cr.Registry/Pull` and the like, and a key that covers none of them
// is refused. A password is the whole of its holder.
//
// What roster accepted is remembered for a while, and forgotten sooner when
// roster's sync stream says the holder changed.
type Authenticator struct {
	c        *Client
	remember time.Duration
	now      func() time.Time

	mu       sync.Mutex
	decided  map[[32]byte]decision
	byHolder map[string]map[[32]byte]struct{}
}

type decision struct {
	s     auth.Subject
	until time.Time
}

func New(c *Client, remember time.Duration) *Authenticator {
	if remember <= 0 {
		remember = time.Minute
	}
	return &Authenticator{
		c:        c,
		remember: remember,
		now:      time.Now,
		decided:  map[[32]byte]decision{},
		byHolder: map[string]map[[32]byte]struct{}{},
	}
}

var _ auth.Authenticator = (*Authenticator)(nil)

// parseName reads `acme/alice` or `@acme/alice`.
func parseName(v string) (string, string, bool) {
	t, h, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(v), "@"), "/")
	if !ok || t == "" || h == "" || strings.Contains(h, "/") {
		return "", "", false
	}
	return t, h, true
}

type who struct {
	Tenant string `json:"tenant,omitempty"`
	Alias  string `json:"alias,omitempty"`
}

type verifyRequest struct {
	Who    who    `json:"who"`
	Kind   string `json:"kind"`
	Secret []byte `json:"secret"`
}

type verifyResponse struct {
	Ok           bool   `json:"ok"`
	Holder       []byte `json:"holder"`
	Tenant       []byte `json:"tenant"`
	Continuation string `json:"continuation"`
}

func (a *Authenticator) Authenticate(ctx context.Context, username, password string) (auth.Subject, error) {
	key := sha256.Sum256([]byte(username + "\x00" + password))
	if s, ok := a.recall(key); ok {
		return s, nil
	}

	var (
		holder              []byte
		tenant, holderAlias string
		only                []auth.Action
	)
	if strings.HasPrefix(password, "rt_") {
		res, err := a.c.introspect(ctx, password)
		if errors.Is(err, ErrNotFound) {
			return auth.Subject{}, auth.ErrUnauthenticated
		}
		if err != nil {
			return auth.Subject{}, err
		}
		if exp := res.GetExpires(); exp != nil && !exp.AsTime().After(a.now()) {
			return auth.Subject{}, auth.ErrUnauthenticated
		}
		only = usedFor(res.GetGrant())
		if only != nil && len(only) == 0 {
			return auth.Subject{}, fmt.Errorf("%w: the key allows none of cr's methods; make one with --allow '/cr.Registry/*'", auth.ErrUnauthenticated)
		}
		holder, tenant, holderAlias = res.GetId(), res.GetTenant(), res.GetAlias()
	} else {
		t, h, ok := parseName(username)
		if !ok {
			return auth.Subject{}, auth.ErrNotMine
		}
		var res verifyResponse
		err := a.c.call(ctx, "roster.VouchService/Verify", verifyRequest{Who: who{Tenant: t, Alias: h}, Kind: "password", Secret: []byte(password)}, &res)
		if errors.Is(err, ErrNotFound) {
			return auth.Subject{}, auth.ErrNotMine
		}
		if err != nil {
			return auth.Subject{}, err
		}
		if !res.Ok {
			if res.Continuation != "" {
				return auth.Subject{}, fmt.Errorf("%w: roster asks for a second factor; sign in with an rt_ key", auth.ErrUnauthenticated)
			}
			return auth.Subject{}, auth.ErrUnauthenticated
		}
		holder, tenant, holderAlias = res.Holder, t, h
	}

	id, err := pdid.From(holder)
	if err != nil {
		return auth.Subject{}, fmt.Errorf("roster: holder: %w", err)
	}
	s := auth.Subject{ID: id.String(), Claims: map[string]any{"tenant": tenant, "holder": holderAlias}, Only: only}
	if tenant != "" && holderAlias != "" {
		s.Aliases = []string{"@" + tenant + "/" + holderAlias}
	}
	if tenant != "" {
		s.Groups = append(s.Groups, "@"+tenant)
	}
	teams, err := a.teamsOf(ctx, holder, tenant)
	if err != nil {
		return auth.Subject{}, err
	}
	s.Groups = append(s.Groups, teams...)

	a.keep(key, s)
	return s, nil
}

// usedFor is what a key's grant lets it be used for here: every action whose
// method the grant covers, by payday's rule for method patterns. A grant that
// narrows no method narrows nothing, and a grant that is not there allows
// nothing, which is how payday reads one.
func usedFor(g *pdpb.Grant) []auth.Action {
	if g == nil {
		return []auth.Action{}
	}
	if g.GetAnyAction() {
		return nil
	}
	out := []auth.Action{}
	for _, a := range auth.Actions {
		if slices.ContainsFunc(g.GetActions(), func(held string) bool { return frame.Covers(held, a.Method()) }) {
			out = append(out, a)
		}
	}
	return out
}

// teamsOf is a group for each team holder is in.
func (a *Authenticator) teamsOf(ctx context.Context, holder []byte, tenant string) ([]string, error) {
	var out []string
	after := ""
	for {
		var res struct {
			Items []struct {
				Team ref `json:"team"`
			} `json:"items"`
			Next string `json:"next"`
		}
		req := map[string]any{"filters": []any{map[string]any{"holder": ref{Id: holder}}}, "size": 100}
		if after != "" {
			req["after"] = after
		}
		if err := a.c.call(ctx, "roster.TeamMembershipService/List", req, &res); err != nil {
			return nil, err
		}
		for _, item := range res.Items {
			g, err := a.teamGroup(ctx, item.Team.Id, tenant)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out = append(out, g)
		}
		if res.Next == "" || len(res.Items) == 0 {
			return out, nil
		}
		after = res.Next
	}
}

// teamGroup is the group a team is: `@tenant/site/team`, or the team's
// identifier when it is in no site. A team's alias is unique within its site
// and not within its tenant, so `@tenant/team` would be two teams at once.
func (a *Authenticator) teamGroup(ctx context.Context, id []byte, tenant string) (string, error) {
	team, err := a.c.get(ctx, "roster.TeamService/Get", id)
	if err != nil {
		return "", err
	}
	if team.Site == nil || len(team.Site.Id) == 0 {
		v, err := pdid.From(id)
		if err != nil {
			return "", fmt.Errorf("%w: team: %s", ErrNotFound, err)
		}
		return v.String(), nil
	}
	site, err := a.c.get(ctx, "roster.SiteService/Get", team.Site.Id)
	if err != nil {
		return "", err
	}
	if tenant == "" || site.Alias == "" || team.Alias == "" {
		return "", fmt.Errorf("%w: a team with no name", ErrNotFound)
	}
	return "@" + tenant + "/" + site.Alias + "/" + team.Alias, nil
}

func (a *Authenticator) recall(key [32]byte) (auth.Subject, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d, ok := a.decided[key]
	if !ok || !a.now().Before(d.until) {
		return auth.Subject{}, false
	}
	return d.s, true
}

func (a *Authenticator) keep(key [32]byte, s auth.Subject) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.decided) > 10_000 {
		clear(a.decided)
		clear(a.byHolder)
	}
	a.decided[key] = decision{s: s, until: a.now().Add(a.remember)}
	if a.byHolder[s.ID] == nil {
		a.byHolder[s.ID] = map[[32]byte]struct{}{}
	}
	a.byHolder[s.ID][key] = struct{}{}
}

// Forget drops what was remembered about the holder with identifier id.
func (a *Authenticator) Forget(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key := range a.byHolder[id] {
		delete(a.decided, key)
	}
	delete(a.byHolder, id)
}

func (a *Authenticator) forgetAll() {
	a.mu.Lock()
	clear(a.decided)
	clear(a.byHolder)
	a.mu.Unlock()
	a.c.forgetNames()
}

// Spin follows roster's sync stream, forgetting a holder it names. A stream
// that ends may have missed something, so everything is forgotten before the
// next one starts.
func (a *Authenticator) Spin(ctx context.Context) error {
	backoff := time.Second
	for {
		err := a.c.stream(ctx, "roster.SyncService/Watch", struct{}{}, func(msg json.RawMessage) error {
			backoff = time.Second
			var ev struct {
				Holder []byte `json:"holder"`
			}
			if err := json.Unmarshal(msg, &ev); err != nil {
				return err
			}
			if id, err := pdid.From(ev.Holder); err == nil {
				a.Forget(id.String())
			}
			return nil
		})
		if ctx.Err() != nil {
			return nil
		}
		a.forgetAll()
		if err != nil {
			log.From(ctx).WarnContext(ctx, "roster: sync stream", slog.String("err", err.Error()))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// Kind is what the count of logins calls this authenticator.
func (a *Authenticator) Kind() string { return "roster" }
