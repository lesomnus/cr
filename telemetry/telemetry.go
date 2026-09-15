// Package telemetry is what cr's packages share of their metrics: the
// histogram boundaries, and instruments that measure nothing when there is no
// meter to make them with, so that a package holds an instrument and never a
// nil.
package telemetry

import (
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// The boundaries: the HTTP semantic conventions' for a duration in seconds,
// and for a size in bytes steps far enough apart to tell a manifest from a
// layer from an image. The SDK's defaults are meant for milliseconds, and put
// every request there is in their first bucket.
var (
	SecondsBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}
	BytesBuckets   = []float64{1 << 10, 16 << 10, 256 << 10, 4 << 20, 64 << 20, 1 << 30, 16 << 30}

	// LongSecondsBuckets is for what takes minutes: a collection, a flush.
	LongSecondsBuckets = []float64{0.1, 0.5, 1, 5, 15, 60, 300, 900, 1800, 3600}
)

// Long is a histogram of durations in seconds, for what takes minutes.
func Long(m metric.Meter, name, description string) metric.Float64Histogram {
	h, err := Meter(m).Float64Histogram(name,
		metric.WithUnit("s"),
		metric.WithDescription(description),
		metric.WithExplicitBucketBoundaries(LongSecondsBuckets...),
	)
	if err != nil {
		return noop.Float64Histogram{}
	}
	return h
}

// Meter answers m, or one that measures nothing.
func Meter(m metric.Meter) metric.Meter {
	if m == nil {
		return noop.Meter{}
	}
	return m
}

// Seconds is a histogram of durations in seconds.
func Seconds(m metric.Meter, name, description string) metric.Float64Histogram {
	h, err := Meter(m).Float64Histogram(name,
		metric.WithUnit("s"),
		metric.WithDescription(description),
		metric.WithExplicitBucketBoundaries(SecondsBuckets...),
	)
	if err != nil {
		return noop.Float64Histogram{}
	}
	return h
}

// Bytes is a histogram of sizes in bytes.
func Bytes(m metric.Meter, name, description string) metric.Int64Histogram {
	h, err := Meter(m).Int64Histogram(name,
		metric.WithUnit("By"),
		metric.WithDescription(description),
		metric.WithExplicitBucketBoundaries(BytesBuckets...),
	)
	if err != nil {
		return noop.Int64Histogram{}
	}
	return h
}

// Counter counts things that only go up.
func Counter(m metric.Meter, name, unit, description string) metric.Int64Counter {
	c, err := Meter(m).Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(description))
	if err != nil {
		return noop.Int64Counter{}
	}
	return c
}

// UpDown counts things that come and go.
func UpDown(m metric.Meter, name, unit, description string) metric.Int64UpDownCounter {
	c, err := Meter(m).Int64UpDownCounter(name, metric.WithUnit(unit), metric.WithDescription(description))
	if err != nil {
		return noop.Int64UpDownCounter{}
	}
	return c
}

// Gauge records the last value of something.
func Gauge(m metric.Meter, name, unit, description string) metric.Int64Gauge {
	g, err := Meter(m).Int64Gauge(name, metric.WithUnit(unit), metric.WithDescription(description))
	if err != nil {
		return noop.Int64Gauge{}
	}
	return g
}
