package cmd

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/lesomnus/otx/log"
	"google.golang.org/grpc"
)

// ShutdownConfig is what happens between `serve` being told to stop -- SIGTERM,
// or SIGINT -- and the process exiting.
type ShutdownConfig struct {
	// Drain is how long both listeners go on answering once the server is told
	// to stop, with `/readyz` failing meanwhile, so that whatever routes by it
	// takes the server out of rotation before anything closes; zero closes at
	// once.
	Drain time.Duration `yaml:"drain"`

	// Timeout is how long requests in flight get to finish once the listeners
	// have closed, before they are cut; zero is twenty seconds.
	Timeout time.Duration `yaml:"timeout"`
}

func (c ShutdownConfig) timeout() time.Duration {
	if c.Timeout <= 0 {
		return 20 * time.Second
	}
	return c.Timeout
}

var errStopping = errors.New("stopping")

// Stopping fails once the server has been told to stop. It is a readiness
// check: what routes by `/readyz` sends the next request elsewhere while this
// server finishes the ones it has.
func (s *Server) Stopping(context.Context) error {
	if s.stopping.Load() {
		return errStopping
	}
	return nil
}

// shutdown takes the server down in the order what routes to it needs.
//
// `/readyz` fails first, and both listeners go on answering through the drain,
// so the next request goes elsewhere while this server can still take it. Then
// the HTTP listener closes, and what it has in flight -- a push, a pull -- gets
// until the timeout to finish. gRPC stops after it rather than beside it: the
// management API over HTTP is a gRPC call inside this process, and a gRPC
// server that stops ends those at once. Whatever is still running at the
// timeout is cut.
func (s *Server) shutdown(ctx context.Context, c ShutdownConfig, g *grpc.Server, srv *http.Server) {
	ctx = context.WithoutCancel(ctx)
	s.stopping.Store(true)
	log.From(ctx).InfoContext(ctx, "stopping", slog.Duration("drain", c.Drain), slog.Duration("timeout", c.timeout()))

	time.Sleep(c.Drain)

	cut, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	if srv != nil {
		if err := srv.Shutdown(cut); err != nil {
			log.From(ctx).WarnContext(ctx, "http: cutting what is still running at the shutdown timeout")
			srv.Close()
		}
	}

	done := make(chan struct{})
	go func() {
		g.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-cut.Done():
		log.From(ctx).WarnContext(ctx, "grpc: cutting what is still running at the shutdown timeout")
		g.Stop()
		<-done
	}

	log.From(ctx).InfoContext(ctx, "stopped")
}

// httpServer serves h, handing requests contexts that carry base's values.
//
// A Connect stream -- a `Watch` above all -- is never idle, so `Shutdown` would
// wait on it until the timeout. Each one ends instead the moment shutdown
// starts, and its client connects again wherever it is sent now. A unary call,
// and every request of the registry, is left to finish.
func httpServer(base context.Context, h http.Handler) *http.Server {
	streams, end := context.WithCancel(base)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/connect+") {
				h.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			defer context.AfterFunc(streams, cancel)()
			h.ServeHTTP(w, r.WithContext(ctx))
		}),
		BaseContext: func(net.Listener) context.Context { return base },
	}
	srv.RegisterOnShutdown(end)
	return srv
}
