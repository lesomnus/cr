package blob

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/lesomnus/flob"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/lesomnus/cr/telemetry"
)

// Measured is stores with every call to a store timed: the histogram
// `cr.store.operation.duration`, by `cr.store.driver`, `cr.store.operation`
// and `cr.store.outcome`. It measures the [flob.Store] methods and nothing a
// capability adds -- a mount, a presigned URL, a walk -- which reach the store
// beneath through Unwrap as they would without it.
func Measured(stores flob.Stores, driver string, m metric.Meter) flob.Stores {
	return &measured{
		inner:  stores,
		driver: attribute.String("cr.store.driver", driver),
		h:      telemetry.Seconds(m, "cr.store.operation.duration", "Duration of calls to the blob store."),
	}
}

type measured struct {
	inner  flob.Stores
	driver attribute.KeyValue
	h      metric.Float64Histogram
}

func (s *measured) Use(id string) flob.Store { return &measuredStore{inner: s.inner.Use(id), m: s} }

// Unwrap is the stores beneath, which is what lists namespaces and prunes
// uploads.
func (s *measured) Unwrap() flob.Stores { return s.inner }

func (s *measured) record(ctx context.Context, op string, start time.Time, err error) {
	outcome := "ok"
	switch {
	case err == nil:
	case errors.Is(err, flob.ErrNotExist):
		outcome = "not_found"
	case errors.Is(err, flob.ErrAlreadyExists):
		outcome = "exists"
	default:
		outcome = "error"
	}
	s.h.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(
		s.driver,
		attribute.String("cr.store.operation", op),
		attribute.String("cr.store.outcome", outcome),
	))
}

type measuredStore struct {
	inner flob.Store
	m     *measured
}

// Unwrap is the store beneath, for the capabilities it has and this does not
// claim.
func (s *measuredStore) Unwrap() flob.Store { return s.inner }

// Add takes as long as its reader does, so an upload's Add is as long as the
// upload.
func (s *measuredStore) Add(ctx context.Context, m flob.Meta, r io.Reader) (flob.Meta, error) {
	start := time.Now()
	out, err := s.inner.Add(ctx, m, r)
	s.m.record(ctx, "add", start, err)
	return out, err
}

func (s *measuredStore) Stat(ctx context.Context, d flob.Digest) (flob.Info, error) {
	start := time.Now()
	info, err := s.inner.Stat(ctx, d)
	s.m.record(ctx, "stat", start, err)
	return info, err
}

// Open is the opening, not the reading that follows it.
func (s *measuredStore) Open(ctx context.Context, d flob.Digest) (io.ReadSeekCloser, flob.Info, error) {
	start := time.Now()
	rc, info, err := s.inner.Open(ctx, d)
	s.m.record(ctx, "open", start, err)
	return rc, info, err
}

func (s *measuredStore) Label(ctx context.Context, d flob.Digest, labels flob.Labels) error {
	start := time.Now()
	err := s.inner.Label(ctx, d, labels)
	s.m.record(ctx, "label", start, err)
	return err
}

func (s *measuredStore) Erase(ctx context.Context, d flob.Digest) error {
	start := time.Now()
	err := s.inner.Erase(ctx, d)
	s.m.record(ctx, "erase", start, err)
	return err
}
