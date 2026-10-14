package entindex

import (
	"context"

	"github.com/lesomnus/cr/api"
	"github.com/lesomnus/cr/server/bare"
	"github.com/lesomnus/cr/server/pd"
	"github.com/lesomnus/payday/pdid"
	"github.com/lesomnus/payday/watch"
)

// WithManifestWatch publishes newly inserted manifests to w after commit.
// With outbox enabled, it queues them in the same transaction instead; the
// server's existing drainer publishes them, including after a restart.
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
	if ix.o.outbox {
		return pd.OutboxRecorder().Record(ctx, bare.Server{Store: bare.Store{Db: ix.client}}, bare.Change{
			Method: api.ManifestCreatedAction, By: api.ManifestCreatedEvent, Key: key.Uuid(),
		})
	}
	ix.created = append(ix.created, watch.Change{By: api.ManifestCreatedEvent, Key: key})
	return nil
}

func (ix *Index) publishCreated(ctx context.Context) {
	if len(ix.created) == 0 {
		return
	}
	ix.o.watch.Broker().Publish(context.WithoutCancel(ctx), watch.Event{
		Method: api.ManifestCreatedAction, Changes: ix.created,
	})
}
