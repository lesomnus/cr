package manifestwatch_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/lesomnus/cr/api"
	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/index/entindex"
	"github.com/lesomnus/cr/internal/ent"
	"github.com/lesomnus/cr/internal/ent/manifest"
	entmigrate "github.com/lesomnus/cr/internal/ent/migrate"
	"github.com/lesomnus/cr/internal/ent/predicate"
	"github.com/lesomnus/cr/server/bare"
	"github.com/lesomnus/cr/server/manifestwatch"
	"github.com/lesomnus/cr/server/pd"
	"github.com/lesomnus/payday/config"
	_ "github.com/lesomnus/payday/config/dbsqlite3"
	"github.com/lesomnus/payday/watch"
	"github.com/opencontainers/go-digest"
	entsql "github.com/protobuf-orm/ent/dialect/sql"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const bundle = "application/vnd.dev.sigstore.bundle.v0.3+json"

type harness struct {
	rows   *ent.Client
	ix     *entindex.Index
	client api.ManifestServiceClient
	w      *watch.Watch
}

func newHarness(t *testing.T, scope bare.Scope, outbox bool, broker watch.Broker) *harness {
	t.Helper()
	ctx := t.Context()
	db, dialect, err := (config.DbConfig{
		Driver: "sqlite3", Dsn: fmt.Sprintf("file:%s/watch.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", t.TempDir()),
	}).Open(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	drv := entsql.OpenDB(dialect, db)
	rows := ent.NewClient(ent.Driver(drv))
	require.NoError(t, entmigrate.NewSchema(drv).Create(ctx))
	w := watch.New(broker)
	sink, err := pd.NewSink(rows, bare.WithScope(scope))
	require.NoError(t, err)
	s := manifestwatch.New(sink.WithWatch(w), w)
	g := grpc.NewServer()
	api.RegisterManifestServiceServer(g, s.Manifest())
	l := bufconn.Listen(1 << 20)
	go g.Serve(l)
	t.Cleanup(func() { g.Stop(); l.Close() })
	conn, err := grpc.NewClient("passthrough:///manifest-watch",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return l.Dial() }))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	ix := entindex.New(rows, entindex.WithManifestWatch(w, outbox), entindex.WithClock(func() time.Time {
		return time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	}))
	return &harness{rows: rows, ix: ix, client: api.NewManifestServiceClient(conn), w: w}
}

func record(name, artifactType string) index.Manifest {
	return index.Manifest{Digest: digest.FromString(name), MediaType: "application/vnd.oci.image.manifest.v1+json", ArtifactType: artifactType}
}

func (h *harness) put(t *testing.T, repo string, m index.Manifest) {
	t.Helper()
	require.NoError(t, h.ix.Manifest().Put(t.Context(), repo, m, nil))
}

func (h *harness) created(t *testing.T, filters ...*api.ManifestFilter) grpc.ServerStreamingClient[api.ManifestWatchResponse] {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := h.client.Watch(ctx, api.ManifestWatchRequest_builder{Filters: filters, CreatedOnly: true}.Build())
	require.NoError(t, err)
	_, err = stream.Header()
	require.NoError(t, err)
	return stream
}

func receive(t *testing.T, stream grpc.ServerStreamingClient[api.ManifestWatchResponse]) *api.ManifestWatchItem {
	t.Helper()
	res, err := stream.Recv()
	require.NoError(t, err)
	require.Len(t, res.GetItems(), 1)
	item := res.GetItems()[0]
	require.Equal(t, api.ManifestCreatedAction, item.GetAction())
	require.NotEmpty(t, item.GetId())
	require.Equal(t, item.GetId(), item.GetValue().GetId())
	return item
}

func TestListArtifactType(t *testing.T) {
	h := newHarness(t, nil, false, watch.Memory())
	for _, repo := range []string{"stage", "prod"} {
		h.put(t, repo, record("bundle", bundle))
		h.put(t, repo, record("image", "application/vnd.oci.image.config.v1+json"))
	}
	h.put(t, "prod", record("second-bundle", bundle))
	f := api.ManifestFilter_builder{}.Build()
	f.SetArtifactType(bundle)
	req := api.ManifestListRequest_builder{Filters: []*api.ManifestFilter{f}, Size: 1}.Build()
	seen := map[string]bool{}
	for {
		res, err := h.client.List(t.Context(), req)
		require.NoError(t, err)
		require.Len(t, res.GetItems(), 1)
		row := res.GetItems()[0]
		require.Equal(t, bundle, row.GetArtifactType())
		key := string(row.GetId())
		require.False(t, seen[key], "equal timestamps must still page without repeating a row")
		seen[key] = true
		if res.GetNext() == "" {
			break
		}
		req.SetAfter(res.GetNext())
	}
	require.Len(t, seen, 3)
	f.SetRepo("prod")
	res, err := h.client.List(t.Context(), api.ManifestListRequest_builder{Filters: []*api.ManifestFilter{f}}.Build())
	require.NoError(t, err)
	require.Len(t, res.GetItems(), 2)
}

func TestWatchCreatedOnly(t *testing.T) {
	h := newHarness(t, nil, false, watch.Memory())
	old := record("already-there", bundle)
	h.put(t, "stage", old)
	f := api.ManifestFilter_builder{}.Build()
	f.SetArtifactType(bundle)
	stream := h.created(t, f)

	// No snapshot, duplicate push, pull update, or deletion may notify.
	h.put(t, "stage", old)
	h.ix.Pulled().Touch("stage", "", old.Digest, time.Now())
	require.NoError(t, h.ix.Flush(t.Context()))
	_, err := h.ix.Manifest().Erase(t.Context(), "stage", old.Digest)
	require.NoError(t, err)
	h.put(t, "stage", record("different-kind", "application/sbom"))
	rolledBack := errors.New("rollback")
	require.ErrorIs(t, h.ix.Tx(t.Context(), "stage", func(tx index.Index) error {
		require.NoError(t, tx.Manifest().Put(t.Context(), "stage", record("rolled-back", bundle), nil))
		return rolledBack
	}), rolledBack)

	fresh := record("fresh-bundle", bundle)
	h.put(t, "stage", fresh)
	first := receive(t, stream)
	require.Equal(t, fresh.Digest.String(), first.GetValue().GetDigest())
	require.Equal(t, "stage", first.GetValue().GetRepo())
	// A second repository gets a new row even when the digest is identical.
	h.put(t, "prod", fresh)
	second := receive(t, stream)
	require.Equal(t, "prod", second.GetValue().GetRepo())
	require.NotEqual(t, first.GetId(), second.GetId())
}

type visible struct{ bare.Unscoped }

func (visible) ManifestScope(context.Context) (predicate.Manifest, error) {
	return manifest.RepoNEQ("hidden"), nil
}

type disconnected struct{}

func (disconnected) Publish(context.Context, watch.Event) {}

func (disconnected) Subscribe() (<-chan watch.Event, func()) {
	events := make(chan watch.Event)
	close(events)
	return events, func() {}
}

func TestWatchFiltersAndScope(t *testing.T) {
	h := newHarness(t, visible{}, false, watch.Memory())
	f := api.ManifestFilter_builder{}.Build()
	f.SetArtifactType(bundle)
	f.SetRepo("stage")
	subject := digest.FromString("subject")
	g := api.ManifestFilter_builder{}.Build()
	g.SetArtifactType("application/sbom")
	g.SetSubject(subject.String())
	stream := h.created(t, f, g)
	h.put(t, "prod", record("wrong-repo", bundle))
	h.put(t, "stage", record("wrong-subject", "application/sbom"))
	hidden := record("hidden", "application/sbom")
	hidden.Subject = subject
	h.put(t, "hidden", hidden)
	match := record("matching", bundle)
	h.put(t, "stage", match)
	require.Equal(t, match.Digest.String(), receive(t, stream).GetValue().GetDigest())
	other := record("other-filter", "application/sbom")
	other.Subject = subject
	h.put(t, "prod", other)
	require.Equal(t, other.Digest.String(), receive(t, stream).GetValue().GetDigest())
}

func TestWatchValidationAndRefCompatibility(t *testing.T) {
	h := newHarness(t, nil, false, watch.Memory())
	f := api.ManifestFilter_builder{}.Build()
	f.SetArtifactType(bundle)
	for _, filters := range [][]*api.ManifestFilter{
		nil, {nil}, {api.ManifestFilter_builder{}.Build()}, {f, api.ManifestFilter_builder{}.Build()},
		make([]*api.ManifestFilter, pd.ManifestFilterLimit+1),
	} {
		stream, err := h.client.Watch(t.Context(), api.ManifestWatchRequest_builder{Filters: filters, CreatedOnly: true}.Build())
		require.NoError(t, err)
		_, err = stream.Recv()
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
	row := record("ref-watch", bundle)
	h.put(t, "stage", row)
	u, err := h.rows.Manifest.Query().Only(t.Context())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream, err := h.client.Watch(ctx, api.ManifestWatchRequest_builder{
		Filters: []*api.ManifestFilter{api.ManifestFilter_builder{Ref: api.ManifestRef_builder{Id: u.Id[:]}.Build()}.Build()},
	}.Build())
	require.NoError(t, err)
	res, err := stream.Recv()
	require.NoError(t, err)
	require.Len(t, res.GetItems(), 1)
	require.Equal(t, row.Digest.String(), res.GetItems()[0].GetValue().GetDigest())
	require.Empty(t, res.GetItems()[0].GetAction())

	disabled := newHarness(t, nil, false, nil)
	stream, err = disabled.client.Watch(ctx, api.ManifestWatchRequest_builder{Filters: []*api.ManifestFilter{f}, CreatedOnly: true}.Build())
	require.NoError(t, err)
	_, err = stream.Recv()
	require.Equal(t, codes.Unimplemented, status.Code(err))

	cut := newHarness(t, nil, false, disconnected{})
	stream = cut.created(t, f)
	_, err = stream.Recv()
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.ErrorContains(t, err, "reconcile with List")
}

func TestManifestNotificationsCommitAndOutbox(t *testing.T) {
	for _, outbox := range []bool{false, true} {
		t.Run(fmt.Sprint(outbox), func(t *testing.T) {
			h := newHarness(t, nil, outbox, watch.Memory())
			events, stop := h.w.Subscribe()
			defer stop()
			rolledBack := errors.New("rollback")
			err := h.ix.Tx(t.Context(), "stage", func(tx index.Index) error {
				require.NoError(t, tx.Manifest().Put(t.Context(), "stage", record("rolled-back", bundle), nil))
				select {
				case <-events:
					t.Fatal("creation published before commit")
				default:
				}
				return rolledBack
			})
			require.ErrorIs(t, err, rolledBack)
			require.Zero(t, h.rows.Manifest.Query().CountX(t.Context()))
			require.Zero(t, h.rows.Outbox.Query().CountX(t.Context()))
			select {
			case <-events:
				t.Fatal("rolled-back creation published")
			default:
			}
			fresh := record("committed", bundle)
			h.put(t, "stage", fresh)
			if outbox {
				require.Equal(t, 1, h.rows.Outbox.Query().CountX(t.Context()))
				select {
				case <-events:
					t.Fatal("outbox mode published before draining")
				default:
				}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				t.Cleanup(func() { cancel(); <-done })
				go func() {
					defer close(done)
					pd.Drain(h.rows, h.w.Broker(), time.Millisecond).Spin(ctx)
				}()
			}
			select {
			case event := <-events:
				require.Equal(t, api.ManifestCreatedAction, event.Method)
				require.Len(t, event.Changes, 1)
				require.Equal(t, api.ManifestCreatedEvent, event.Changes[0].By)
				res, err := h.client.Get(t.Context(), api.ManifestGetRequest_builder{
					Ref: api.ManifestRef_builder{Id: event.Changes[0].Key.Bytes()}.Build(),
				}.Build())
				require.NoError(t, err)
				require.Equal(t, fresh.Digest.String(), res.GetDigest())
			case <-time.After(5 * time.Second):
				t.Fatal("committed creation was not published")
			}
		})
	}
}
