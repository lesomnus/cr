package registry_test

import (
	"encoding/json"
	"net/http"
	"testing"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func TestSearch(t *testing.T) {
	x := newHarness(t)
	v1h := x.h.(interface{ V1() http.Handler }).V1()

	for _, name := range []string{"acme/app", "acme/web", "other/tool"} {
		config := x.pushBlob(name, []byte(`{"name":"`+name+`"}`))
		m := v1.Manifest{
			Versioned:   specs.Versioned{SchemaVersion: 2},
			MediaType:   v1.MediaTypeImageManifest,
			Config:      v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: config, Size: int64(len(`{"name":"` + name + `"}`))},
			Layers:      []v1.Descriptor{},
			Annotations: map[string]string{"org.opencontainers.image.description": "the " + name},
		}
		b, _ := json.Marshal(m)
		require.Equal(t, http.StatusCreated, x.pushManifest(name, "latest", b, v1.MediaTypeImageManifest).StatusCode)
	}

	v := &harness{t: t, h: v1h}
	res := v.do("GET", "/v1/_ping", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "true", res.Header.Get("X-Docker-Registry-Standalone"))

	res = v.do("GET", "/v1/search?q=acme&n=10", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	var out struct {
		Query      string `json:"query"`
		NumResults int    `json:"num_results"`
		Results    []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			StarCount   int    `json:"star_count"`
			IsOfficial  bool   `json:"is_official"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(read(t, res), &out))
	require.Equal(t, "acme", out.Query)
	require.Equal(t, 2, out.NumResults)
	require.Equal(t, "acme/app", out.Results[0].Name)
	require.Equal(t, "the acme/app", out.Results[0].Description)

	res = v.do("GET", "/v1/search?q=TOOL&n=1", nil)
	require.NoError(t, json.Unmarshal(read(t, res), &out))
	require.Equal(t, 1, out.NumResults)
	require.Equal(t, "other/tool", out.Results[0].Name)

	res = v.do("GET", "/v1/nothing", nil)
	require.Equal(t, http.StatusNotFound, res.StatusCode)
}
