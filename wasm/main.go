//go:build js && wasm

// Command wasm is this app served from inside the page it serves.
//
// It is the same server the process runs: the same generated services, the same
// stack, the same wall generated from the same schema. Two things differ, and
// both are one line -- the database is SQLite in a Web Worker instead of a file
// or a socket, and calls arrive over a message port instead of HTTP/2.
//
// What that buys is why it exists at all. A browser reload restarts the whole
// server: new instance, new database, nothing left over. Somebody working on
// the front end does not start a backend, does not migrate anything, and does
// not have to remember what state they left it in.
//
//	GOOS=js GOARCH=wasm go build -tags grpcnotrace -o ts/public/app.wasm ./wasm
//
// The tag is gRPC's own. It drops `golang.org/x/net/trace`, a ring buffer of
// recent RPCs served at `/debug/requests` by a handler an app registers itself
// -- off unless `grpc.EnableTracing` is set, never registered here, and never
// reached anyway, since a sandbox serves with `grpc-dgram` rather than
// `grpc.Server`. It renders its page with `html/template`, which is most of
// what the tag saves.
//
// # Why this is a second entry point and not a flag
//
// Nothing in payday's runtime is allowed to assume a file system, a listener or
// a network, and this file is what makes that a fact rather than an intention:
// it is built for a platform where none of the three exist. `cmd` is the other
// entry point, and the two assemble the same parts differently -- which is the
// whole reason the wiring is left visible in this repository rather than hidden
// behind a Serve(cfg).
package main

import (
	"context"
	"fmt"
	"log"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/transport/jsport"

	pdauth "github.com/lesomnus/payday/auth"
	"github.com/lesomnus/payday/config"
	"github.com/lesomnus/payday/gate"

	// SQLite in a worker of its own. The other driver runs the engine on
	// wazero, which is a wasm runtime written in Go, so here it would be wasm
	// inside wasm.
	_ "github.com/lesomnus/payday/config/dbsqlite3wasm"

	"github.com/opencontainers/go-digest"

	app "github.com/lesomnus/cr/api"
	"github.com/lesomnus/cr/cmd"
	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/index/entindex"
	"github.com/lesomnus/cr/internal/ent"
	entmigrate "github.com/lesomnus/cr/internal/ent/migrate"
)

func main() {
	ctx := context.Background()

	// Held in memory rather than in OPFS, which is the decision that makes a
	// reload a fresh server. A sandbox that remembered would be a sandbox
	// somebody has to clear.
	s, err := cmd.Build(ctx, cmd.Config{
		Db: config.DbConfig{
			Driver: "sqlite3-wasm",

			// The leading slash is load-bearing: `file:sandbox` is a relative
			// name to the memdb VFS, and a relative name is resolved per
			// connection, so the second one in the pool opens an empty database
			// of its own.
			Dsn: "file:/sandbox?vfs=memdb",

			// One connection, because there is one of it: the engine is a single
			// JS thread in a worker, so a second connection buys no parallelism
			// and costs the lock -- there is no WAL here, and the driver rejects
			// `_busy_timeout` because its busy handler would sleep on the thread
			// that has to deliver the other connection's COMMIT.
			//
			// Both of these read as a page bug rather than a database one: what
			// a browser making two calls at once sees is one of them refused
			// with `could not say who is calling`.
			MaxOpenConns: 1,
		},

		// Named, because payday refuses a deployment that leaves it unsaid --
		// `memory` is right for one replica and silently wrong for two, so the
		// answer has to be written rather than defaulted. Here it is right by
		// construction: there is exactly one of this server and it is inside
		// the page.
		Watch: config.WatchConfig{Broker: config.BrokerMemory},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	// The schema is created rather than migrated. In a process that would be
	// the wrong way round -- versioned migrations are what a deployment runs --
	// but there is no database here that outlives the page, so there is nothing
	// for a migration to move.
	// sql/no-client-schema omits Client.Schema, so use the generated migration
	// package with the same driver the server's client was built on.
	if err := entmigrate.NewSchema(s.Drv).Create(ctx); err != nil {
		log.Fatal(err)
	}

	// The first rows, through the server the wall was never installed on.
	//
	// A tenant cannot be put up from inside one -- the Gate layer refuses it to
	// everybody, which is the same answer a real deployment gives -- so a
	// sandbox that seeded nothing would be one where the first thing anybody
	// tries is refused, correctly, and there is no way round it from the page.
	if err := seed(ctx, s.Ungated); err != nil {
		log.Fatal(err)
	}
	if err := seedRegistry(ctx, s.Ent); err != nil {
		log.Fatal(err)
	}

	// A server that is not gRPC's, taking the same services.
	gw := jsport.NewGateway()

	// The same two interceptors the process serves with, because the stack
	// behind them is the same stack: `s.Walled` reads a frame and refuses a
	// request that has none, so a server registered without these answers
	// "who is asking?" to everything.
	//
	// `Plain` believes what the caller writes, which is what a sandbox is --
	// there is nobody else in the page to lie to. Replace it here and not in
	// `cmd`: the two entry points are separate on purpose.
	srv := drpc.NewServer(gw,
		drpc.ChainUnaryInterceptor(
			pdauth.InterceptorUnary(pdauth.Plain(), cmd.Resolver(s.Ungated), pdauth.PublicDefault),
			gate.Unary(s.Policy),
		),
		drpc.ChainStreamInterceptor(
			pdauth.InterceptorStream(pdauth.Plain(), cmd.Resolver(s.Ungated), pdauth.PublicDefault),
			gate.Stream(s.Policy),
		),
	)
	app.RegisterServer(srv, s.Walled)

	// Publishing the entry point is the readiness signal, so nothing may be
	// published before the registration above is done -- and it blocks, because
	// a main that returns takes the instance down and the page sees its calls
	// start failing.
	log.Fatal(gw.Serve(ctx, srv))
}

// seed puts a tenant and somebody in it, so the page has an app to look at.
//
// The same two rows `cr init` writes, and for the same reason: the page
// signs in as somebody, and there has to be a somebody to be.
func seed(ctx context.Context, s app.Server) error {
	t, err := s.Tenant().Add(ctx, app.TenantAddRequest_builder{
		Alias: "acme",
		Name:  "Acme",
	}.Build())
	if err != nil {
		return fmt.Errorf("the tenant: %w", err)
	}

	if _, err := s.Holder().Add(ctx, app.HolderAddRequest_builder{
		Tenant: app.TenantRef_builder{Id: t.GetId()}.Build(),
		Alias:  "admin",
	}.Build()); err != nil {
		return fmt.Errorf("the holder: %w", err)
	}

	return nil
}

// seedRegistry puts a repository with a tagged image and a signature of it,
// so the page has a registry to show.
//
// The rows go in the way a push writes them, through the index: the entity
// services refuse to add them, since a row with nothing in a store behind it
// is not something the API should be able to make. There is no store here at
// all -- the sandbox serves no /v2/ -- so these rows are pictures of a push.
func seedRegistry(ctx context.Context, client *ent.Client) error {
	ix := entindex.New(client)
	const repo = "library/hello"

	image := digest.FromString("image")
	signature := digest.FromString("signature")

	return ix.Tx(ctx, repo, func(tx index.Index) error {
		if _, err := tx.Repo().Ensure(ctx, repo); err != nil {
			return err
		}
		if _, err := tx.Repo().Update(ctx, repo, index.RepoPatch{Description: ptr("Hello from the sandbox")}); err != nil {
			return err
		}
		if err := tx.Manifest().Put(ctx, repo, index.Manifest{
			Digest:       image,
			MediaType:    "application/vnd.oci.image.manifest.v1+json",
			ArtifactType: "application/vnd.oci.image.config.v1+json",
			Size:         525,
		}, []digest.Digest{digest.FromString("config"), digest.FromString("layer")}); err != nil {
			return err
		}
		if err := tx.Manifest().Put(ctx, repo, index.Manifest{
			Digest:       signature,
			MediaType:    "application/vnd.oci.image.manifest.v1+json",
			ArtifactType: "application/vnd.dev.cosign.artifact.sig.v1+json",
			Subject:      image,
			Size:         710,
		}, nil); err != nil {
			return err
		}
		return tx.Tag().Set(ctx, repo, "latest", image, "")
	})
}

func ptr[T any](v T) *T { return &v }
