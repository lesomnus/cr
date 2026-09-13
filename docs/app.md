# cr

A [payday](https://github.com/lesomnus/payday) app.

The generated messages live at the module root, which is why the binary is in
`cmd/cr/` rather than beside them: an app is **one** Go package for
everything generated -- the ent schemas of two packages cannot have an edge
between them, and the tenant wall is an edge.

To put them somewhere else, change `option go_package` in `proto/app/*.proto`
and regenerate. It is read rather than configured, so there is one place that
says it and it is a place you own:

```proto
option go_package = "github.com/lesomnus/cr/api";   // -> api/thing.pb.go, api.Thing
```

Everything else stays where it is -- `internal/ent`, `server/bare`, `server/pd`
are named from the module root, not from the messages. Every entity has to say
the same one, and `pd gen` refuses rather than generating an app whose wall has
no edge to stand on. What it will not do is rewrite the Go you wrote: the
imports in `cmd/` are yours, and moving the package is a compile error until you
follow it.

## What is where

Two rules, and between them they say whether a file is yours.

| | |
| --- | --- |
| `proto/app/*.proto` | **yours** -- the entities, and any service you write |
| `proto/ext/**` | **yours** -- overlays, merged into a generated contract |
| `proto/**/*_svc.g.proto` | generated: the contract of an entity |
| `proto/payday/` | generated in whole; payday's own entities, copied in |
| `internal/ent/`, `server/bare/`, `server/pd/` | generated |
| `*.g.go`, `*.pb.go` | generated, wherever `go_package` puts them |
| `ts/gen/` | generated in whole |
| everything else | yours |

So: **`.g` means a generator wrote it**, and `proto/payday/` is the one
directory where that is true of every file rather than of the ones marked.
Nothing else needs remembering; `pd gen` rewrites all of it and `pd gen --check`
says when a commit did not carry it.

The `.g` stops at the schema on purpose. A contract is `thing_svc.g.proto` and
the Go generated from it is `thing_svc.pb.go`, because by then the marker is
true of everything in sight and says nothing.

## What to read first

`cmd/serve.go`. It is the stack written out -- which layers, in which order, and
which server the wall is on -- and it is deliberately not hidden behind a
`payday.Serve(cfg)`. Everything else in `cmd/` is small.

`proto/app/thing.proto` is the other half. The `(payday.entity)` option at the
bottom of it is where the domain byte, the tenant wall, the `List` and the
`Watch` all come from.

## Generating

```sh
go tool pd gen .          # messages, servers, ent schema, layers
go tool pd gen --ts .     # and the TypeScript half
go tool pd doctor .       # what would go wrong before it does
```

`pd gen --check` regenerates and fails if anything moved, which is what CI runs:
a generated file that was not regenerated compiles perfectly and is wrong.

`pd gen` also pins the buf dependencies the first time, and that is not a
convenience. `buf dep update` compiles the workspace before it writes the lock,
and this app's schema names `Tenant` — which does not exist until a
generation has copied payday's entities into `proto/payday/`. So run by hand it
fails on a tree nothing has generated yet, and there is exactly one moment it
can run: inside `pd gen`, between those two things. Once `buf.lock` has entries
in it they are yours, and nothing here moves them.

## Upgrading payday

```sh
go get -u github.com/lesomnus/payday
go tool pd gen .
```

payday owns some of this app's schema — `Tenant`, `Holder`, `Audit`, `Outbox` —
so a field added to one of them there arrives in `internal/ent` here the next
time you generate. **Nothing about that is loud on its own.** It compiles, the
tests pass against a database the tests just created, and the first sign of
trouble is a column that is not there in the one handler that reads it.

So two things refuse rather than trusting anyone to remember:

- **`pd gen` that did not happen.** The generated `server/pd/pd.g.go` carries
  the payday it came out of, and `pd.NewSink` refuses a binary linking a
  different one. `pd gen --check` fails on it too, which is what turns "we
  upgraded and forgot" into a red build rather than a strange afternoon.
- **the migration that did not happen.** `serve` looks at the database before it
  answers anything and refuses one that is not the shape `internal/ent`
  describes, printing the SQL that is missing. Writing that migration is yours —
  payday's entities and yours are in one database and one ent client, so payday
  cannot own it.

Neither says anything when it cannot: a `replace` to a checkout, a workspace, a
build with no version — all of those are somebody developing, and a guard that
refused them would be a guard nobody could work under.

While developing, `db.migrate: true` swaps the second refusal for
`ent.Schema.Create`. That is a decision about who may alter tables and it should
be made on purpose, not left on.

## Adding an entity

```sh
go tool pd entity add --tenanted --watch Widget .
go tool pd entity list .
```

It picks a domain nothing else has and writes the tenancy out, which are the two
things that are cheap to get wrong here and expensive to find later.

## Running

```sh
go run ./cmd/cr init          # the first tenant, and somebody in it
go run ./cmd/cr config        # what this deployment is configured with
go run ./cmd/cr config env    # every variable it can be told through
go run ./cmd/cr serve
```

`init` is there because there is nowhere else it could be. A tenant is not put
up from inside one, so the first row of a deployment cannot arrive over the API
— what puts it there is `Server.Ungated`, which is not a privilege anybody
holds but a server instance this process was handed. Running it twice is an
error rather than a no-op, because an `init` that quietly did nothing is one
somebody runs against the wrong deployment and believes.

The configuration is a file, then the environment over the top of it. It is read
on the **root** command, so it has happened whichever subcommand runs, and
`--config` names a file when the default is not wanted — which is how one app
runs as two deployments. What an app can be told is what `cmd/config.go`
declares; nothing is documented separately, because a documented list goes out
of date on the commit that adds a field.

### Building it for a deployment

```sh
go build -tags grpcnotrace -ldflags="-s -w" ./cmd/cr
```

Neither is required and neither changes what the binary does. `-s -w` drops
DWARF, which is most of what a Go binary weighs and is worth keeping in a build
somebody debugs with a profiler. `grpcnotrace` is gRPC's own tag: it drops
`golang.org/x/net/trace`, a ring buffer of recent RPCs served at
`/debug/requests` — off unless `grpc.EnableTracing` is set, and reachable only
through a handler this app does not register. If you want it, leave the tag off
and register the handler; what payday gives you instead is OpenTelemetry, which
`cmd/config.go` already configures, and the trail, which records what a write
changed rather than that it happened.

A build flag rather than something payday decides, because it has to be: a
library cannot set the tags its consumers build with.

## The page

```sh
go tool pd gen --ts .
cd ts && npm install && npm run dev
```

`ts/` is a React app, and it is small enough to read in one sitting. What is
worth reading is what is **not** in it: nothing declares which query a write
invalidates, nothing pushes a new row into a list, and nothing tells the tenant
shown next to a row that it is the same tenant shown at the top of the page.

That falls out of the reads going through the framework. `useQuery` makes the
call, so the store knows which rows were drawn; when one of them changes —
from a write here, from another query, from a `Watch` the server is streaming —
everything drawing it re-renders, at once, with nothing joining them up.
`useCall` is the same trick backwards: the row a write answered with goes into
the store, and the lists over that entity are read again, because a create can
change what belongs in one and only the server knows which.

The store is opened for a **credential** and mirrored to IndexedDB under it, so
a reload draws the page it had rather than a spinner for it, and signing out
takes the copy with it. `ts/src/store.ts` is the whole of that, and deleting two
lines of it turns the mirror off.

React is a **peer** dependency of payday and an optional one. `payday/store` and
`payday/query` know nothing about it; `payday/react` is twenty lines of
`useSyncExternalStore` over them under five exports, and the same file for Vue
or Svelte is the same length.

## A browser

gRPC is not a protocol a page can speak — not a library that is missing, frames
the platform does not let anything write — so `cr.yaml` has a second
listener in it:

```yaml
server:
  http:
    addr: ":8080"
    allow_web: true
    origins: ["http://localhost:5173"]
```

What answers there is the **same** server: the same interceptors, the same
credential, the same wall, speaking Connect and gRPC-Web instead. A Connect call
is a POST with a JSON body, so it is also what to reach for from a shell:

```sh
curl -sX POST http://localhost:8080/app.ThingService/List \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' -d '{}'
```

Under TLS it carries **native gRPC** as well — HTTP/2 arrives by ALPN and a
`grpcurl` reaches it. The two listeners are a decision about the transport gRPC
brings, not about what is reachable where.

Whatever else this app serves over HTTP goes on the same mux, in `serve.go`:

```go
h, err := web.New(c.Server.Http, g)
h.Handle("POST /session", sessions.Serve(login))
```

That is where signing a browser in lands, and the page needs it: `auth.Plain`
takes the caller's word for who they are, which is right for a sandbox and for
tests and is not something to serve where anyone can reach it.
`auth/authsession` is the rest of it — the endpoint, the cookie and its
attributes, the expiry and the store — and what it asks of this app is a
`Verify`, since the people are in this app's schema and their secrets are
wherever this app keeps them. The cross-origin answer is over the whole mux, so
a route added here is reachable from the same page the RPCs are.

`go tool pd gen --ts .` writes the messages and service descriptors into
`ts/gen`, along with `entities.ts` — one declaration per entity, which is what
the local store is built from. Nothing there is behaviour, and nothing is
generated per service: `ts/src/client.ts` turns a descriptor into a client in one
line, for whatever does not want to go through the store.

## What is enforced, and why

Every one of these is refused at generation, and every one of them is something
that fails **quietly** when it is left out:

| | what it costs to forget |
| --- | --- |
| `domain:` | identifiers that say nothing about what they name |
| a domain twice | an identifier that lies about what it names |
| tenancy unsaid | every row outside the wall, with nothing failing |
| `watch:` with no version | a stale answer overwriting a fresh one, on the client |
| `watch:` with no `ref` filter | a stream that cannot say which rows it is about |
| an overlay on payday's own field number | `alias` quietly becoming whatever the overlay said |
| a list order not ending in the key | a page that repeats a row or skips one |

And two that are warnings rather than refusals, because a small table is a real
thing: a list order no index covers, and an alias that is not a name.
