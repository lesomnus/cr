// Package roster is roster as cr's authenticator: `rt_` keys and passwords
// checked by roster, a holder's teams as groups, and roster's word that a
// holder changed dropping what cr remembered about them.
//
// It speaks Connect's JSON over HTTP to roster's control plane with cr's own
// `rk_` key, so cr carries none of roster's generated code and needs none of
// its versions to agree with its own.
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
	"strings"
	"sync"
	"time"

	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/lesomnus/cr/auth"
)

// Client calls roster's control plane.
type Client struct {
	base string
	key  string
	http *http.Client
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
		base: strings.TrimSuffix(baseURL, "/"),
		key:  key,
		http: &http.Client{},
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

// TokenService is payday's TokenService over this client, for
// `auth.Remote`: how the management API reads a token roster issued.
func (c *Client) TokenService() pdpb.TokenServiceClient { return tokenService{c} }

type tokenService struct{ c *Client }

func (t tokenService) Introspect(ctx context.Context, in *pdpb.TokenIntrospectRequest, _ ...grpc.CallOption) (*pdpb.TokenIntrospectResponse, error) {
	out := &pdpb.TokenIntrospectResponse{}
	if err := t.c.call(ctx, "payday.TokenService/Introspect", in, out); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, status.Error(codes.NotFound, "no such token")
		}
		return nil, err
	}
	return out, nil
}

// Authenticator authenticates against roster: an `rt_` key given as the
// password is introspected, anything else is a password for the holder the
// username names, `acme/alice` or `@acme/alice`.
//
// The subject is the holder's identifier, with `@tenant/alias` as an alias;
// its groups are `@tenant` and `@tenant/team` for each team it is in. What
// roster accepted is remembered for a while, and forgotten sooner when
// roster's sync stream says the holder changed.
type Authenticator struct {
	c        *Client
	remember time.Duration
	now      func() time.Time

	mu       sync.Mutex
	decided  map[[32]byte]decision
	byHolder map[string]map[[32]byte]struct{}
	teams    map[string]string
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
		teams:    map[string]string{},
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
	)
	if strings.HasPrefix(password, "rt_") {
		res := &pdpb.TokenIntrospectResponse{}
		err := a.c.call(ctx, "payday.TokenService/Introspect", pdpb.TokenIntrospectRequest_builder{Token: password}.Build(), res)
		if errors.Is(err, ErrNotFound) {
			return auth.Subject{}, auth.ErrUnauthenticated
		}
		if err != nil {
			return auth.Subject{}, err
		}
		if exp := res.GetExpires(); exp != nil && !exp.AsTime().After(a.now()) {
			return auth.Subject{}, auth.ErrUnauthenticated
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
	s := auth.Subject{ID: id.String(), Claims: map[string]any{"tenant": tenant, "holder": holderAlias}}
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

type ref struct {
	Id []byte `json:"id"`
}

// teamsOf is `@tenant/team` for each team holder is in.
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
			alias, err := a.teamAlias(ctx, item.Team.Id)
			if err != nil {
				return nil, err
			}
			if alias != "" && tenant != "" {
				out = append(out, "@"+tenant+"/"+alias)
			}
		}
		if res.Next == "" || len(res.Items) == 0 {
			return out, nil
		}
		after = res.Next
	}
}

// teamAlias is a team's alias, remembered for the life of the process: an
// alias that changes is a group that stops matching, which is the safe way to
// be wrong.
func (a *Authenticator) teamAlias(ctx context.Context, id []byte) (string, error) {
	k := string(id)
	a.mu.Lock()
	alias, ok := a.teams[k]
	a.mu.Unlock()
	if ok {
		return alias, nil
	}
	var res struct {
		Alias string `json:"alias"`
	}
	err := a.c.call(ctx, "roster.TeamService/Get", map[string]any{"ref": ref{Id: id}}, &res)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.teams[k] = res.Alias
	a.mu.Unlock()
	return res.Alias, nil
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
	defer a.mu.Unlock()
	clear(a.decided)
	clear(a.byHolder)
	clear(a.teams)
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
