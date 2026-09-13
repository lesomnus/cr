// Package entruns records garbage collection runs as GcRun rows, which is what
// `GET /admin/gc` reads and the management plane lists.
package entruns

import (
	"context"
	"time"

	"github.com/lesomnus/payday/pdid"
	"github.com/protobuf-orm/ent/dialect/sql"

	"github.com/lesomnus/cr/gc"
	"github.com/lesomnus/cr/internal/ent"
	"github.com/lesomnus/cr/internal/ent/gcrun"
	"github.com/lesomnus/cr/server/pd"
)

// maxMissing bounds what one row says is missing; the log has the rest.
const maxMissing = 1000

type Runs struct {
	client *ent.Client
}

func New(client *ent.Client) *Runs {
	return &Runs{client: client}
}

var _ gc.Runs = (*Runs)(nil)

func toRun(v *ent.GcRun) gc.Run {
	r := gc.Run{
		ID:           pdid.Id(v.Id).String(),
		Kind:         v.Kind,
		Trigger:      v.Trigger,
		State:        v.State,
		Error:        v.Error,
		Stages:       int(v.Stages),
		Tags:         int(v.Tags),
		Manifests:    int(v.Manifests),
		Repositories: int(v.Repositories),
		Blobs:        int(v.Blobs),
		Bytes:        v.Bytes,
		Missing:      v.Missing,
		Started:      v.DateCreated,
		Finished:     v.DateFinished,
	}
	if r.Missing == nil {
		r.Missing = []string{}
	}
	return r
}

func (r *Runs) Start(ctx context.Context, kind, trigger string) (gc.Run, error) {
	now := time.Now().UTC()
	v, err := r.client.GcRun.Create().
		SetId(pdid.New(pd.GcRunDomain).Uuid()).
		SetKind(kind).
		SetTrigger(trigger).
		SetState(gc.StateRunning).
		SetError("").
		SetStages(0).
		SetTags(0).
		SetManifests(0).
		SetRepositories(0).
		SetBlobs(0).
		SetBytes(0).
		SetMissing([]string{}).
		SetDateCreated(now).
		SetDateUpdated(now).
		Save(ctx)
	if err != nil {
		return gc.Run{}, err
	}
	return toRun(v), nil
}

func (r *Runs) Finish(ctx context.Context, run gc.Run) error {
	id, err := pdid.Parse(run.ID)
	if err != nil {
		return gc.ErrRunNotFound
	}
	missing := run.Missing
	if len(missing) > maxMissing {
		missing = missing[:maxMissing]
	}
	if missing == nil {
		missing = []string{}
	}
	u := r.client.GcRun.UpdateOneId(id.Uuid()).
		SetState(run.State).
		SetError(run.Error).
		SetStages(int64(run.Stages)).
		SetTags(int64(run.Tags)).
		SetManifests(int64(run.Manifests)).
		SetRepositories(int64(run.Repositories)).
		SetBlobs(int64(run.Blobs)).
		SetBytes(run.Bytes).
		SetMissing(missing).
		SetDateUpdated(time.Now().UTC())
	if run.Finished != nil {
		u = u.SetDateFinished(run.Finished.UTC())
	}
	if err := u.Exec(ctx); err != nil {
		if ent.IsNotFound(err) {
			return gc.ErrRunNotFound
		}
		return err
	}
	return nil
}

func (r *Runs) Get(ctx context.Context, id string) (gc.Run, error) {
	k, err := pdid.Parse(id)
	if err != nil {
		return gc.Run{}, gc.ErrRunNotFound
	}
	v, err := r.client.GcRun.Get(ctx, k.Uuid())
	if err != nil {
		if ent.IsNotFound(err) {
			return gc.Run{}, gc.ErrRunNotFound
		}
		return gc.Run{}, err
	}
	return toRun(v), nil
}

func (r *Runs) List(ctx context.Context, n int) ([]gc.Run, error) {
	q := r.client.GcRun.Query().Order(gcrun.ByDateCreated(sql.OrderDesc()), gcrun.ById(sql.OrderDesc()))
	if n > 0 {
		q = q.Limit(n)
	}
	vs, err := q.All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]gc.Run, 0, len(vs))
	for _, v := range vs {
		out = append(out, toRun(v))
	}
	return out, nil
}
