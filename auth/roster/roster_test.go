package roster

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lesomnus/cr/auth"
)

// fake is roster's control plane, as much of it as cr calls.
type fake struct {
	srv    *httptest.Server
	holder []byte
	tenant []byte
	team   []byte
	calls  atomic.Int32
	events chan []byte
}

func newFake(t *testing.T) *fake {
	f := &fake{holder: pdid.New(2).Bytes(), tenant: pdid.New(1).Bytes(), team: pdid.New(9).Bytes(), events: make(chan []byte, 4)}
	b64 := base64.StdEncoding.EncodeToString
	mux := http.NewServeMux()

	unary := func(procedure string, h func(body map[string]any) (int, any)) {
		mux.HandleFunc("/"+procedure, func(w http.ResponseWriter, r *http.Request) {
			f.calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if r.Header.Get("Authorization") != "Bearer rk_test" || r.Header.Get("Connect-Protocol-Version") != "1" {
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{"code": "unauthenticated", "message": "who is this"})
				return
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			code, v := h(body)
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(v)
		})
	}

	unary("payday.TokenService/Introspect", func(body map[string]any) (int, any) {
		if body["token"] != "rt_good" {
			return http.StatusNotFound, map[string]string{"code": "not_found", "message": "no such token"}
		}
		return http.StatusOK, map[string]any{"id": b64(f.holder), "tenantId": b64(f.tenant), "tenant": "acme", "alias": "ci"}
	})
	unary("roster.VouchService/Verify", func(body map[string]any) (int, any) {
		who, _ := body["who"].(map[string]any)
		secret, _ := base64.StdEncoding.DecodeString(body["secret"].(string))
		switch {
		case body["kind"] != "password":
			return http.StatusOK, map[string]any{}
		case who["tenant"] == "acme" && who["alias"] == "alice" && string(secret) == "wonderland":
			return http.StatusOK, map[string]any{"ok": true, "holder": b64(f.holder), "tenant": b64(f.tenant)}
		case who["alias"] == "bob":
			return http.StatusOK, map[string]any{"continuation": "c-1", "available": []any{map[string]any{"kind": "totp"}}}
		}
		return http.StatusOK, map[string]any{}
	})
	unary("roster.TeamMembershipService/List", func(body map[string]any) (int, any) {
		return http.StatusOK, map[string]any{"items": []any{map[string]any{"team": map[string]any{"id": b64(f.team)}}}}
	})
	unary("roster.TeamService/Get", func(body map[string]any) (int, any) {
		return http.StatusOK, map[string]any{"alias": "devs"}
	})
	mux.HandleFunc("/roster.SyncService/Watch", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/connect+json" {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		w.Header().Set("Content-Type", "application/connect+json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for {
			select {
			case h := <-f.events:
				b, _ := json.Marshal(map[string]any{"holder": b64(h), "reason": "SYNC_REASON_SUSPENDED"})
				head := []byte{0, 0, 0, 0, 0}
				binary.BigEndian.PutUint32(head[1:], uint32(len(b)))
				w.Write(head)
				w.Write(b)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestAuthenticate(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	c, err := NewClient(f.srv.URL, "rk_test")
	require.NoError(t, err)
	a := New(c, time.Hour)

	holder, err := pdid.From(f.holder)
	require.NoError(t, err)

	s, err := a.Authenticate(ctx, "whoever", "rt_good")
	require.NoError(t, err)
	require.Equal(t, holder.String(), s.ID)
	require.Equal(t, []string{"@acme/ci"}, s.Aliases)
	require.Equal(t, []string{"@acme", "@acme/devs"}, s.Groups)

	calls := f.calls.Load()
	_, err = a.Authenticate(ctx, "whoever", "rt_good")
	require.NoError(t, err)
	require.Equal(t, calls, f.calls.Load(), "what roster accepted is remembered")

	_, err = a.Authenticate(ctx, "whoever", "rt_revoked")
	require.ErrorIs(t, err, auth.ErrUnauthenticated)

	s, err = a.Authenticate(ctx, "@acme/alice", "wonderland")
	require.NoError(t, err)
	require.Equal(t, holder.String(), s.ID)
	require.Equal(t, []string{"@acme/alice"}, s.Aliases)

	_, err = a.Authenticate(ctx, "acme/alice", "looking-glass")
	require.ErrorIs(t, err, auth.ErrUnauthenticated)
	_, err = a.Authenticate(ctx, "acme/bob", "anything")
	require.ErrorIs(t, err, auth.ErrUnauthenticated)
	require.Contains(t, err.Error(), "second factor")

	// A name roster could not hold is somebody else's to judge.
	_, err = a.Authenticate(ctx, "alice", "wonderland")
	require.ErrorIs(t, err, auth.ErrNotMine)

	// A key roster does not know is not the caller's fault.
	bad, err := NewClient(f.srv.URL, "rk_wrong")
	require.NoError(t, err)
	_, err = New(bad, time.Hour).Authenticate(ctx, "whoever", "rt_good")
	require.Error(t, err)
	require.NotErrorIs(t, err, auth.ErrUnauthenticated)
}

func TestTokenService(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	c, err := NewClient(f.srv.URL, "rk_test")
	require.NoError(t, err)

	res, err := c.TokenService().Introspect(ctx, pdpb.TokenIntrospectRequest_builder{Token: "rt_good"}.Build())
	require.NoError(t, err)
	require.Equal(t, "acme", res.GetTenant())
	require.Equal(t, f.holder, res.GetId())

	_, err = c.TokenService().Introspect(ctx, pdpb.TokenIntrospectRequest_builder{Token: "rt_nope"}.Build())
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestSyncForgets(t *testing.T) {
	f := newFake(t)
	c, err := NewClient(f.srv.URL, "rk_test")
	require.NoError(t, err)
	a := New(c, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.Spin(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	_, err = a.Authenticate(ctx, "whoever", "rt_good")
	require.NoError(t, err)
	calls := f.calls.Load()

	f.events <- f.holder
	require.Eventually(t, func() bool {
		if _, err := a.Authenticate(ctx, "whoever", "rt_good"); err != nil {
			return false
		}
		return f.calls.Load() > calls
	}, 5*time.Second, 20*time.Millisecond)
}
