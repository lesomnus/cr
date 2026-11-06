package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/xli/cfg"
	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/cmd"
)

func TestOrderedProxyConfigurationAndRotatingBearer(t *testing.T) {
	var primaryCalls, fallbackCalls atomic.Int32
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("first"), 0o600))
	var expected atomic.Value
	expected.Store("Bearer first")
	body := []byte("manifest")
	d := digest.FromBytes(body)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		require.Equal(t, expected.Load(), r.Header.Get("Authorization"))
		w.WriteHeader(503)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		require.Equal(t, expected.Load(), r.Header.Get("Authorization"))
		w.Header().Set("Docker-Content-Digest", d.String())
		if r.Method != http.MethodHead {
			w.Write(body)
		}
	}))
	defer fallback.Close()
	configFile := filepath.Join(t.TempDir(), "cr.yaml")
	input := fmt.Sprintf(`registry:
  proxies:
    - prefix: dist
      remote: dist
      upstreams: [%q, %q]
      auth: {kind: bearer, token: "${file:%s}"}
`, primary.URL, fallback.URL, token)
	require.NoError(t, os.WriteFile(configFile, []byte(input), 0o600))
	var config cmd.Config
	snapshot, err := cfg.New(cmd.Name, &config, cfg.WithPaths()).Read(configFile, nil)
	require.NoError(t, err)
	_, proxies, _, err := Proxies(snapshot.Config.Registry, flob.NewMemStores(), nil)
	require.NoError(t, err)
	require.Len(t, proxies, 1)
	require.Equal(t, primary.URL, proxies[0].Upstream.String())
	for _, value := range []string{"first", "other"} {
		expected.Store("Bearer " + value)
		next := token + ".next"
		require.NoError(t, os.WriteFile(next, []byte(value), 0o600))
		require.NoError(t, os.Rename(next, token))
		got, _, gotDigest, err := proxies[0].Upstream.GetManifest(t.Context(), "dist/prod/demo", d.String(), 4096)
		require.NoError(t, err)
		require.Equal(t, body, got)
		require.Equal(t, d, gotDigest)
	}
	require.EqualValues(t, 2, primaryCalls.Load())
	require.EqualValues(t, 2, fallbackCalls.Load())
}

func TestProxyOriginSelectionValidation(t *testing.T) {
	for _, input := range []string{
		"upstreams: []",
		"upstreams: [https://one.example]\n      upstream: https://legacy.example",
		"upstreams: [not-a-url]",
	} {
		t.Run(input, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cr.yaml")
			require.NoError(t, os.WriteFile(path, []byte("registry:\n  proxies:\n    - prefix: dist\n      "+input+"\n"), 0o600))
			var c cmd.Config
			snapshot, err := cfg.New(cmd.Name, &c, cfg.WithPaths()).Read(path, nil)
			require.NoError(t, err)
			_, _, _, err = Proxies(snapshot.Config.Registry, flob.NewMemStores(), nil)
			require.ErrorContains(t, err, "upstream")
		})
	}
	_, ps, _, err := Proxies(cmd.RegistryConfig{Proxies: []cmd.ProxyConfig{{Prefix: "dist", Upstream: "https://legacy.example"}}}, flob.NewMemStores(), nil)
	require.NoError(t, err)
	require.Equal(t, "https://legacy.example", ps[0].Upstream.String())
}
