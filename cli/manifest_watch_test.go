package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lesomnus/cr/api"
	"github.com/lesomnus/cr/cmd"
	"github.com/lesomnus/payday/config"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestRegistryManifestCreationWatch(t *testing.T) {
	c := cmd.Config{
		Db:       config.DbConfig{Driver: "sqlite3", Dsn: fmt.Sprintf("file:%s/cr.db?_pragma=journal_mode(WAL)", t.TempDir())},
		Watch:    config.WatchConfig{Broker: config.BrokerMemory},
		Registry: cmd.RegistryConfig{Storage: cmd.StorageConfig{Driver: "os", Os: cmd.OsStorageConfig{Root: t.TempDir()}}},
	}
	s, err := cmd.Build(t.Context(), c)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	require.NoError(t, Migrate(t.Context(), s))
	require.NoError(t, Registry(t.Context(), &c, s))
	g := grpc.NewServer()
	api.RegisterManifestServiceServer(g, s.Ungated.Manifest())
	l := bufconn.Listen(1 << 20)
	go g.Serve(l)
	t.Cleanup(func() { g.Stop(); l.Close() })
	conn, err := grpc.NewClient("passthrough:///registry-watch",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return l.Dial() }))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	const kind = "application/vnd.dev.sigstore.bundle.v0.3+json"
	f := api.ManifestFilter_builder{}.Build()
	f.SetArtifactType(kind)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream, err := api.NewManifestServiceClient(conn).Watch(ctx, api.ManifestWatchRequest_builder{
		Filters: []*api.ManifestFilter{f}, CreatedOnly: true,
	}.Build())
	require.NoError(t, err)
	_, err = stream.Header()
	require.NoError(t, err)
	configBlob := []byte("{}")
	configDigest := digest.FromBytes(configBlob)
	manifest, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: kind,
		Config: v1.Descriptor{MediaType: v1.MediaTypeEmptyJSON, Digest: configDigest, Size: int64(len(configBlob))},
		Layers: []v1.Descriptor{},
	})
	require.NoError(t, err)
	push := func(repo string) {
		t.Helper()
		blob := httptest.NewRecorder()
		s.Routes["/v2/"].ServeHTTP(blob, httptest.NewRequest(http.MethodPost,
			"/v2/"+repo+"/blobs/uploads/?digest="+configDigest.String(), bytes.NewReader(configBlob)))
		require.Equal(t, http.StatusCreated, blob.Code, blob.Body.String())
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/v2/"+repo+"/manifests/"+digest.FromBytes(manifest).String(), bytes.NewReader(manifest))
		req.Header.Set("Content-Type", v1.MediaTypeImageManifest)
		s.Routes["/v2/"].ServeHTTP(w, req)
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	}
	push("stage")
	first, err := stream.Recv()
	require.NoError(t, err)
	require.Len(t, first.GetItems(), 1)
	require.Equal(t, "stage", first.GetItems()[0].GetValue().GetRepo())
	require.Equal(t, kind, first.GetItems()[0].GetValue().GetArtifactType())
	require.Equal(t, api.ManifestCreatedAction, first.GetItems()[0].GetAction())
	push("stage")
	push("prod")
	second, err := stream.Recv()
	require.NoError(t, err)
	require.Len(t, second.GetItems(), 1)
	require.Equal(t, "prod", second.GetItems()[0].GetValue().GetRepo())
	require.Equal(t, first.GetItems()[0].GetValue().GetDigest(), second.GetItems()[0].GetValue().GetDigest())
	require.NotEqual(t, first.GetItems()[0].GetId(), second.GetItems()[0].GetId())
}
