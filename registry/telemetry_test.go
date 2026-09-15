package registry_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxtest"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/registry"
)

// TestErrorsAreCounted: an error envelope is counted under its code, which
// is what tells the 404s apart.
func TestErrorsAreCounted(t *testing.T) {
	h := otxtest.New(t)
	ctx := h.Into(context.Background())
	reg := registry.New(registry.Config{Stores: flob.NewMemStores(), Index: memindex.New(), Meter: otx.From(ctx).Meter()})
	x := &harness{t: t, h: reg}

	require.Equal(t, http.StatusNotFound, x.do("GET", "/v2/acme/none/tags/list", nil).StatusCode)
	require.Equal(t, http.StatusNotFound, x.do("GET", "/v2/acme/none/tags/list", nil).StatusCode)
	require.Equal(t, http.StatusBadRequest, x.do("GET", "/v2/Not_Valid/tags/list", nil).StatusCode)

	counts := map[string]int64{}
	for _, sm := range h.Collect(ctx).ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "cr.registry.errors" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			for _, dp := range sum.DataPoints {
				code, _ := dp.Attributes.Value("cr.error.code")
				route, _ := dp.Attributes.Value("http.route")
				counts[code.AsString()+" "+route.AsString()] += dp.Value
			}
		}
	}
	require.Equal(t, map[string]int64{
		"NAME_UNKNOWN /v2/{name}/tags/list": 2,
		"NAME_INVALID /v2/{name}/tags/list": 1,
	}, counts)
}
