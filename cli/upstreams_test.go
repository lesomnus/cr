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

	"github.com/lesomnus/cr/blob"
	"github.com/lesomnus/cr/cmd"
)

func readProxyConfig(t *testing.T, input string) (cmd.RegistryConfig, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cr.yaml")
	require.NoError(t, os.WriteFile(path, []byte("registry:\n  proxies:\n    - prefix: dist\n      remote: dist\n      "+input+"\n"), 0o600))
	var config cmd.Config
	snapshot, err := cfg.New(cmd.Name, &config, cfg.WithPaths()).Read(path, nil)
	if err != nil {
		return cmd.RegistryConfig{}, err
	}
	return snapshot.Config.Registry, nil
}

func replaceProxyToken(t *testing.T, path, value string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path+".next", []byte(value), 0o600))
	require.NoError(t, os.Rename(path+".next", path))
}

func TestOrderedProxyConfigurationAndIndependentRotatingBearers(t *testing.T) {
	var primaryCalls, fallbackCalls atomic.Int32
	primaryToken := filepath.Join(t.TempDir(), "primary-token")
	fallbackToken := filepath.Join(t.TempDir(), "fallback-token")
	var primaryExpected, fallbackExpected atomic.Value
	body := []byte("manifest")
	d := digest.FromBytes(body)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		require.Equal(t, primaryExpected.Load(), r.Header.Get("Authorization"))
		w.WriteHeader(503)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		require.Equal(t, fallbackExpected.Load(), r.Header.Get("Authorization"))
		w.Write(body)
	}))
	defer fallback.Close()
	config, err := readProxyConfig(t, fmt.Sprintf(`upstreams:
        - url: %q
          auth: {kind: bearer, token: "${file:%s}"}
        - url: %q
          auth: {kind: bearer, token: "${file:%s}"}`, primary.URL, primaryToken, fallback.URL, fallbackToken))
	require.NoError(t, err)
	_, proxies, _, err := Proxies(config, flob.NewMemStores(), nil)
	require.NoError(t, err)
	require.Len(t, proxies, 1)
	require.Equal(t, primary.URL, proxies[0].Upstream.String())
	for _, tokens := range [][2]string{{"hq-first", "cloud-first"}, {"hq-rotated", "cloud-first"}, {"hq-rotated", "cloud-rotated"}} {
		primaryExpected.Store("Bearer " + tokens[0])
		fallbackExpected.Store("Bearer " + tokens[1])
		replaceProxyToken(t, primaryToken, tokens[0])
		replaceProxyToken(t, fallbackToken, tokens[1])
		got, _, gotDigest, err := proxies[0].Upstream.GetManifest(t.Context(), "dist/prod/demo", d.String(), 4096)
		require.NoError(t, err)
		require.Equal(t, body, got)
		require.Equal(t, d, gotDigest)
	}
	require.EqualValues(t, 3, primaryCalls.Load())
	require.EqualValues(t, 3, fallbackCalls.Load())
}

func TestOrderedProxySeparateAuthKindsAndAnonymousTarget(t *testing.T) {
	body := []byte("fallback manifest")
	d := digest.FromBytes(body)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer hq-only", r.Header.Get("Authorization"))
		w.WriteHeader(503)
	}))
	defer primary.Close()
	for _, kind := range []string{"anonymous", "password"} {
		t.Run(kind, func(t *testing.T) {
			var requests atomic.Int32
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if kind == "anonymous" {
					require.Empty(t, r.Header.Get("Authorization"), "a target never inherits another target's credential")
				} else {
					if r.Header.Get("Authorization") == "" {
						w.Header().Set("WWW-Authenticate", `Basic realm="cloud"`)
						w.WriteHeader(401)
						return
					}
					user, password, ok := r.BasicAuth()
					require.True(t, ok, "the primary bearer must not reach this origin")
					require.Equal(t, "cloud-reader", user)
					require.Equal(t, "cloud-only", password)
				}
				w.Write(body)
			}))
			defer fallback.Close()
			auth := ""
			if kind == "password" {
				auth = "\n          auth: {kind: password, username: cloud-reader, password: cloud-only}"
			}
			config, err := readProxyConfig(t, fmt.Sprintf(`upstreams:
        - url: %q
          auth: {kind: bearer, token: hq-only}
        - url: %q%s`, primary.URL, fallback.URL, auth))
			require.NoError(t, err)
			_, proxies, _, err := Proxies(config, flob.NewMemStores(), nil)
			require.NoError(t, err)
			for range 2 {
				got, _, _, err := proxies[0].Upstream.GetManifest(t.Context(), "dist/demo", d.String(), 4096)
				require.NoError(t, err)
				require.Equal(t, body, got)
			}
			want := int32(2)
			if kind == "password" {
				want = 4 // Each origin answers its own Basic challenge.
			}
			require.Equal(t, want, requests.Load())
		})
	}
}

func TestOrderedProxySharedTokenFileIsExplicit(t *testing.T) {
	token := filepath.Join(t.TempDir(), "shared-token")
	replaceProxyToken(t, token, "shared-session")
	body := []byte("shared-session manifest")
	d := digest.FromBytes(body)
	origin := func(status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "Bearer shared-session", r.Header.Get("Authorization"))
			w.WriteHeader(status)
			if status == 200 {
				w.Write(body)
			}
		}))
	}
	primary, fallback := origin(503), origin(200)
	defer primary.Close()
	defer fallback.Close()
	config, err := readProxyConfig(t, fmt.Sprintf(`upstreams:
        - url: %q
          auth: {kind: bearer, token: "${file:%s}"}
        - url: %q
          auth: {kind: bearer, token: "${file:%s}"}`, primary.URL, token, fallback.URL, token))
	require.NoError(t, err)
	_, proxies, _, err := Proxies(config, flob.NewMemStores(), nil)
	require.NoError(t, err)
	_, _, _, err = proxies[0].Upstream.GetManifest(t.Context(), "dist/demo", d.String(), 4096)
	require.NoError(t, err)
}

func TestOrderedProxyUnreadableTargetCredentialStopsFallback(t *testing.T) {
	var laterCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer primary-only", r.Header.Get("Authorization"))
		w.WriteHeader(503)
	}))
	defer primary.Close()
	later := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { laterCalls.Add(1) }))
	defer later.Close()
	missing := filepath.Join(t.TempDir(), "missing-token")
	config, err := readProxyConfig(t, fmt.Sprintf(`upstreams:
        - url: %q
          auth: {kind: bearer, token: primary-only}
        - url: %q
          auth: {kind: bearer, token: "${file:%s}"}
        - url: %q`, primary.URL, later.URL, missing, later.URL))
	require.NoError(t, err)
	_, proxies, _, err := Proxies(config, flob.NewMemStores(), nil)
	require.NoError(t, err)
	_, _, _, err = proxies[0].Upstream.GetManifest(t.Context(), "dist/demo", digest.FromString("manifest").String(), 4096)
	require.ErrorIs(t, err, blob.ErrUpstreamUnauthorized)
	require.Zero(t, laterCalls.Load(), "an unreadable credential stops before sending to that or later targets")
}

func TestProxyOriginSelectionValidation(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"upstreams: []", "upstreams: at least one"},
		{"upstreams: [{url: https://one.example}]\n      upstream: https://legacy.example", "upstream and upstreams"},
		{"upstreams: [{url: not-a-url}]", "upstreams[0].url:"},
		{"upstreams: [{auth: {kind: bearer, token: x}}]", "upstreams[0].url:"},
		{"upstreams: [{url: https://one.example}]\n      auth: {kind: bearer, token: shared}", "auth: with upstreams"},
		{"upstreams: [{url: https://one.example}]\n      auth: {username: stray}", "auth: with upstreams"},
		{"upstreams: [{url: https://one.example}]\n      token_file: /run/token", "token_file: moved to auth"},
		{"upstreams: [{url: https://one.example}, {url: https://two.example, auth: {kind: bearer}}]", "upstreams[1].auth.token: not set"},
		{"upstreams: [{url: https://one.example, auth: {kind: basic}}]", "upstreams[0].auth.kind:"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			config, err := readProxyConfig(t, tc.input)
			require.NoError(t, err)
			_, _, _, err = Proxies(config, flob.NewMemStores(), nil)
			require.ErrorContains(t, err, "registry.proxies[0]."+tc.want)
		})
	}
	for _, input := range []string{
		"upstreams: [https://one.example]",
		"upstreams: [{url: https://one.example, auth: {kind: bearer, tokne: wrong}}]",
	} {
		_, err := readProxyConfig(t, input)
		require.Error(t, err, "the strict loader must reject malformed entries and unknown auth fields")
	}
}

func TestLegacySingleProxyCredentialStillWorks(t *testing.T) {
	body := []byte("legacy manifest")
	d := digest.FromBytes(body)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer legacy-token", r.Header.Get("Authorization"))
		w.Write(body)
	}))
	defer origin.Close()
	config, err := readProxyConfig(t, fmt.Sprintf("upstream: %q\n      auth: {kind: bearer, token: legacy-token}", origin.URL))
	require.NoError(t, err)
	_, proxies, _, err := Proxies(config, flob.NewMemStores(), nil)
	require.NoError(t, err)
	_, _, _, err = proxies[0].Upstream.GetManifest(t.Context(), "dist/demo", d.String(), 4096)
	require.NoError(t, err)
}
