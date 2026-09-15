package cli

import (
	"context"
	"log/slog"
	"net"
	"slices"

	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/xli"
	entschema "github.com/protobuf-orm/ent/dialect/sql/schema"
	"golang.org/x/sync/errgroup"

	"github.com/lesomnus/payday/spin"

	"github.com/lesomnus/cr/cmd"
	entmigrate "github.com/lesomnus/cr/internal/ent/migrate"
)

// NewCmdServe is `<app> serve`.
//
// It is the app's own and not payday's, for the reason at the top of
// `cmd/config.go`: the body of this command is the stack, and a framework that
// supplied it would be hiding the one thing a reader of an app most needs to
// see.
func NewCmdServe(c *cmd.Config) *xli.Command {
	return &xli.Command{
		Name:  "serve",
		Brief: "answer requests",

		Handler: xli.OnRun(func(ctx context.Context, _ *xli.Command, next xli.Next) error {
			// Telemetry first, because everything after it logs. What
			// `Build` answers with is a context carrying the providers, and
			// what reads them is `grpcx` -- so a server built on a context
			// this did not touch traces and logs into providers that discard.
			ctx, done, err := Telemetry(ctx, c)
			if err != nil {
				return err
			}
			defer done()

			s, err := cmd.Build(ctx, *c)
			if err != nil {
				return err
			}
			defer s.Close()

			// The database, before anything is served on it.
			//
			// payday owns some of this app's schema, so a field added to a
			// holder there arrives in `internal/ent` the next time this app
			// generates -- and nothing about that is loud. It compiles, the
			// tests pass against a database the tests just created, and the
			// first sign of trouble is a column that is not there in the one
			// handler that reads it.
			//
			// So one of the two happens here, and which one is the operator's
			// to say. `db.migrate: true` hands the serving process the right to
			// alter tables, which is right for development and is a thing to
			// decide on purpose; anything else and the shapes have to agree
			// already.
			if c.Db.Migrate {
				if err := Migrate(ctx, s); err != nil {
					return err
				}
			} else if err := entschema.Check(ctx, s.Db, s.Dialect, entmigrate.Tables); err != nil {
				return err
			}

			if err := runtimeMetrics(ctx); err != nil {
				return err
			}
			if err := Registry(ctx, c, s); err != nil {
				return err
			}

			// Not `Plain`: this listener is reachable, and `Plain`
			// believes whoever a caller says they are.
			h, err := Management(c, s)
			if err != nil {
				return err
			}
			s.Auth = h

			l, err := net.Listen("tcp", c.Server.ListenAddr())
			if err != nil {
				return err
			}

			log.From(ctx).InfoContext(ctx, "grpc", slog.String("addr", l.Addr().String()))

			// The background work and the server, together: whichever stops
			// first stops the other. A loop that keeps running under a server
			// that is going down is a process that will not exit, and a server
			// that goes on answering after its outbox drain has died is one
			// that accepts writes it will never publish.
			g, ctx := errgroup.WithContext(ctx)
			g.Go(func() error { return spin.Run(ctx, slices.Values(s.Spin)) })
			g.Go(func() error { return s.Serve(ctx, *c, l) })

			return g.Wait()
		}),
	}
}
