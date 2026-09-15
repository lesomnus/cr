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

	"github.com/lesomnus/cr/telemetry"
)

// Instrument traces and measures h with the providers on ctx: a server span
// per request named for its route, and the HTTP server metrics the semantic
// conventions name -- the request duration, the bytes read from the request
// and written to the response, and the requests in flight -- by method, route
// and status.
//
// route names a request's route without its variables, `/v2/{name}/blobs/{digest}`,
// so neither the span names nor the metric's attributes grow with the number
// of repositories.
func Instrument(ctx context.Context, route func(*http.Request) string, h http.Handler) http.Handler {
	o := otx.From(ctx)
	tracer := o.Tracer()
	prop := o.Propagator()
	m := o.Meter()
	duration := telemetry.Seconds(m, "http.server.request.duration", "Duration of HTTP server requests.")
	requestSize := telemetry.Bytes(m, "http.server.request.body.size", "Bytes read from HTTP server request bodies.")
	responseSize := telemetry.Bytes(m, "http.server.response.body.size", "Bytes written to HTTP server response bodies.")
	active := telemetry.UpDown(m, "http.server.active_requests", "{request}", "Number of active HTTP server requests.")

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

		inFlight := metric.WithAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("http.route", rt),
		)
		active.Add(ctx, 1, inFlight)
		defer active.Add(ctx, -1, inFlight)

		body := &counted{ReadCloser: r.Body}
		r = r.WithContext(ctx)
		r.Body = body
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)

		span.SetAttributes(attribute.Int("http.response.status_code", rec.status))
		if rec.status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(rec.status))
		}
		attrs := metric.WithAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("http.route", rt),
			attribute.Int("http.response.status_code", rec.status),
		)
		duration.Record(ctx, time.Since(start).Seconds(), attrs)
		requestSize.Record(ctx, body.n, attrs)
		responseSize.Record(ctx, rec.written, attrs)
	})
}

// counted counts what a handler reads of the request body: what a push sent
// and the registry took, rather than what the header promised.
type counted struct {
	io.ReadCloser
	n int64
}

func (c *counted) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}

// recorder notes the status a handler answered with and what it wrote, and
// passes everything else through: `ReadFrom` especially, since that is what
// lets a blob served from a file go out with sendfile.
type recorder struct {
	http.ResponseWriter
	status  int
	wrote   bool
	written int64
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
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

func (r *recorder) ReadFrom(src io.Reader) (int64, error) {
	r.wrote = true
	var (
		n   int64
		err error
	)
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		n, err = rf.ReadFrom(src)
	} else {
		n, err = io.Copy(struct{ io.Writer }{r.ResponseWriter}, src)
	}
	r.written += n
	return n, err
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
