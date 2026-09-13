package registry_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/lesomnus/flob"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/gc"
	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/registry"
)

func TestAdminGc(t *testing.T) {
	stores := flob.NewMemStores()
	ix := memindex.New()
	col := gc.New(gc.Config{Stores: stores, Index: ix, Runs: &gc.MemRuns{}})
	reg := registry.New(registry.Config{Stores: stores, Index: ix, Collector: col})

	x := &harness{t: t, h: reg}
	stray := x.pushBlob("acme/app", []byte("stray"))
	admin := &harness{t: t, h: reg.Admin()}

	res := admin.do("POST", "/admin/gc", nil)
	require.Equal(t, http.StatusAccepted, res.StatusCode)
	var run gc.Run
	require.NoError(t, json.Unmarshal(read(t, res), &run))
	require.Equal(t, gc.KindFull, run.Kind)
	require.Equal(t, gc.TriggerAdmin, run.Trigger)
	require.Equal(t, "/admin/gc/"+run.ID, res.Header.Get("Location"))

	require.Eventually(t, func() bool {
		res := admin.do("GET", "/admin/gc/"+run.ID, nil)
		if res.StatusCode != http.StatusOK {
			return false
		}
		var v gc.Run
		json.Unmarshal(read(t, res), &v)
		return v.State == gc.StateDone && v.Blobs == 1
	}, 5*time.Second, 10*time.Millisecond)

	res = x.do("HEAD", "/v2/acme/app/blobs/"+stray.String(), nil)
	require.Equal(t, http.StatusNotFound, res.StatusCode)

	res = admin.do("GET", "/admin/gc", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	var list struct {
		Runs []gc.Run `json:"runs"`
	}
	require.NoError(t, json.Unmarshal(read(t, res), &list))
	require.Len(t, list.Runs, 1)

	res = admin.do("GET", "/admin/gc/nope", nil)
	require.Equal(t, http.StatusNotFound, res.StatusCode)

	// A registry with no collector has no such endpoint.
	none := &harness{t: t, h: registry.New(registry.Config{Stores: stores, Index: ix}).Admin()}
	require.Equal(t, http.StatusNotFound, none.do("POST", "/admin/gc", nil).StatusCode)
}

func TestAdminGcNeedsAdmin(t *testing.T) {
	g := newGuarded(t)
	stores := flob.NewMemStores()
	ix := memindex.New()
	col := gc.New(gc.Config{Stores: stores, Index: ix, Runs: &gc.MemRuns{}})
	admin := &harness{t: t, h: registry.New(registry.Config{Stores: stores, Index: ix, Guard: g.guard, Collector: col}).Admin()}

	require.Equal(t, http.StatusUnauthorized, admin.do("POST", "/admin/gc", nil).StatusCode)
	require.Equal(t, http.StatusForbidden, admin.do("POST", "/admin/gc", nil, "Authorization", basic("bob")).StatusCode)
	require.Equal(t, http.StatusForbidden, admin.do("GET", "/admin/gc", nil, "Authorization", basic("carol")).StatusCode)

	res := admin.do("POST", "/admin/gc", nil, "Authorization", basic("alice"))
	require.Equal(t, http.StatusAccepted, res.StatusCode)
}
