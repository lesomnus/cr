// Package entindex is the index over the ent client payday generates from
// cr's schema, on SQLite and PostgreSQL.
//
// It talks to the client directly and not through payday's servers: the
// registry's hot path has no frame, no wall and no gate to pass, and every
// row it writes is one the management plane reads back through them.
package entindex

import (
	"cmp"
	"context"
	stdsql "database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/protobuf-orm/ent/dialect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/internal/ent"
	"github.com/lesomnus/cr/telemetry"
)

// Index is [index.Index] over an ent client.
type Index struct {
	client  *ent.Client
	dialect string

	// Set on the Index a transaction hands to its function.
	inTx bool

	o *options
}

type options struct {
	wait time.Duration
	now  func() time.Time

	// The per-repository lock where the database has none to offer, and the
	// single writer SQLite wants, so two transactions never race to upgrade.
	stripes [256]chan struct{}
	writer  chan struct{}

	pulls *pulls

	// How long a repository's lock was waited for, and how often the wait
	// gave up; see [WithMeter].
	lockWait metric.Float64Histogram
	lockBusy metric.Int64Counter
}

type Option func(*options)

// WithMeter measures the waits for repositories' locks with m: the histogram
// `cr.repository.lock.wait` and the counter `cr.repository.lock.timeouts`,
// each by `cr.lock.for`, which is what [index.Waiting] said was waiting.
func WithMeter(m metric.Meter) Option {
	return func(o *options) {
		o.lockWait = telemetry.Seconds(m, "cr.repository.lock.wait", "Time a write waited for its repository's lock.")
		o.lockBusy = telemetry.Counter(m, "cr.repository.lock.timeouts", "{wait}", "Waits for a repository's lock that gave up.")
	}
}

// WithWait bounds how long a transaction waits for a repository's lock
// before it is [index.ErrBusy]. The default is thirty seconds.
func WithWait(d time.Duration) Option {
	return func(o *options) { o.wait = d }
}

// WithClock replaces time.Now.
func WithClock(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

var _ index.Index = (*Index)(nil)

func New(client *ent.Client, opts ...Option) *Index {
	o := &options{
		wait:   30 * time.Second,
		now:    time.Now,
		writer: make(chan struct{}, 1),
	}
	for i := range o.stripes {
		o.stripes[i] = make(chan struct{}, 1)
	}
	for _, opt := range opts {
		opt(o)
	}
	if o.lockWait == nil {
		WithMeter(nil)(o)
	}
	ix := &Index{client: client, dialect: client.Dialect(), o: o}
	o.pulls = newPulls(ix)
	return ix
}

// waited records how long ctx waited for a repository's lock, under what
// [index.Waiting] said was waiting, and whether the wait gave up.
func (ix *Index) waited(ctx context.Context, start time.Time, err error) {
	attrs := metric.WithAttributes(attribute.String("cr.lock.for", index.WaitingFor(ctx)))
	ix.o.lockWait.Record(ctx, time.Since(start).Seconds(), attrs)
	if errors.Is(err, index.ErrBusy) {
		ix.o.lockBusy.Add(ctx, 1, attrs)
	}
}

func (ix *Index) Repo() index.Repos         { return repos{ix} }
func (ix *Index) Manifest() index.Manifests { return manifests{ix} }
func (ix *Index) Tag() index.Tags           { return tags{ix} }
func (ix *Index) Pulled() index.Pulled      { return ix.o.pulls }

func (ix *Index) now() time.Time { return ix.o.now().UTC() }

// Spin flushes the pull bookkeeping until ctx is done; see [index.Pulled].
func (ix *Index) Spin(ctx context.Context) error {
	return ix.o.pulls.run(ctx)
}

// Flush writes what [index.Pulled] has queued.
func (ix *Index) Flush(ctx context.Context) error {
	return ix.o.pulls.flush(ctx)
}

func key(repo string) uint64 {
	h := fnv.New64a()
	h.Write([]byte("cr/repository/"))
	h.Write([]byte(repo))
	return h.Sum64()
}

func (ix *Index) acquire(ctx context.Context, c chan struct{}) (func(), error) {
	var timeout <-chan time.Time
	if ix.o.wait > 0 {
		t := time.NewTimer(ix.o.wait)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case c <- struct{}{}:
		return func() { <-c }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timeout:
		return nil, index.ErrBusy
	}
}

func (ix *Index) Tx(ctx context.Context, repo string, fn func(index.Index) error) error {
	if ix.inTx {
		return fn(ix)
	}

	pg := ix.dialect == dialect.Postgres
	if !pg {
		start := time.Now()
		if repo != "" {
			release, err := ix.acquire(ctx, ix.o.stripes[key(repo)%uint64(len(ix.o.stripes))])
			if err != nil {
				ix.waited(ctx, start, err)
				return err
			}
			defer release()
		}
		release, err := ix.acquire(ctx, ix.o.writer)
		ix.waited(ctx, start, err)
		if err != nil {
			return err
		}
		defer release()
	}

	drv, tx, err := dialect.BeginTx(ctx, ix.client.Driver())
	if err != nil {
		return err
	}
	if pg && repo != "" {
		start := time.Now()
		err := ix.lockPostgres(ctx, tx, repo)
		ix.waited(ctx, start, err)
		if err != nil {
			tx.Rollback()
			return err
		}
	}

	child := &Index{client: ix.client.WithDriver(drv), dialect: ix.dialect, inTx: true, o: ix.o}
	if err := fn(child); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (ix *Index) lockPostgres(ctx context.Context, tx dialect.Tx, repo string) error {
	if ix.o.wait > 0 {
		q := fmt.Sprintf("SET LOCAL lock_timeout = %d", ix.o.wait.Milliseconds())
		if err := tx.Exec(ctx, q, []any{}, nil); err != nil {
			return err
		}
	}
	var res stdsql.Result
	err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", []any{int64(key(repo))}, &res)
	if err == nil {
		return nil
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) && state.SQLState() == "55P03" {
		return index.ErrBusy
	}
	return err
}

// pulls is the batched write behind [index.Pulled].
type pulls struct {
	ix *Index

	mu        sync.Mutex
	manifests map[pullKey]time.Time
	tags      map[pullKey]time.Time
}

type pullKey struct{ repo, name string }

// Most distinct (repo, digest) pairs kept between flushes; past it a touch is
// dropped, which costs a retention decision a little precision.
const maxPending = 100_000

func newPulls(ix *Index) *pulls {
	return &pulls{ix: ix, manifests: map[pullKey]time.Time{}, tags: map[pullKey]time.Time{}}
}

func (p *pulls) Touch(repo, tag string, d digestLike, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.manifests) >= maxPending {
		return
	}
	k := pullKey{repo, string(d)}
	if at.After(p.manifests[k]) {
		p.manifests[k] = at
	}
	if tag != "" {
		k := pullKey{repo, tag}
		if at.After(p.tags[k]) {
			p.tags[k] = at
		}
	}
}

func (p *pulls) run(ctx context.Context) error {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			p.flush(context.WithoutCancel(ctx))
			return nil
		case <-t.C:
			p.flush(ctx)
		}
	}
}

// flushChunk is how many rows one flush transaction touches before it
// commits: enough for the round trips to amortise, few enough that SQLite's
// writer and PostgreSQL's row locks are not held for long.
const flushChunk = 500

func (p *pulls) flush(ctx context.Context) error {
	ctx = index.Waiting(ctx, "bookkeeping")
	p.mu.Lock()
	ms, ts := p.manifests, p.tags
	p.manifests, p.tags = map[pullKey]time.Time{}, map[pullKey]time.Time{}
	p.mu.Unlock()

	// A transaction per chunk rather than a commit per row, with the rows in
	// (repo, name) order -- the order a delete erases a repository's tags in
	// -- so that the two cannot deadlock on each other's rows. Manifests and
	// tags go separately, so no transaction holds rows of both tables. A
	// chunk that fails is lost, which costs a retention decision nothing
	// noticeable.
	var errs []error
	write := func(m map[pullKey]time.Time, touch func(*Index, context.Context, string, string, time.Time) error) {
		keys := slices.SortedFunc(maps.Keys(m), func(a, b pullKey) int {
			return cmp.Or(strings.Compare(a.repo, b.repo), strings.Compare(a.name, b.name))
		})
		for chunk := range slices.Chunk(keys, flushChunk) {
			err := p.ix.Tx(ctx, "", func(tx index.Index) error {
				ix := tx.(*Index)
				for _, k := range chunk {
					if err := touch(ix, ctx, k.repo, k.name, m[k]); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	write(ms, (*Index).touchManifest)
	write(ts, (*Index).touchTag)
	return errors.Join(errs...)
}
