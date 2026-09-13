// Package httpx is what cr's plain HTTP handlers share: telemetry, and the
// health endpoints a deployment probes.
package httpx

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/lesomnus/otx"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Instrument traces and measures h with the providers on ctx: a server span
// per request named for its route, and the request duration histogram the
// HTTP semantic conventions name, by method, route and status.
//
// route names a request's route without its variables, `/v2/{name}/blobs/{digest}`,
// so neither the span names nor the metric's attributes grow with the number
// of repositories.
func Instrument(ctx context.Context, route func(*http.Request) string, h http.Handler) http.Handler {
	o := otx.From(ctx)
	tracer := o.Tracer()
	prop := o.Propagator()
	duration, err := o.Meter().Float64Histogram("http.server.request.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of HTTP server requests."),
	)
	if err != nil {
		duration = nil
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rt := route(r)

		ctx := prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tracer.Start(ctx, r.Method+" "+rt,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", r.Method),
				attribute.String("http.route", rt),
				attribute.String("url.path", r.URL.Path),
			),
		)
		defer span.End()

		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r.WithContext(ctx))

		span.SetAttributes(attribute.Int("http.response.status_code", rec.status))
		if rec.status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(rec.status))
		}
		if duration != nil {
			duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(
				attribute.String("http.request.method", r.Method),
				attribute.String("http.route", rt),
				attribute.Int("http.response.status_code", rec.status),
			))
		}
	})
}

// recorder notes the status a handler answered with, and passes everything
// else through: `ReadFrom` especially, since that is what lets a blob served
// from a file go out with sendfile.
type recorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *recorder) WriteHeader(status int) {
	if !r.wrote {
		r.status = status
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

func (r *recorder) ReadFrom(src io.Reader) (int64, error) {
	r.wrote = true
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(struct{ io.Writer }{r.ResponseWriter}, src)
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Live answers 200 for as long as the process can answer at all.
func Live() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	})
}

// Ready answers 200 when every check passes within a second, and 503 with the
// first failure otherwise: what a load balancer should route by.
func Ready(checks ...func(context.Context) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for _, check := range checks {
			if err := check(ctx); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte(err.Error() + "\n"))
				return
			}
		}
		w.Write([]byte("ok\n"))
	})
}
