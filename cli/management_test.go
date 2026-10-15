package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lesomnus/cr/api"
	"github.com/lesomnus/cr/cmd"
	"github.com/lesomnus/payday/auth"
	"github.com/lesomnus/payday/config"
	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdpb"
	"github.com/lesomnus/payday/pdtest"
	"github.com/lesomnus/payday/rbac"
	"github.com/lesomnus/payday/resource"
	"github.com/lesomnus/payday/watch"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const managementReader = `tokens:
  - holder: '@operator/kamino'
    token: test-reader
policy:
  roles:
    '@operator/reader':
      methods: [/app.ManifestService/Get, /app.ManifestService/List, /payday.ResourceService/Watch, /payday.ResourceService/Batch, /payday.BatchService/Do]
  groups:
    '@operator/readers':
      members: ['@operator/kamino']
  bindings:
    - role: '@operator/reader'
      group: '@operator/readers'
`

func TestManagementRowFreeReadersAndRevocation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "management.yaml")
	write := func(v string) { require.NoError(t, os.WriteFile(p, []byte(v), 0600)) }
	write(managementReader)
	c := cmd.Config{
		Db:         config.DbConfig{Driver: "sqlite3", Dsn: fmt.Sprintf("file:%s/cr.db?_pragma=journal_mode(WAL)", t.TempDir())},
		Watch:      config.WatchConfig{Broker: config.BrokerMemory},
		Management: cmd.ManagementConfig{File: p},
	}
	s, err := cmd.Build(t.Context(), c)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	require.NoError(t, Migrate(t.Context(), s))
	s.Auth, err = Management(&c, s)
	require.NoError(t, err)
	g, err := s.Grpc(t.Context(), c)
	require.NoError(t, err)
	conn := pdtest.Serve(t, g)
	ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(t.Context(), "authorization", "Bearer test-reader"), 5*time.Second)
	defer cancel()
	_, err = api.NewManifestServiceClient(conn).List(ctx, api.ManifestListRequest_builder{}.Build())
	require.NoError(t, err)
	for _, count := range []func(context.Context) (int, error){s.Ent.Holder.Query().Count, s.Ent.Tenant.Query().Count} {
		n, err := count(ctx)
		require.NoError(t, err)
		require.Zero(t, n)
	}
	_, err = api.NewTagRuleServiceClient(conn).List(ctx, api.TagRuleListRequest_builder{}.Build())
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = api.NewManifestServiceClient(conn).List(t.Context(), api.ManifestListRequest_builder{}.Build())
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	request, err := anypb.New(api.TagRuleListRequest_builder{}.Build())
	require.NoError(t, err)
	for _, call := range []func(context.Context, *pdpb.BatchRequest, ...grpc.CallOption) (*pdpb.BatchResponse, error){pdpb.NewResourceServiceClient(conn).Batch, pdpb.NewBatchServiceClient(conn).Do} {
		_, err := call(ctx, pdpb.BatchRequest_builder{Ops: []*pdpb.Op{pdpb.Op_builder{Method: api.TagRuleService_List_FullMethodName, Request: request}.Build()}}.Build())
		require.Equal(t, codes.PermissionDenied, status.Code(err))
	}
	stream, err := pdpb.NewResourceServiceClient(conn).Watch(ctx, pdpb.WatchRequest_builder{}.Build())
	require.NoError(t, err)
	_, err = stream.Header()
	require.NoError(t, err)
	// The Watch endpoint alone must not expose images lacking Get permission.
	for _, value := range []proto.Message{
		api.Repository_builder{Id: pdid.New(10).Bytes(), Name: "private"}.Build(),
		api.Manifest_builder{Id: pdid.New(11).Bytes(), Repo: "visible", DateCreated: timestamppb.Now(), DateUpdated: timestamppb.Now()}.Build(),
	} {
		event, err := resource.Event(pdid.New(4).Bytes(), time.Now(), "/index/Create", "/index/Create", pdpb.ChangeKind_CHANGE_KIND_CREATED, nil, value)
		require.NoError(t, err)
		s.Watch.Broker().Publish(ctx, watch.Event{Changes: []watch.Change{{Resource: event}}})
	}
	event, err := stream.Recv()
	require.NoError(t, err)
	require.True(t, event.GetValue().MessageIs(&api.Manifest{}), "Watch exposed an image without Get permission")
	var store *managementStore
	for _, worker := range s.Spin {
		if v, ok := worker.(*managementStore); ok {
			store = v
		}
	}
	require.NotNil(t, store)
	write("policy: [")
	_ = store.refresh()
	require.Error(t, store.refresh())
	_, err = api.NewManifestServiceClient(conn).List(ctx, api.ManifestListRequest_builder{}.Build())
	require.NoError(t, err, "invalid edits keep the last valid credentials and grants")
	write("tokens:\n  - holder: '@operator/kamino'\n    token: test-reader\npolicy: {}\n")
	require.NoError(t, store.refresh())
	require.NoError(t, store.refresh())
	_, err = stream.Recv()
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	_, err = api.NewManifestServiceClient(conn).List(ctx, api.ManifestListRequest_builder{}.Build())
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	write("tokens: []\npolicy: {}\n")
	require.NoError(t, store.refresh())
	require.NoError(t, store.refresh())
	_, err = api.NewManifestServiceClient(conn).List(ctx, api.ManifestListRequest_builder{}.Build())
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestManagementAllowNarrowsRoles(t *testing.T) {
	for _, tt := range []struct {
		name      string
		allow     []string
		policy    *rbac.Rules
		get, list bool
	}{
		{"legacy", nil, nil, true, true},
		{"empty", []string{}, nil, false, false},
		{"allow", []string{"/app.ManifestService/Get"}, nil, true, false},
		{"role intersection", []string{"/app.ManifestService/*"}, &rbac.Rules{Roles: map[string]rbac.Role{"@operator/reader": {Methods: []string{"/app.*/Get"}}}, Bindings: []rbac.Binding{{Role: "@operator/reader", Holder: "@operator/kamino"}}}, true, false},
		{"no binding", nil, &rbac.Rules{}, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ids, err := managementIdentities(managementDocument{Tokens: []cmd.ManagementTokenConfig{{Holder: "@operator/kamino", Token: "test", Allow: tt.allow}}, Policy: tt.policy})
			require.NoError(t, err)
			for _, id := range ids {
				require.Equal(t, tt.get, id.Grant.Allows("/app.ManifestService/Get"))
				require.Equal(t, tt.list, id.Grant.Allows("/app.ManifestService/List"))
			}
		})
	}
}

func TestManagementEmptyAllowSurvivesYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "management.yaml")
	require.NoError(t, os.WriteFile(p, []byte("tokens:\n - holder: '@operator/kamino'\n   token: test\n   allow: []\n"), 0600))
	s, err := newManagementStore(&cmd.Config{Management: cmd.ManagementConfig{File: p}})
	require.NoError(t, err)
	id, ok := s.lookup("test")
	require.True(t, ok)
	require.False(t, id.Grant.Allows("/app.ManifestService/Get"))
	require.Equal(t, cmd.ConfigurationAuthority, id.Authority)
}

func TestManagementWritesNeedPersistentHolder(t *testing.T) {
	c := cmd.Config{Db: config.DbConfig{Driver: "sqlite3", Dsn: fmt.Sprintf("file:%s/cr.db", t.TempDir())}, Watch: config.WatchConfig{Broker: config.BrokerMemory}, Management: cmd.ManagementConfig{Tokens: []cmd.ManagementTokenConfig{{Holder: "@operator/writer", Token: "test-writer"}, {Holder: "@operator/reader", Token: "test-reader", Allow: []string{api.HolderService_List_FullMethodName}}, {Holder: "@elsewhere/reader", Token: "test-elsewhere", Allow: []string{api.HolderService_List_FullMethodName}}}}}
	s, err := cmd.Build(t.Context(), c)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	require.NoError(t, Migrate(t.Context(), s))
	s.Auth, err = Management(&c, s)
	require.NoError(t, err)
	g, err := s.Grpc(t.Context(), c)
	require.NoError(t, err)
	conn := pdtest.Serve(t, g)
	wire := metadata.AppendToOutgoingContext(t.Context(), "authorization", "Bearer test-writer")
	_, err = api.NewHolderServiceClient(conn).Add(wire, api.HolderAddRequest_builder{Alias: "other"}.Build())
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "requires a registered holder @operator/writer")
	request, err := anypb.New(api.HolderAddRequest_builder{Alias: "other"}.Build())
	require.NoError(t, err)
	_, err = pdpb.NewResourceServiceClient(conn).Batch(wire, pdpb.BatchRequest_builder{Ops: []*pdpb.Op{pdpb.Op_builder{Method: api.HolderService_Add_FullMethodName, Request: request}.Build()}}.Build())
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "requires a registered holder")
	// An unrelated credential with the same name still uses directory resolution.
	_, err = s.Resolver.Resolve(t.Context(), auth.Identity{Tenant: "operator", Alias: "writer"})
	require.ErrorIs(t, err, auth.ErrNoCredential)
	tenant, err := s.Ungated.Tenant().Add(t.Context(), api.TenantAddRequest_builder{Alias: "operator"}.Build())
	require.NoError(t, err)
	_, err = s.Ungated.Holder().Add(t.Context(), api.HolderAddRequest_builder{Tenant: tenant.Ref(), Alias: "writer"}.Build())
	require.NoError(t, err)
	_, err = api.NewHolderServiceClient(conn).Add(wire, api.HolderAddRequest_builder{Tenant: tenant.Ref(), Alias: "other"}.Build())
	require.NoError(t, err)
	list, err := api.NewHolderServiceClient(conn).List(wire, api.HolderListRequest_builder{}.Build())
	require.NoError(t, err)
	require.Len(t, list.GetItems(), 2)
	// Missing holders receive the existing tenant's scope, or an empty scope.
	for _, tt := range []struct {
		token string
		count int
	}{{"test-reader", 2}, {"test-elsewhere", 0}} {
		scoped := metadata.AppendToOutgoingContext(t.Context(), "authorization", "Bearer "+tt.token)
		rows, err := api.NewHolderServiceClient(conn).List(scoped, api.HolderListRequest_builder{}.Build())
		require.NoError(t, err)
		require.Len(t, rows.GetItems(), tt.count)
	}
}
