package entindex

import (
	"context"

	"github.com/lesomnus/cr/api"
	"github.com/lesomnus/cr/server/bare"
	"github.com/lesomnus/cr/server/pd"
	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/pdpb"
	"github.com/lesomnus/payday/resource"
	"github.com/lesomnus/payday/watch"
	"google.golang.org/protobuf/proto"
)

// ManifestPutMethod identifies the direct index operation in resource events.
// Manifests are read-only over gRPC; this names the writer, not a callable RPC.
const ManifestPutMethod = "/cr.index.Manifest/Put"

// WithManifestWatch publishes newly inserted manifests to ResourceService.Watch
// after commit. With outbox enabled, the same envelope is queued in the write
// transaction and the server's drainer publishes it, including after a restart.
func WithManifestWatch(w *watch.Watch, outbox bool) Option {
	return func(o *options) {
		if w != nil && w.Broker() != nil {
			o.watch = w
			o.outbox = outbox
		}
	}
}

func (ix *Index) recordCreated(ctx context.Context, key pdid.Id) error {
	if ix.o.watch == nil {
		return nil
	}
	// Direct SQL bypasses ORM recorders. Read the persisted value inside this
	// transaction so later updates/deletion cannot change the event's payload.
	// Subscriber authorization is applied by payday's historical projector.
	s := bare.Server{Store: bare.Store{Db: ix.client}}
	value, err := s.Manifest().Get(ctx, api.ManifestGetRequest_builder{
		Ref: api.ManifestRef_builder{Id: key.Bytes()}.Build(),
	}.Build())
	if err != nil {
		return err
	}
	id, at := pdid.New(pd.OutboxDomain), ix.now()
	event, err := resource.Event(id.Bytes(), at, ManifestPutMethod, ManifestPutMethod,
		pdpb.ChangeKind_CHANGE_KIND_CREATED, nil, value)
	if err != nil {
		return err
	}
	if ix.o.outbox {
		data, err := proto.Marshal(event)
		if err != nil {
			return err
		}
		return ix.client.Outbox.Create().SetId(id.Uuid()).SetDateCreated(at).
			SetActorId(pdid.Nil.Uuid()).SetTenantId(pdid.Nil.Uuid()).
			SetMethod(ManifestPutMethod).SetBy(ManifestPutMethod).
			SetObjectId(key.Uuid()).SetPatch([]byte{}).SetEvent(data).Exec(ctx)
	}
	ix.created = append(ix.created, watch.Change{
		Method: ManifestPutMethod, By: ManifestPutMethod, Key: key, Resource: event,
	})
	return nil
}

func (ix *Index) publishCreated(ctx context.Context) {
	if len(ix.created) == 0 {
		return
	}
	ix.o.watch.Broker().Publish(context.WithoutCancel(ctx), watch.Event{
		Method: ManifestPutMethod, Changes: ix.created,
	})
}
