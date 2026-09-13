package cmd

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net"
	"net/http"

	"github.com/lesomnus/otx/log"
	"github.com/protobuf-orm/ent/dialect"
	entsql "github.com/protobuf-orm/ent/dialect/sql"
	"google.golang.org/grpc"

	"github.com/lesomnus/payday/auth"
	"github.com/lesomnus/payday/gate"
	"github.com/lesomnus/payday/grpcx"
	"github.com/lesomnus/payday/pdpb"
	"github.com/lesomnus/payday/watch"
	"github.com/lesomnus/payday/web"

	app "github.com/lesomnus/cr/api"
	"github.com/lesomnus/cr/internal/ent"
	"github.com/lesomnus/cr/server/bare"
	"github.com/lesomnus/cr/server/pd"
)

// Server is a built app: the database it runs on and the two stacks it answers
// through.
type Server struct {
	Db  *sql.DB
	Ent *ent.Client

	// Drv is what the client was built on, kept because a transaction is begun
	// on a driver and a `*ent.Client` does not hand out the one it holds. It is
	// what `pd.Batch` puts a whole stack onto.
	Drv dialect.Driver

	// Dialect is what that driver speaks, which `Open` worked out and nothing
	// else can. It is kept because the guard below needs it, and re-deriving it
	// from the configuration would be a second answer to a question already
	// answered.
	Dialect string

	// Watch is what a change is published to once the call that made it has
	// answered. The broker is named rather than defaulted: the one that
	// publishes in this process is right for one replica and **silently wrong**
	// for two, since a subscriber on one never hears about a write on another.
	Watch *watch.Watch

	// Walled is what a caller reaches, and Ungated is what the deployment does
	// its own work through -- putting the first tenant there, working out who
	// is calling. Neither is a privilege anybody holds: the second is a server
	// instance somebody was handed, so going around the wall is a line of
	// wiring a reader can find rather than a rule that opens up whenever
	// nobody is asking.
	Walled  app.Server
	Ungated app.Server

	// Auth is how a credential is read, and nothing that faces anybody is
	// [auth.Plain] -- which believes what the caller writes about themselves.
	// Replacing it is the first thing this app has to do that payday cannot do
	// for it.
	//
	// It is set after [Build] rather than being one of its arguments, because
	// what reads a credential is often built from what [Build] made: a session
	// handler needs a store, and a store is a table in this database.
	Auth auth.Handler

	// Resolver is how a credential becomes a caller. Nil looks the holder up
	// in this deployment's rows; a deployment whose callers another server
	// vouches for mirrors them first, see [MirrorResolver].
	Resolver auth.Resolver

	// Policy is what a caller may do and which tenants they may see, and nil is
	// the answer payday gives on its own: their own tenant and nothing more.
	//
	// A field beside [Server.Auth] because they are the pair -- who is calling,
	// and what that means here -- and because the two places it has to be
	// installed are not one place. `gate.Interceptor` covers the calls gRPC
	// dispatches; `c.Server.Guard` covers the operations inside a batch, which
	// arrive as one method carrying many and would otherwise be authorised as
	// the batch rather than as themselves.
	//
	// It is what a second binary is built from. A deployment that wants an
	// operator's path serves this app again with a policy that answers
	// differently, on an address only an operator can reach, rather than
	// putting a rule in here that says some row is special.
	Policy gate.Policy

	// Spin is whatever this deployment has to run besides answering requests.
	// It is a slice rather than a method because a server with nothing to run
	// should write nothing at all; see `payday/spin`.
	Spin []any

	// Routes are served on the HTTP listener beside the RPCs, by pattern: the
	// registry's `/v2/` first of all. Set after [Build] for the reason
	// [Server.Auth] is, and because what they are built from -- a store on a
	// disk -- is nothing a sandbox has.
	Routes map[string]http.Handler
}

// Build opens the database and stacks the servers.
//
// The two hooks are the whole of what payday puts in the write and read paths,
// and both come out of what the schema declared: [pd.Minter] stamps a new row
// with the domain of its entity and refuses one of another, [pd.Wall] narrows
// every read to the tenants the caller may see.
func Build(ctx context.Context, c Config) (*Server, error) {
	db, dialect, err := c.Db.Open(ctx)
	if err != nil {
		return nil, err
	}

	// The ent client is the app's to build, which is why config hands back a
	// *sql.DB and the dialect rather than a client: the client is generated
	// into this app from this app's schema, and payday has no name for it.
	drv := entsql.OpenDB(dialect, db)
	client := ent.NewClient(ent.Driver(drv))

	// The server that talks to the database, twice: once as it is, and once
	// with the wall on it.
	//
	// Two things are said to it rather than to the stack, and for the same
	// reason -- both are about the statement that runs. The trail is kept by
	// the servers that do the writing, since every RPC that changes anything
	// has to report itself from inside the transaction that changes it. The
	// wall is a predicate and a predicate belongs in the WHERE.
	b, err := c.Watch.Build(c.Db)
	if err != nil {
		db.Close()
		return nil, err
	}

	w := watch.New(b)

	// The recorders, in the order they are told. The trail is required -- a
	// write it could not account for is undone -- and the rest say for
	// themselves whether they are; see `bare.Recorders`.
	// Every write is told to these, in this order, inside the transaction that
	// makes it. It is one value rather than the option given three times
	// because `WithRecorder` refuses to be given twice: neither losing one nor
	// quietly appending in call order is a thing a framework should decide, so
	// the order is written here where it can be read.
	rec := bare.Recorders{pd.Recorder(), pd.WatchRecorder(w)}
	if c.Watch.Outbox {
		// Last, so that a queue this could not be written to undoes a write
		// which the trail and the in-process publish have already agreed to
		// rather than the other way round. It makes no difference to what is
		// committed -- either way nothing is -- and it makes the log read in
		// the order things were tried.
		rec = append(rec, pd.OutboxRecorder())
	}

	opts := []bare.Option{bare.WithMinter(pd.Minter()), bare.WithRecorder(rec)}

	sink, err := pd.NewSink(client, opts...)
	if err != nil {
		db.Close()
		return nil, err
	}

	walled, err := pd.NewSink(client, append(opts, bare.WithScope(pd.Wall()))...)
	if err != nil {
		db.Close()
		return nil, err
	}

	// The stack a caller reaches. `pd.Gate` is outermost, so nothing behind it
	// asks again.
	stacked, err := app.Build(walled.WithWatch(w), pd.AuditBuild(), pd.GateBuild())
	if err != nil {
		db.Close()
		return nil, err
	}

	// And the same servers with no wall and no gate, which is what the
	// deployment does its own work through. It is not a privilege anybody
	// holds: it is an instance somebody was handed, so going around the wall
	// is a line of wiring a reader can find rather than a rule that opens up
	// whenever nobody is asking.
	ungated, err := app.Build(sink.WithWatch(w), pd.AuditBuild())
	if err != nil {
		db.Close()
		return nil, err
	}

	s := &Server{Db: db, Ent: client, Drv: drv, Dialect: dialect, Watch: w, Walled: stacked, Ungated: ungated}
	if c.Watch.Outbox && b != nil {
		// The loop that makes an event durable. It is not a layer and not a
		// method on any server -- `spin.Run` finds it in whatever is handed
		// over, which is what keeps a server that has no background work from
		// carrying an empty method saying so.
		s.Spin = append(s.Spin, pd.Drain(client, b, c.Watch.Every()))
	}

	return s, nil
}

func (s *Server) Close() error { return s.Db.Close() }

// Grpc builds the server every call arrives at.
//
// The chain is payday's and the order is this app's to read: what records a
// call is outside everything, then the recovery, then the deadline a call that
// named none is given, then how often one caller may ask, then what is closed
// to callers entirely.
//
// It is separate from [Server.Serve] so that a test can travel exactly this
// and answer on a listener that is a channel; see pdtest.
func (s *Server) Grpc(ctx context.Context, c Config, opts ...grpc.ServerOption) (*grpc.Server, error) {
	// Who is calling comes first, since everything after it reads the frame.
	// `Plain` believes what the caller writes, which is right for a sandbox
	// and for tests and is not something to serve where anyone can reach it.
	//
	// [Server.Auth] replaces it, which is how this app reads something else --
	// a certificate, a token, a session cookie. It is a field rather than a
	// configuration setting for the reason payday suggests two deployments
	// rather than a flag: a mistake in a YAML file must not be able to turn
	// authentication off.
	h := s.Auth
	if h == nil {
		h = auth.Plain()
	}

	resolver := s.Resolver
	if resolver == nil {
		resolver = Resolver(s.Ungated)
	}

	chain := grpcx.Serving(ctx, grpcx.WithDeadline(c.Server.CallTimeout())).
		WithUnary(auth.InterceptorUnary(h, resolver, auth.PublicDefault)).
		WithStream(auth.InterceptorStream(h, resolver, auth.PublicDefault)).
		WithUnary(grpcx.LimitUnary(c.Server.Limiter(), gate.ByTenant())).
		With(gate.Interceptor(s.Policy)).
		With(s.Watch.Interceptor()).
		WithUnary(grpcx.ClosedUnary(c.Server.Closed()))

	os := append(opts, chain.ServerOptions()...)
	// A certificate that cannot be read is a server that must not start, which
	// is why this answers with an error: `GrpcOptions` reads the files
	// `server.tls` names.
	vs, err := c.Server.GrpcOptions()
	if err != nil {
		return nil, err
	}

	os = append(os, vs...)

	g := grpc.NewServer(os...)
	app.RegisterServer(g, s.Walled)

	// The batch, with the same rules the chain above enforces -- read off the
	// same configuration rather than written out again, which is the only way
	// the two stay in step. What they enforce by looking at the method gRPC
	// dispatched, this enforces per operation.
	if b, err := pd.Batch(s.Walled, s.Drv, c.Server.Guard(s.Policy)); err == nil {
		pdpb.RegisterBatchServiceServer(g, b)
	} else {
		// A deployment that closed nothing, limits nothing and has no policy.
		// It is a real thing to be and it is also what a guard nobody filled in
		// looks like, so the batch is not served rather than served open.
		log.From(ctx).WarnContext(ctx, "no batch", slog.String("why", err.Error()))
	}

	return g, nil
}

// Serve answers on `l` until the context is done.
func (s *Server) Serve(ctx context.Context, c Config, l net.Listener) error {
	g, err := s.Grpc(ctx, c)
	if err != nil {
		return err
	}

	stop, err := s.serveHttp(ctx, c, g)
	if err != nil {
		return err
	}
	defer stop()

	go func() {
		<-ctx.Done()
		g.GracefulStop()
	}()

	return g.Serve(l)
}

// serveHttp is the second listener, for whatever cannot speak gRPC -- which is
// every browser. Nothing is opened unless the configuration named an address.
//
// It is the **same** `g`: a page reaches the handlers a gRPC client reaches,
// through the interceptors a gRPC client goes through, behind the same wall.
// There is no second stack here for a rule to be missing from.
func (s *Server) serveHttp(ctx context.Context, c Config, g *grpc.Server) (func(), error) {
	if !c.Server.Http.Serves() {
		if len(s.Routes) > 0 {
			return nil, errors.New("server.http.addr is not set, and the registry is served on it")
		}
		return func() {}, nil
	}

	h, err := web.New(c.Server.Http, g)
	if err != nil {
		return nil, err
	}

	// Whatever this app serves over HTTP goes here, on the same mux and behind
	// the same cross-origin answer. Signing a browser in is the one to expect,
	// and `auth/authsession` is most of it: the endpoint, the cookie and its
	// attributes, the expiry and the store it is kept in. What it takes from
	// this app is a `Verify`, because the people are in this app's schema and
	// what checking their secret means is this app's to say.
	//
	//	h.Handle("POST /session", sessions.Serve(login))
	//
	// A gRPC path is `/<service>/<method>`, so an ordinary route cannot collide
	// with one -- and `ServeMux` panics rather than shadowing if one somehow
	// does.
	for pattern, route := range s.Routes {
		h.Handle(pattern, route)
	}

	l, err := net.Listen("tcp", c.Server.Http.Addr)
	if err != nil {
		return nil, err
	}

	// The requests' contexts carry what `ctx` carries -- the telemetry, the
	// logger -- and not its cancellation, which is `srv.Close`'s to do.
	base := context.WithoutCancel(ctx)
	srv := &http.Server{Handler: h, BaseContext: func(net.Listener) context.Context { return base }}
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.From(ctx).ErrorContext(ctx, "http", slog.String("err", err.Error()))
		}
	}()

	log.From(ctx).InfoContext(ctx, "http", slog.String("addr", l.Addr().String()))

	return func() { srv.Close() }, nil
}
