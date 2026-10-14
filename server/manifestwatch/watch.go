// Package manifestwatch adds artifact-scoped creation notifications to the
// generated Manifest service.
package manifestwatch

import (
	"errors"

	"github.com/lesomnus/cr/api"
	"github.com/lesomnus/cr/server/pd"
	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/watch"
	"github.com/protobuf-orm/ent/dialect"
	"github.com/protobuf-orm/protoc-gen-orm-ent/runtime/enttx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type Server struct {
	api.Overlay
	w *watch.Watch
}

func New(next api.Server, w *watch.Watch) Server {
	return Server{Overlay: api.NewOverlay(next), w: w}
}

func Build(w *watch.Watch) api.Builder {
	return api.BuilderFunc(func(next api.Server) (api.Server, error) {
		return New(next, w), nil
	})
}

func (s Server) WithDriver(drv dialect.Driver) (api.Server, error) {
	next, err := enttx.Rebind(s.Next(), drv)
	if err != nil {
		return nil, err
	}
	return New(next, s.w), nil
}

var (
	_ api.Server               = Server{}
	_ enttx.Binder[api.Server] = Server{}
)

func (s Server) Manifest() api.ManifestServiceServer {
	return service{ManifestServiceServer: s.Next().Manifest(), w: s.w}
}

type service struct {
	api.ManifestServiceServer
	w *watch.Watch
}

type filter struct {
	value *api.ManifestFilter
	key   pdid.Id
}

func (s service) Watch(req *api.ManifestWatchRequest, out grpc.ServerStreamingServer[api.ManifestWatchResponse]) error {
	if !req.GetCreatedOnly() {
		return s.ManifestServiceServer.Watch(req, out)
	}
	ctx := out.Context()
	fs := req.GetFilters()
	if len(fs) == 0 || len(fs) > pd.ManifestFilterLimit {
		return status.Errorf(codes.InvalidArgument, "filters: provide between 1 and %d artifact_type filters", pd.ManifestFilterLimit)
	}
	filters := make([]filter, 0, len(fs))
	for i, f := range fs {
		if f.GetArtifactType() == "" {
			return status.Errorf(codes.InvalidArgument, "filters[%d]: created_only requires a nonempty artifact_type", i)
		}
		v := filter{value: proto.Clone(f).(*api.ManifestFilter)}
		if f.HasRef() {
			row, err := s.Get(ctx, api.ManifestGetRequest_builder{Ref: f.GetRef()}.Build())
			if err != nil {
				return err
			}
			v.key, err = pdid.From(row.GetId())
			if err != nil {
				return err
			}
		}
		filters = append(filters, v)
	}
	if s.w == nil || s.w.Broker() == nil {
		return watch.ErrNoBroker
	}
	events, stop := s.w.Subscribe()
	defer stop()
	if err := out.SendHeader(nil); err != nil {
		return err
	}

	// Creation streams have no snapshot or growing set of seen keys. Only
	// insertions reach this source, so pull bookkeeping and removals are quiet.
	for {
		keys, err := watch.Next(ctx, events, api.ManifestCreatedEvent)
		if err != nil {
			if errors.Is(err, watch.ErrBehind) {
				return status.Error(codes.ResourceExhausted, "creation stream fell behind; reconnect and reconcile with List")
			}
			return err
		}
		items := make([]*api.ManifestWatchItem, 0, len(keys))
		for key := range keys {
			fs := make([]*api.ManifestFilter, 0, len(filters))
			for _, f := range filters {
				if f.key != pdid.Nil && f.key != key {
					continue
				}
				v := proto.Clone(f.value).(*api.ManifestFilter)
				v.SetRef(api.ManifestRef_builder{Id: key.Bytes()}.Build())
				fs = append(fs, v)
			}
			if len(fs) == 0 {
				continue
			}
			// Reuse List for both filter semantics and the caller's read scope.
			// The extra ref bounds every query to the row just created.
			res, err := s.List(ctx, api.ManifestListRequest_builder{Filters: fs, Size: 1}.Build())
			if err != nil {
				return err
			}
			for _, row := range res.GetItems() {
				items = append(items, api.ManifestWatchItem_builder{
					Id: row.GetId(), Value: row, Action: api.ManifestCreatedAction,
				}.Build())
			}
		}
		if len(items) > 0 {
			if err := out.Send(api.ManifestWatchResponse_builder{Items: items}.Build()); err != nil {
				return err
			}
		}
	}
}
