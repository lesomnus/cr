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
	"github.com/lesomnus/payday/auth"
	"github.com/lesomnus/payday/config"
	"github.com/lesomnus/payday/pdpb"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/anypb"
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
	tenant, err := s.Ungated.Tenant().Add(t.Context(), api.TenantAddRequest_builder{Alias: "operator"}.Build())
	require.NoError(t, err)
	_, err = s.Ungated.Holder().Add(t.Context(), api.HolderAddRequest_builder{Tenant: tenant.Ref(), Alias: "admin"}.Build())
	require.NoError(t, err)
	s.Auth = auth.PlainInProcess()
	g, err := s.Grpc(t.Context(), c)
	require.NoError(t, err)
	l := bufconn.Listen(1 << 20)
	go g.Serve(l)
	t.Cleanup(func() { g.Stop(); l.Close() })
	conn, err := grpc.NewClient("passthrough:///registry-watch",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return l.Dial() }))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	unauth, err := pdpb.NewResourceServiceClient(conn).Watch(t.Context(), pdpb.WatchRequest_builder{}.Build())
	require.NoError(t, err)
	_, err = unauth.Recv()
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	const kind = "application/vnd.dev.sigstore.bundle.v0.3+json"
	ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(t.Context(), "authorization", "Plain @operator/admin"), 5*time.Second)
	defer cancel()
	resources := pdpb.NewResourceServiceClient(conn)
	holders, err := resources.Watch(ctx, pdpb.WatchRequest_builder{Filter: `has(event.value) && type(event.value) == app.Holder`}.Build())
	require.NoError(t, err)
	_, err = holders.Header()
	require.NoError(t, err)
	add := func(alias string) *pdpb.Op {
		t.Helper()
		request, err := anypb.New(api.HolderAddRequest_builder{Tenant: tenant.Ref(), Alias: alias}.Build())
		require.NoError(t, err)
		return pdpb.Op_builder{Method: api.HolderService_Add_FullMethodName, Request: request}.Build()
	}
	_, err = resources.Batch(ctx, pdpb.BatchRequest_builder{Ops: []*pdpb.Op{add("rollback"), add("rollback")}}.Build())
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	for _, call := range []struct {
		method string
		alias  string
		do     func(context.Context, *pdpb.BatchRequest, ...grpc.CallOption) (*pdpb.BatchResponse, error)
	}{
		{pdpb.ResourceService_Batch_FullMethodName, "resource", resources.Batch},
		{pdpb.BatchService_Do_FullMethodName, "legacy", pdpb.NewBatchServiceClient(conn).Do},
	} {
		_, err := call.do(ctx, pdpb.BatchRequest_builder{Ops: []*pdpb.Op{add(call.alias)}}.Build())
		require.NoError(t, err)
		event, err := holders.Recv()
		require.NoError(t, err)
		value := &api.Holder{}
		require.NoError(t, event.GetValue().UnmarshalTo(value))
		require.Equal(t, call.alias, value.GetAlias(), "rolled-back Batch must not publish")
		require.Equal(t, api.HolderService_Add_FullMethodName, event.GetMethod())
		require.Equal(t, call.method, event.GetCallMethod())
		require.Equal(t, pdpb.ChangeKind_CHANGE_KIND_CREATED, event.GetKind())
		require.False(t, event.HasPrevious())
	}
	stream, err := pdpb.NewResourceServiceClient(conn).Watch(ctx, pdpb.WatchRequest_builder{Filter: `has(event.value) && type(event.value) == app.Manifest && event.kind == payday.CHANGE_KIND_CREATED && event.value.artifact_type == "` + kind + `"`}.Build())
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
	firstValue := &api.Manifest{}
	require.NoError(t, first.GetValue().UnmarshalTo(firstValue))
	require.Equal(t, "stage", firstValue.GetRepo())
	require.Equal(t, kind, firstValue.GetArtifactType())
	require.Equal(t, pdpb.ChangeKind_CHANGE_KIND_CREATED, first.GetKind())
	push("stage")
	push("prod")
	second, err := stream.Recv()
	require.NoError(t, err)
	secondValue := &api.Manifest{}
	require.NoError(t, second.GetValue().UnmarshalTo(secondValue))
	require.Equal(t, "prod", secondValue.GetRepo())
	require.Equal(t, firstValue.GetDigest(), secondValue.GetDigest())
	require.NotEqual(t, firstValue.GetId(), secondValue.GetId())
}
