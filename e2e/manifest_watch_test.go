package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/lesomnus/cr/api"
	"github.com/lesomnus/cr/cli"
	"github.com/lesomnus/cr/cmd"
	"github.com/lesomnus/cr/server/pd"
	"github.com/lesomnus/payday/config"
	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/spin"
	"github.com/lesomnus/payday/watch"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestPostgresManifestCreationWatch(t *testing.T) {
	dsn := os.Getenv("CR_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("CR_TEST_POSTGRES is not set")
	}
	for _, outbox := range []bool{false, true} {
		name := "broker"
		if outbox {
			name = "outbox"
		}
		t.Run(name, func(t *testing.T) {
			c := cmd.Config{
				Db:    config.DbConfig{Driver: "pgx", Dsn: schema(t, dsn)},
				Watch: config.WatchConfig{Broker: "postgres", Outbox: outbox, DrainEvery: time.Millisecond},
				Registry: cmd.RegistryConfig{
					Storage: cmd.StorageConfig{Driver: "os", Os: cmd.OsStorageConfig{Root: t.TempDir()}},
					Gc:      cmd.GcConfig{Every: -time.Second},
				},
			}
			build := func() *cmd.Server {
				t.Helper()
				s, err := cmd.Build(t.Context(), c)
				require.NoError(t, err)
				t.Cleanup(func() { s.Close() })
				require.NoError(t, cli.Migrate(t.Context(), s))
				require.NoError(t, cli.Registry(t.Context(), &c, s))
				return s
			}
			a, b := build(), build()
			// The broker listener connects asynchronously. Confirm a notification
			// crosses replicas before beginning the API's creation subscription.
			events, stop := b.Watch.Subscribe()
			defer stop()
			require.Eventually(t, func() bool {
				a.Watch.Broker().Publish(t.Context(), watch.Event{Method: "ready", Changes: []watch.Change{{
					By: "/test/Ready", Key: pdid.New(pd.ManifestDomain),
				}}})
				select {
				case event := <-events:
					return event.Method == "ready"
				default:
					return false
				}
			}, 5*time.Second, 10*time.Millisecond)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { defer close(done); spin.Run(ctx, slices.Values(a.Spin)) }()
			t.Cleanup(func() { cancel(); <-done })
			g := grpc.NewServer()
			api.RegisterManifestServiceServer(g, b.Ungated.Manifest())
			l := bufconn.Listen(1 << 20)
			go g.Serve(l)
			t.Cleanup(func() { g.Stop(); l.Close() })
			conn, err := grpc.NewClient("passthrough:///replica-b",
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return l.Dial() }))
			require.NoError(t, err)
			t.Cleanup(func() { conn.Close() })
			const kind = "application/vnd.dev.sigstore.bundle.v0.3+json"
			f := api.ManifestFilter_builder{}.Build()
			f.SetArtifactType(kind)
			streamCtx, streamCancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer streamCancel()
			client := api.NewManifestServiceClient(conn)
			stream, err := client.Watch(streamCtx, api.ManifestWatchRequest_builder{Filters: []*api.ManifestFilter{f}, CreatedOnly: true}.Build())
			require.NoError(t, err)
			_, err = stream.Header()
			require.NoError(t, err)
			push := func(repo, artifactType string) digest.Digest {
				t.Helper()
				body, err := json.Marshal(v1.Manifest{
					Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: artifactType,
					Config: v1.Descriptor{MediaType: v1.MediaTypeEmptyJSON, Digest: digest.FromString("{}"), Size: 2}, Layers: []v1.Descriptor{},
				})
				require.NoError(t, err)
				d := digest.FromBytes(body)
				req := httptest.NewRequest(http.MethodPut, "/v2/"+repo+"/manifests/"+d.String(), bytes.NewReader(body))
				req.Header.Set("Content-Type", v1.MediaTypeImageManifest)
				res := httptest.NewRecorder()
				a.Routes["/v2/"].ServeHTTP(res, req)
				require.Equal(t, http.StatusCreated, res.Code, res.Body.String())
				return d
			}
			d := push("stage", kind)
			first, err := stream.Recv()
			require.NoError(t, err)
			require.Len(t, first.GetItems(), 1)
			require.Equal(t, "stage", first.GetItems()[0].GetValue().GetRepo())
			require.Equal(t, api.ManifestCreatedAction, first.GetItems()[0].GetAction())
			push("stage", kind)
			push("stage", "application/sbom")
			push("prod", kind)
			second, err := stream.Recv()
			require.NoError(t, err)
			require.Len(t, second.GetItems(), 1)
			require.Equal(t, "prod", second.GetItems()[0].GetValue().GetRepo())
			require.Equal(t, d.String(), second.GetItems()[0].GetValue().GetDigest())
			require.NotEqual(t, first.GetItems()[0].GetId(), second.GetItems()[0].GetId())
			res, err := client.List(t.Context(), api.ManifestListRequest_builder{Filters: []*api.ManifestFilter{f}}.Build())
			require.NoError(t, err)
			require.Len(t, res.GetItems(), 2)
		})
	}
}
