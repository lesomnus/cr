package registry_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"
	"time"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxtest"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/registry"
)

// TestGuardIsMeasured: logins are counted by who accepted them, refusals by
// action and by where, and the policy says how old it is.
func TestGuardIsMeasured(t *testing.T) {
	h := otxtest.New(t)
	ctx := h.Into(context.Background())
	meter := otx.From(ctx).Meter()

	st := auth.NewPolicyStore(time.Hour, auth.Static{Bindings: []auth.Binding{
		{Subject: "alice", Repo: "*", Actions: []auth.Action{auth.ActionAll}},
	}}).Measure(meter)
	require.NoError(t, st.Refresh(ctx))
	tokens, err := auth.NewTokens([]auth.StaticToken{{Name: "alice", Token: "alice-secret"}, {Name: "bob", Token: "bob-secret"}})
	require.NoError(t, err)
	k, err := auth.GenerateKey()
	require.NoError(t, err)
	issuer, err := auth.NewIssuer("cr", "registry.test", time.Minute, k)
	require.NoError(t, err)
	g := (&auth.Guard{Authenticator: auth.Chain{tokens}, Policy: st, Issuer: issuer}).Measure(meter)
	mux := http.NewServeMux()
	mux.Handle("/v2/", registry.New(registry.Config{Stores: flob.NewMemStores(), Index: memindex.New(), Guard: g, Meter: meter}))
	mux.HandleFunc("/token", g.ServeToken)
	x := &harness{t: t, h: mux}
	basic := func(user, pass string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	}

	// A login that checks, one that does not, one that checks and may do
	// nothing -- refused at the token endpoint -- and the same on a request.
	require.Equal(t, http.StatusOK, x.do("GET", "/token?scope=repository:acme/app:pull,push", nil, "Authorization", basic("alice", "alice-secret")).StatusCode)
	require.Equal(t, http.StatusUnauthorized, x.do("GET", "/token", nil, "Authorization", basic("bob", "wrong")).StatusCode)
	require.Equal(t, http.StatusOK, x.do("GET", "/token?scope=repository:acme/app:pull,push", nil, "Authorization", basic("bob", "bob-secret")).StatusCode)
	require.Equal(t, http.StatusForbidden, x.do("GET", "/v2/acme/app/tags/list", nil, "Authorization", basic("bob", "bob-secret")).StatusCode)

	logins := map[string]int64{}
	denied := map[string]int64{}
	age := int64(-1)
	for _, sm := range h.Collect(ctx).ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "cr.auth.logins":
				for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
					via, _ := dp.Attributes.Value("cr.auth.authenticator")
					outcome, _ := dp.Attributes.Value("cr.auth.outcome")
					logins[via.AsString()+" "+outcome.AsString()] += dp.Value
				}
			case "cr.auth.denied":
				for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
					action, _ := dp.Attributes.Value("cr.auth.action")
					at, _ := dp.Attributes.Value("cr.auth.at")
					denied[at.AsString()+" "+action.AsString()] += dp.Value
				}
			case "cr.auth.policy.age":
				for _, dp := range m.Data.(metricdata.Gauge[int64]).DataPoints {
					age = dp.Value
				}
			}
		}
	}
	require.Equal(t, map[string]int64{"static ok": 3, "none refused": 1}, logins)
	require.Equal(t, map[string]int64{"token pull": 1, "token push": 1, "token tag": 1, "request pull": 1}, denied)
	require.GreaterOrEqual(t, age, int64(0), "the policy was loaded a moment ago")
}
