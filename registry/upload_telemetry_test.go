package registry_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxtest"
	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/registry"
)

// TestUploadsAreCounted: every way an upload ends is a count under its
// outcome.
func TestUploadsAreCounted(t *testing.T) {
	h := otxtest.New(t)
	ctx := h.Into(context.Background())
	reg := registry.New(registry.Config{Stores: flob.NewMemStores(), Index: memindex.New(), Meter: otx.From(ctx).Meter()})
	x := &harness{t: t, h: reg}

	one := x.pushBlob("acme/app", []byte("one"))
	x.pushBlob("acme/app", []byte("one"))

	res := x.do("POST", "/v2/acme/app/blobs/uploads/", nil)
	require.Equal(t, http.StatusAccepted, res.StatusCode)
	loc := res.Header.Get("Location")
	res = x.do("PATCH", loc, []byte("two"), "Content-Type", "application/octet-stream")
	require.Equal(t, http.StatusAccepted, res.StatusCode)
	sep := "?"
	if strings.Contains(loc, "?") {
		sep = "&"
	}
	res = x.do("PUT", loc+sep+"digest="+digest.FromString("two").String(), nil)
	require.Equal(t, http.StatusCreated, res.StatusCode)

	res = x.do("POST", "/v2/acme/app/blobs/uploads/", nil)
	require.Equal(t, http.StatusAccepted, res.StatusCode)
	res = x.do("DELETE", res.Header.Get("Location"), nil)
	require.Equal(t, http.StatusNoContent, res.StatusCode)

	res = x.do("POST", "/v2/acme/other/blobs/uploads/?mount="+one.String()+"&from=acme/app", nil)
	require.Equal(t, http.StatusCreated, res.StatusCode)

	counts := map[string]int64{}
	for _, sm := range h.Collect(ctx).ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "cr.uploads" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			for _, dp := range sum.DataPoints {
				outcome, _ := dp.Attributes.Value("cr.upload.outcome")
				counts[outcome.AsString()] += dp.Value
			}
		}
	}
	require.Equal(t, map[string]int64{"completed": 2, "exists": 1, "cancelled": 1, "mounted": 1}, counts)
}
