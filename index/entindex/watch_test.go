package entindex_test

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
	"github.com/lesomnus/cr/server/pd"
	"github.com/lesomnus/payday/config"
	_ "github.com/lesomnus/payday/config/dbsqlite3"
	"github.com/lesomnus/payday/pdpb"
	"github.com/lesomnus/payday/watch"
	"github.com/opencontainers/go-digest"
	entsql "github.com/protobuf-orm/ent/dialect/sql"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

const bundle = "application/vnd.dev.sigstore.bundle.v0.3+json"

type harness struct {
	rows      *ent.Client
	ix        *entindex.Index
	client    api.ManifestServiceClient
	w         *watch.Watch
	resources pdpb.ResourceServiceClient
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
	s := sink.WithWatch(w)
	guard := (config.ServerConfig{}).Guard(nil)
	resources, err := pd.Resources(s, drv, guard, w, pd.ResourceProjector(rows, scope, guard))
	require.NoError(t, err)
	g := grpc.NewServer()
	api.RegisterManifestServiceServer(g, s.Manifest())
	pdpb.RegisterResourceServiceServer(g, resources)
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
	return &harness{rows: rows, ix: ix, client: api.NewManifestServiceClient(conn), resources: pdpb.NewResourceServiceClient(conn), w: w}
}

func record(name, artifactType string) index.Manifest {
	return index.Manifest{Digest: digest.FromString(name), MediaType: "application/vnd.oci.image.manifest.v1+json", ArtifactType: artifactType}
}

func (h *harness) put(t *testing.T, repo string, m index.Manifest) {
	t.Helper()
	require.NoError(t, h.ix.Manifest().Put(t.Context(), repo, m, nil))
}

const manifestCreations = `has(event.value) && type(event.value) == app.Manifest && event.kind == payday.CHANGE_KIND_CREATED`

func (h *harness) created(t *testing.T, filter string) grpc.ServerStreamingClient[pdpb.WatchEvent] {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := h.resources.Watch(ctx, pdpb.WatchRequest_builder{Filter: filter}.Build())
	require.NoError(t, err)
	_, err = stream.Header()
	require.NoError(t, err)
	return stream
}

func receive(t *testing.T, stream grpc.ServerStreamingClient[pdpb.WatchEvent]) *api.Manifest {
	t.Helper()
	event, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, pdpb.ChangeKind_CHANGE_KIND_CREATED, event.GetKind())
	require.Equal(t, entindex.ManifestPutMethod, event.GetMethod())
	require.Equal(t, entindex.ManifestPutMethod, event.GetCallMethod())
	require.Len(t, event.GetId(), 16)
	require.NoError(t, event.GetDateCreated().CheckValid())
	require.False(t, event.HasPrevious())
	value := &api.Manifest{}
	require.NoError(t, event.GetValue().UnmarshalTo(value))
	require.NotEmpty(t, value.GetId())
	require.NotEqual(t, value.GetId(), event.GetId())
	return value
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

func TestResourceWatchManifestCreations(t *testing.T) {
	h := newHarness(t, nil, false, watch.Memory())
	old := record("already-there", bundle)
	h.put(t, "stage", old)
	stream := h.created(t, manifestCreations+` && event.value.artifact_type == "`+bundle+`"`)

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
	require.Equal(t, fresh.Digest.String(), first.GetDigest())
	require.Equal(t, "stage", first.GetRepo())
	// A second repository gets a new row even when the digest is identical.
	h.put(t, "prod", fresh)
	second := receive(t, stream)
	require.Equal(t, "prod", second.GetRepo())
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
	subject := digest.FromString("subject")
	stream := h.created(t, manifestCreations+` && (
		(event.value.artifact_type == "`+bundle+`" && event.value.repo == "stage") ||
		(event.value.artifact_type == "application/sbom" && event.value.subject == "`+subject.String()+`"))`)
	h.put(t, "prod", record("wrong-repo", bundle))
	h.put(t, "stage", record("wrong-subject", "application/sbom"))
	hidden := record("hidden", "application/sbom")
	hidden.Subject = subject
	h.put(t, "hidden", hidden)
	match := record("matching", bundle)
	h.put(t, "stage", match)
	require.Equal(t, match.Digest.String(), receive(t, stream).GetDigest())
	other := record("other-filter", "application/sbom")
	other.Subject = subject
	h.put(t, "prod", other)
	require.Equal(t, other.Digest.String(), receive(t, stream).GetDigest())
}

func TestWatchValidationAndRefCompatibility(t *testing.T) {
	h := newHarness(t, nil, false, watch.Memory())
	for _, filter := range []string{"event.kind ==", "1", "event.unknown"} {
		stream, err := h.resources.Watch(t.Context(), pdpb.WatchRequest_builder{Filter: filter}.Build())
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
	events, err := disabled.resources.Watch(ctx, pdpb.WatchRequest_builder{}.Build())
	require.NoError(t, err)
	_, err = events.Recv()
	require.Equal(t, codes.Unavailable, status.Code(err))

	cut := newHarness(t, nil, false, disconnected{})
	events = cut.created(t, manifestCreations)
	_, err = events.Recv()
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
			var stored *pdpb.WatchEvent
			if outbox {
				row := h.rows.Outbox.Query().OnlyX(t.Context())
				require.NotNil(t, row.Event)
				stored = &pdpb.WatchEvent{}
				require.NoError(t, proto.Unmarshal(*row.Event, stored))
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
				require.Equal(t, entindex.ManifestPutMethod, event.Method)
				require.Len(t, event.Changes, 1)
				require.Equal(t, entindex.ManifestPutMethod, event.Changes[0].By)
				require.NotNil(t, event.Changes[0].Resource)
				if outbox {
					require.True(t, proto.Equal(stored, event.Changes[0].Resource))
				}
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

func TestManifestCreationOutboxKeepsRecordedImage(t *testing.T) {
	h := newHarness(t, visible{}, true, watch.Memory())
	stream := h.created(t, manifestCreations+` && !has(event.value.date_pulled) && event.method == "/cr.index.Manifest/Put" && event.value.artifact_type == "`+bundle+`"`)
	old := record("imported", bundle)
	old.CreatedAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	h.put(t, "hidden", old)
	h.put(t, "stage", old)
	row := h.rows.Manifest.Query().Where(manifest.Repo("stage")).OnlyX(t.Context())
	// Change a field used by the CEL filter, then delete before the drainer can
	// deliver. Authorization and payload must use the recorded root image.
	require.NoError(t, h.rows.Manifest.UpdateOne(row).SetDatePulled(time.Now()).Exec(t.Context()))
	_, err := h.ix.Manifest().Erase(t.Context(), "stage", old.Digest)
	require.NoError(t, err)
	_, err = h.ix.Manifest().Erase(t.Context(), "hidden", old.Digest)
	require.NoError(t, err)
	require.Zero(t, h.rows.Manifest.Query().CountX(t.Context()))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		pd.Drain(h.rows, h.w.Broker(), time.Millisecond).Spin(ctx)
	}()
	event, err := stream.Recv()
	require.NoError(t, err)
	value := &api.Manifest{}
	require.NoError(t, event.GetValue().UnmarshalTo(value))
	require.Equal(t, "stage", value.GetRepo(), "the hidden image must still be denied after deletion")
	require.Equal(t, bundle, value.GetArtifactType())
	require.Equal(t, old.CreatedAt, value.GetDateCreated().AsTime())
	require.Nil(t, value.GetDatePulled())
	require.True(t, event.GetDateCreated().AsTime().After(old.CreatedAt))
}
