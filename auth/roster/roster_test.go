package roster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fake is roster's data plane, as much of it as cr calls, answering the way
// the real one does: an introspection names the holder and its tenant by
// identifier alone.
type fake struct {
	srv    *httptest.Server
	holder []byte
	tenant []byte
	calls  atomic.Int32
}

func newFake(t *testing.T) *fake {
	f := &fake{
		holder: pdid.New(2).Bytes(),
		tenant: pdid.New(1).Bytes(),
	}
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
	names := func(body map[string]any, id []byte) bool {
		ref, _ := body["ref"].(map[string]any)
		return ref != nil && ref["id"] == b64(id)
	}
	notFound := map[string]string{"code": "not_found", "message": "not found"}

	grants := map[string]map[string]any{
		"rt_good": {"anyTenant": true, "anySet": true, "actions": []string{"/app.TagRuleService/*"}},
	}
	unary("payday.TokenService/Introspect", func(body map[string]any) (int, any) {
		g, ok := grants[body["token"].(string)]
		if !ok {
			return http.StatusNotFound, map[string]string{"code": "not_found", "message": "no such token"}
		}
		return http.StatusOK, map[string]any{"id": b64(f.holder), "tenantId": b64(f.tenant), "tenant": "", "alias": "", "grant": g}
	})
	unary("roster.HolderService/Get", func(body map[string]any) (int, any) {
		if !names(body, f.holder) {
			return http.StatusNotFound, notFound
		}
		return http.StatusOK, map[string]any{"id": b64(f.holder), "tenant": map[string]any{"id": b64(f.tenant), "alias": ""}, "alias": "ci"}
	})
	unary("roster.TenantService/Get", func(body map[string]any) (int, any) {
		if !names(body, f.tenant) {
			return http.StatusNotFound, notFound
		}
		return http.StatusOK, map[string]any{"id": b64(f.tenant), "alias": "acme"}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestTokenService(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	c, err := NewClient(f.srv.URL, "rk_test")
	require.NoError(t, err)

	// roster answers with identifiers; the management API's mirror puts the
	// rows up under the names.
	res, err := c.TokenService().Introspect(ctx, pdpb.TokenIntrospectRequest_builder{Token: "rt_good"}.Build())
	require.NoError(t, err)
	require.Equal(t, f.holder, res.GetId())
	require.Equal(t, f.tenant, res.GetTenantId())
	require.Equal(t, "acme", res.GetTenant())
	require.Equal(t, "ci", res.GetAlias())

	_, err = c.TokenService().Introspect(ctx, pdpb.TokenIntrospectRequest_builder{Token: "rt_nope"}.Build())
	require.Equal(t, codes.NotFound, status.Code(err))
}
