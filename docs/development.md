# Developing cr

cr is a [payday](https://github.com/lesomnus/payday) app. The management plane
-- its entities, servers, command line and TypeScript client -- is generated
from `proto/`, and the registry beside it is written by hand. `CLAUDE.md` is
the short version of this page; the rest of `docs/` is for people running cr.

## The code

| | |
| --- | --- |
| `oci/` | names, references, digests, manifest parsing, media types, the error envelope |
| `registry/` | the `/v2/`, `/v1/` and `/admin/` handlers, written against `flob.Stores` and `index.Index` |
| `blob/` | what wraps flob: the prefix router, the upstream store and cache for pull-through, the constant blobs, tag labels |
| `index/` | the `Index` port: `entindex` over the generated ent client, `memindex` in memory, `indextest` the suite both pass, `rebuild` |
| `auth/` | authenticators, bindings and tag rules, the token issuer; `entpolicy` reads rows, `roster` speaks to roster |
| `gc/` | the online collection and the sweep; `entruns` records the runs |
| `export/` | `cr export` |
| `httpx/` | instrumentation and the health handlers |
| `cmd/`, `cli/` | the configuration, the server's wiring, the command line |
| `wasm/` | the sandbox build |
| `proto/app/` | the entities: `Repository`, `Manifest`, `ManifestBlob`, `Tag`, `Binding`, `TagRule`, `GcRun` |
| `api/`, `internal/ent/`, `server/` | generated |
| `ts/` | the page, and its generated client |

**The registry does not go through payday's runtime.** payday's
authentication, wall and gate are gRPC interceptors, and a plain HTTP handler
receives none of them. The registry has credentials and a per-repository
authorization of its own, so `registry/` talks to `index.Index`, and `entindex`
uses the generated ent client directly. The management plane goes through the
whole stack, so a change to a binding is audited and watched like any payday
row.

**The registry's tables are payday entities too** -- `Repository`, `Manifest`,
`ManifestBlob`, `Tag` -- global, erased for good, and read-only over the API,
so that `pd gen` owns their schema and migrations and the page reads them like
any other entity.

## Testing

```sh
go test ./...                                                           # handlers on flob's memory store and memindex; entindex on SQLite
CR_TEST_POSTGRES=postgres://... go test -run TestPostgres ./index/...   # entindex on PostgreSQL, a schema per test
./scripts/conformance.sh                                                # the OCI conformance suite against the built binary
```

CI runs all three, `pd gen --check`, the page's build and the image's build.
`oidc-release.yml` and `oidc-sibling.yml` check OpenID Connect against
GitHub's own ID tokens on every push to main: the release workflow's push is
allowed and the sibling's is refused.

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

The generated messages live in `api/`, named by `option go_package` in every
`proto/app/*.proto`:

```proto
option go_package = "github.com/lesomnus/cr/api";   // -> api/binding.pb.go, api.Binding
```

Every entity has to name the same package -- an app is **one** Go package for
everything generated, since the ent schemas of two packages cannot have an edge
between them and the tenant wall is an edge -- and `pd gen` refuses rather than
generating an app whose wall has no edge to stand on. `internal/ent`,
`server/bare` and `server/pd` are named from the module root, not from the
messages.

## What to read first

`cmd/serve.go`. It is the management plane's stack written out -- which layers,
in which order, and which server the wall is on -- and it is deliberately not
hidden behind a `payday.Serve(cfg)`. `cli/registry.go` puts the registry beside
it, and `registry/registry.go` is the registry.

`proto/app/binding.proto` is the other half. The `(payday.entity)` option at the
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
and this app's schema names `Tenant` -- which does not exist until a
generation has copied payday's entities into `proto/payday/`. So run by hand it
fails on a tree nothing has generated yet, and there is exactly one moment it
can run: inside `pd gen`, between those two things. Once `buf.lock` has entries
in it they are yours, and nothing here moves them.

## Upgrading payday

```sh
go get -u github.com/lesomnus/payday
go tool pd gen .
```

payday owns some of this app's schema -- `Tenant`, `Holder`, `Audit`, `Outbox` --
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
  describes, printing the SQL that is missing. Writing that migration is yours --
  payday's entities and yours are in one database and one ent client, so payday
  cannot own it.

Neither says anything when it cannot: a `replace` to a checkout, a workspace, a
build with no version -- all of those are somebody developing, and a guard that
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
-- what puts it there is `Server.Ungated`, which is not a privilege anybody
holds but a server instance this process was handed. Running it twice is an
error rather than a no-op, because an `init` that quietly did nothing is one
somebody runs against the wrong deployment and believes.

The configuration is a file, then the environment over the top of it. It is read
on the **root** command, so it has happened whichever subcommand runs, and
`--config` names a file when the default is not wanted -- which is how one app
runs as two deployments. `cmd/config.go` and the structs it names are what a
deployment can be told; [operating.md](operating.md) and [access.md](access.md)
describe them for people running cr, and a field added here is added there.

### Building it for a deployment

```sh
docker buildx bake            # the image, for linux/amd64 and linux/arm64
docker buildx bake test       # vet and the tests, on a machine with no Go toolchain
go build -tags grpcnotrace -ldflags="-s -w" ./cmd/cr
```

`docker-bake.hcl` tags one build four ways -- `:$TAG` (`local` unless told
otherwise), `:r<run>`, `:YYMMDD` and `:YYMMDD-r<run>` -- and labels it with its
revision and version. CI's `image` job builds it on every pull request and
pushes nothing; on `main`, once every other job has passed, it pushes to
`ghcr.io/lesomnus/cr` with `TAG=edge`.

Neither flag of the `go build` is required and neither changes what the binary
does. `-s -w`
drops DWARF, which is most of what a Go binary weighs and is worth keeping in a
build somebody debugs with a profiler. `grpcnotrace` is gRPC's own tag: it drops
`golang.org/x/net/trace`, a ring buffer of recent RPCs served at
`/debug/requests` -- off unless `grpc.EnableTracing` is set, and reachable only
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
call, so the store knows which rows were drawn; when one of them changes --
from a write here, from another query, from a `Watch` the server is streaming --
everything drawing it re-renders, at once, with nothing joining them up.
`useCall` is the same trick backwards: the row a write answered with goes into
the store, and the lists over that entity are read again, because a create can
change what belongs in one and only the server knows which.

The store is opened for a **credential** and mirrored to IndexedDB under it, so
a reload draws the page it had rather than a spinner for it, and signing out
takes the copy with it. `ts/src/store.ts` is the whole of that, and deleting two
lines of it turns the mirror off.

The page signs in as `@tenant/holder` with `auth.Plain`, which takes the
caller's word for it. That is right for the sandbox and for tests, and it is
why the page is not yet one to point at a deployment: `cr serve` reads bearer
tokens from `management.tokens` and roster instead, and the page does not send
one.

React is a **peer** dependency of payday and an optional one. `payday/store` and
`payday/query` know nothing about it; `payday/react` is twenty lines of
`useSyncExternalStore` over them under five exports, and the same file for Vue
or Svelte is the same length.

## A browser

gRPC is not a protocol a page can speak -- not a library that is missing, frames
the platform does not let anything write -- so the HTTP listener translates:

```yaml
server:
  http:
    addr: ":5000"
    allow_web: true
    origins: ["http://localhost:5173"]
```

What answers there is the **same** server: the same interceptors, the same
credential, the same wall, speaking Connect and gRPC-Web instead. A Connect call
is a POST with a JSON body, so it is also what to reach for from a shell:

```sh
curl -sX POST http://localhost:5000/app.RepositoryService/List \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' -d '{}'
```

Under TLS it carries **native gRPC** as well -- HTTP/2 arrives by ALPN and a
`grpcurl` reaches it. The two listeners are a decision about the transport gRPC
brings, not about what is reachable where.

Whatever else this app serves over HTTP goes on the same mux, through
`Server.Routes` in `cmd/serve.go`; the registry, the token endpoints and the
health checks are put there by `cli/registry.go`. The cross-origin answer is
over the whole mux, so a route added there is reachable from the same page the
RPCs are.

`go tool pd gen --ts .` writes the messages and service descriptors into
`ts/gen`, along with `entities.ts` -- one declaration per entity, which is what
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
