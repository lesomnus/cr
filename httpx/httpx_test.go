package httpx_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lesomnus/otx/otxtest"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/lesomnus/cr/httpx"
)

func TestInstrument(t *testing.T) {
	h := otxtest.New(t)
	ctx := h.Into(context.Background())

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/missing") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// A body copied in, as ServeContent does, still reaches the client.
		io.Copy(w, strings.NewReader("blob"))
	})
	route := func(r *http.Request) string { return "/v2/{name}/blobs/{digest}" }
	handler := httpx.Instrument(ctx, route, inner)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/v2/acme/app/blobs/sha256:abc", nil))
	require.Equal(t, "blob", w.Body.String())

	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("HEAD", "/v2/acme/app/blobs/missing", nil))
	require.Equal(t, http.StatusNotFound, w.Code)

	spans := h.Ended()
	require.Len(t, spans, 2)
	require.Equal(t, "GET /v2/{name}/blobs/{digest}", spans[0].Name())
	status := map[string]int64{}
	for _, s := range spans {
		for _, a := range s.Attributes() {
			if a.Key == "http.response.status_code" {
				status[s.Name()] = a.Value.AsInt64()
			}
		}
	}
	require.Equal(t, map[string]int64{"GET /v2/{name}/blobs/{digest}": 200, "HEAD /v2/{name}/blobs/{digest}": 404}, status)

	found := false
	for _, sm := range h.Collect(ctx).ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.request.duration" {
				continue
			}
			found = true
			hist, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok)
			require.Len(t, hist.DataPoints, 2)
		}
	}
	require.True(t, found)
}

func TestHealth(t *testing.T) {
	w := httptest.NewRecorder()
	httpx.Live().ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	require.Equal(t, http.StatusOK, w.Code)

	ok := func(context.Context) error { return nil }
	down := func(context.Context) error { return errors.New("database: connection refused") }

	w = httptest.NewRecorder()
	httpx.Ready(ok).ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	require.Equal(t, http.StatusOK, w.Code)

	w = httptest.NewRecorder()
	httpx.Ready(ok, down).ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, w.Body.String(), "connection refused")
}
