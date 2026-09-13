# Implementation plan

Status: draft, 2026-09-13, revised the same day after flob and payday closed
the issues in §8. Follows [issue #1](https://github.com/lesomnus/cr/issues/1),
which settles the storage model. This document settles what is built on top of
it and in what order. Facts about flob, payday, roster, go-app and the ent fork
below were read from their sources on 2026-09-13; file references point into
those repos.

## The principle

cr exists because a registry took a store-wide exclusive lock on every blob
`HEAD` and made clients wait nineteen seconds for disks that were idle
([#1](https://github.com/lesomnus/cr/issues/1)). So the rule that outranks
the others is **availability over strong consistency**, the same trade flob
makes and states in its README:

- A request path never waits on anything store-wide. Blob `HEAD`, `GET`,
  `PUT` and every read touch flob and nothing else; they take no lock and
  open no transaction.
- **Leaks are tolerated and swept later; losses are not.** Every online
  reclamation step is "remove a reference the index no longer has", so a
  race leaves bytes behind and never takes away bytes something still
  points to.
- The one serialization is manifest writes within one repository, and it
  is a transaction-scoped database lock with no I/O inside it (§3). It
  exists because the alternative is a loss, not a leak. Nothing else is
  serialized, and GC runs per repository under that same lock so no other
  repository notices (§7).
- Bookkeeping that would put a write on a read path, such as last-pull
  times, is batched and asynchronous, and losing a batch is acceptable.
- A design that needs a store-wide lock, a global read-only window, or a
  pass over every repository before it can answer is rejected on that
  ground alone.

## 0. Decisions

| question | decision | why |
| --- | --- | --- |
| scope | the whole distribution-spec v1.1 surface, `_catalog`, `/v1/search`, a token endpoint, and a management plane: per-repository permissions, fine-grained actions, tag rules | §1, §5, §6 |
| the `/v2/` core | hand-written `net/http` against two ports, `flob.Stores` and `index.Index`; no framework on the hot path | byte streaming over REST; nothing here is an entity anything could generate |
| the management plane | **payday**, in the same binary, on the same database; the `/v2/` handler mounts on payday's `web.Mux`. Decided on 2026-09-13 after the checks in §2. | permissions, tag rules and repository metadata are CRUD with List, Watch, an audit trail and a TypeScript client, which is what payday generates; and the sandbox runs the whole server in the browser, so the management page is developed and demonstrated with no backend. §2 says what the hot path does and does not take from it. |
| ORM | the ent fork at `github.com/lesomnus/ent` (module path `github.com/protobuf-orm/ent`), which is what payday generates against | it keeps Atlas: `dialect/sql/schema/atlas.go` and `versioned.go` drive versioned migrations through atlas's executor. Nothing to add for migrations. sqlite, postgres and mysql remain; gremlin is gone. |
| users | **cr stores no credentials.** Subjects come from authenticators; roster is one of them and needs no change | per-repository authorization is cr's own. roster refuses it on purpose (`docs/position.md`: "repositories are the product's"). |
| tokens | cr's own token endpoint, JWT signed by cr, JWKS published | the distribution flow needs an `access` claim; roster never issues anything a third party verifies |
| GC | two tiers: online retention that may leak but never loses, and a mark-and-sweep that runs one repository at a time under that repository's write lock | no global read-only window; enumeration is flob's `Walker` and `Namespacer` (`store.go:154-197`), and `Erase` is namespace-local so a sweep is too |
| pull-through | flob's `NewCacheStores(primary, origin)` in front of an OCI-origin `Store` that cr writes; the handler serves with `http.ServeContent` | the tap now survives the `ServeContent` seek probe and concurrent misses share one fill (flob `7b0208c`, `cfe82fb`). cr's part is the origin `Store`. |

## 1. The endpoint surface

| id | method | path | needs |
| --- | --- | --- | --- |
| end-1 | GET | `/v2/` | auth only |
| end-2 | GET, HEAD | `/v2/<name>/blobs/<digest>` | flob `Stat` / `Open`; `Range`; 307 via `Presigner` |
| end-3 | GET, HEAD | `/v2/<name>/manifests/<reference>` | index (tag → digest), flob `Open`; `Accept` negotiation |
| end-4a | POST | `/v2/<name>/blobs/uploads/` | `Stager.Begin`; the stage id is the upload reference |
| end-4b | POST | `/v2/<name>/blobs/uploads/?digest=` | flob `Add` with expected digest, streaming body |
| end-5 | PATCH | `/v2/<name>/blobs/uploads/<ref>` | `Stage.Append(expectedOffset)`; `Content-Range` → `416` on `ErrOffsetMismatch` |
| end-6 | PUT | `/v2/<name>/blobs/uploads/<ref>?digest=` | optional last `Append`, then `Stage.Commit(Meta{Digest})` |
| end-7 | PUT | `/v2/<name>/manifests/<reference>` | parse, validate, tag rules, flob `Add`, index txn; `OCI-Subject` |
| end-8a/b | GET | `/v2/<name>/tags/list[?n=&last=]` | index; `Link` header |
| end-9 | DELETE | `/v2/<name>/manifests/<reference>` | tag rules, index txn, release layers, flob `Erase` |
| end-10 | DELETE | `/v2/<name>/blobs/<digest>` | index (referenced?), flob `Erase` |
| end-11 | POST | `/v2/<name>/blobs/uploads/?mount=&from=` | authz `pull` on `from`; `AsLinker` → `Link`, else `Open`+`Add` |
| end-12a/b | GET | `/v2/<name>/referrers/<digest>[?artifactType=]` | index; `OCI-Filters-Applied` |
| end-13 | GET | `/v2/<name>/blobs/uploads/<ref>` | `Stage.Stat` → `Range: 0-<offset-1>` |
| — | DELETE | `/v2/<name>/blobs/uploads/<ref>` | `Stage.Abort`; distribution API, not in the OCI table |
| — | GET | `/v2/_catalog[?n=&last=]` | index, filtered by what the subject may pull; not in the spec but `crane`, `skopeo` and registry-ui use it |
| — | GET | `/v1/_ping`, `/v1/search?q=&n=` | §6 |
| — | GET | `/token` | §5 |
| — | GET | `/.well-known/jwks.json` | §5 |
| — | Connect | `/cr.RepositoryService/*`, `/cr.BindingService/*`, `/cr.TagRuleService/*`, … | the management plane, generated |

Cross-cutting: the JSON error envelope and every code the spec names
(`BLOB_UNKNOWN`, `BLOB_UPLOAD_INVALID`, `BLOB_UPLOAD_UNKNOWN`, `DIGEST_INVALID`,
`MANIFEST_BLOB_UNKNOWN`, `MANIFEST_INVALID`, `MANIFEST_UNKNOWN`, `NAME_INVALID`,
`NAME_UNKNOWN`, `SIZE_INVALID`, `UNAUTHORIZED`, `DENIED`, `UNSUPPORTED`,
`TOOMANYREQUESTS`); `Docker-Content-Digest` on every blob and manifest
response; `OCI-Chunk-Min-Length` on end-4a.

Digests: flob hashes with the algorithm of a supplied `Meta.Digest`, sha256,
sha384 or sha512 (`os.go:84-92`, flob `9821686`); anything else is
`DIGEST_INVALID`. The conformance suite uses sha256 only.

## 2. Layout, and where payday stops

```
proto/                    payday entities: Repository, Binding, TagRule, and the hot tables (§3)
internal/ent/             generated by pd gen, never edited
server/…                  generated CRUD, gate, audit; cr's own layers beside them
cmd/, cli/                the payday shape; cr serve, cr gc, cr <entity> add|ls|…
oci/                      names, references, digests, manifest parsing, media types, the error envelope
registry/                 the /v2 and /v1 handlers, written against the ports and nothing concrete
index/                    the Index port and its record types
index/entindex/           the port implemented over the generated ent client
blob/                     what wraps flob: name→id, the prefix router, the OCI-origin Store
auth/                     Authenticator, Authorizer, TagPolicy, the token issuer
auth/htpasswd, auth/static, auth/oidc, auth/roster
gc/
ts/                       the generated client; registry-ui's management side later
```

**The hot path does not go through payday's runtime.** payday's auth, gate,
wall and audit are gRPC interceptors and generated server layers; a plain
handler on `web.Mux` receives none of them (`web/web.go:78`, `auth/plain.go:55`).
That is fine, because `/v2/` has its own credential shapes, Basic and the
distribution JWT, and its own authorization, per repository, which payday's
gate cannot express. So `registry/` talks to `index.Index`, and `entindex`
talks to the generated `*ent.Client` directly, with no frame. The management
plane goes through the full stack, so a change to a `Binding` is audited and
watched like any payday row.

What the hot path takes from payday: the schema and migrations, the ent
client, config, telemetry, TLS and the listeners. What it does not: identity
per request, the wall, the gate.

**Every table is a payday entity, including the hot ones.** Verified on
2026-09-13 by running `pd new`, `pd gen` and `go build` on a throwaway app:

- `pd gen` removes `internal/ent/schema` wholesale on every run
  (`internal/pdcli/gen.go:486-494`), so a hand-written ent schema beside the
  generated ones is silently deleted, and `pd gen --check` reports it as an
  orphan. A second ent tree outside payday's layout does survive, but it gets
  no migration, no drift check and no edges to the rest. Not worth it.
- A `global: {}` entity with `erase: {hard: {}}`, a UUID key and a unique
  index on its natural key generates and builds; `TagScope` comes out as
  "declared global, so it is not behind the wall at all". The cost is one UUID
  column per row. A string primary key also passes generation but bypasses the
  minter and the slug paths, so it is outside the design; a composite key is
  refused. UUID key plus `indexes: {unique (repo, name)}` is the shape.
- Field numbers 1–7 and 13–15 are payday's (`docs/guide/schema.md:286-322`):
  `name` must be 5, and `repo`, `digest` and the rest start at 8.
- A plain handler on `web.Mux` may use `*ent.Client` with no frame in context.
  No generated privacy policy, hook or interceptor refuses it; the wall is a
  predicate on `bare.Store.Scope`, and the gate is a gRPC interceptor. Checked
  with a `/v2/<name>/tags/list` handler that read and wrote a `Tag` row.
- The sandbox exposes what is registered as a gRPC service and nothing else;
  `wasm/main.go` builds no `http.ServeMux`. So `/v2/` is invisible there, by
  design, and the management page browses repositories and tags through their
  entity services instead, which is one more reason the hot tables are
  entities. At payday `ae2bf9a` the sandbox template did not compile
  ([payday#17](https://github.com/lesomnus/payday/issues/17),
  [payday#18](https://github.com/lesomnus/payday/issues/18)); fixed in
  `d571a79`, which also builds the rendered template in CI, and re-verified
  here on `5fb4c99`: `pd new`, `pd gen`, `pd sandbox init`, then the wasm and
  native builds all pass.
- payday needs Go 1.27 (`go.work`), for the standard library `uuid` package.

**Tenancy.** payday's wall is per `Tenant`. Tenant = the owner of a
namespace prefix (`acme/*`), which is also Docker Hub's shape and Harbor's
"project", with one operator tenant owning unprefixed names. Repository,
Binding and TagRule sit behind that wall. With roster on, cr's tenants
mirror roster's: the `tenant_id` that `Introspect` returns is the row, and
members, teams and robots are roster's, not cr's. Without roster there is
the operator tenant and nothing to mirror. Phase 3; phases 0–2 do not
touch it.

### The name → flob id mapping

flob now encodes every namespace id into one path segment itself
(`namespace.go:11-16`, flob `052f489`): an id made of `[A-Za-z0-9_.-]` is
used as is, anything else, including a `/`, becomes `~` plus base64url. So cr
passes the repository name straight to `Use` and keeps no mapping. cr still
validates `<name>` against the spec grammar, because `NAME_INVALID` is the
answer the spec wants and flob would otherwise accept anything.

## 3. The index

Derived data over immutable content, authoritative for names and policy. Blob
`HEAD`, `GET` and `PUT` never touch it. That is what keeps zot's failure
structurally impossible here.

```go
// Index is the registry's view of names over the content flob holds. One
// accessor per resource, the way payday's Server has one per entity, and
// the only thing the /v2 handler talks to besides flob.
type Index interface {
	Repo() Repos
	Manifest() Manifests
	Tag() Tags
	// Tx runs fn inside one transaction. The Index handed to fn is the one
	// fn uses; the outer one is not touched until fn returns.
	Tx(ctx context.Context, fn func(Index) error) error
}

// Page is (last, n) as the spec paginates: names after Last, at most N.
type Page struct {
	Last string
	N    int
}

type Repos interface {
	// Ensure returns the repository, creating it on first use. Called from
	// the first manifest put, never from a blob.
	Ensure(ctx context.Context, name string) (Repo, error)
	Get(ctx context.Context, name string) (Repo, error)
	// Update changes what the management plane owns: description, visibility.
	Update(ctx context.Context, name string, patch RepoPatch) (Repo, error)
	// List is _catalog: lexical order, after p.Last.
	List(ctx context.Context, p Page) ([]string, error)
	// Search is /v1/search: name or description containing q, at most n.
	Search(ctx context.Context, q string, n int) ([]RepoSummary, error)
	// Erase removes the repository and every row under it. The caller has
	// already walked Manifest().Marks and erased the blobs from flob.
	Erase(ctx context.Context, name string) error
}

type Manifests interface {
	// Put records a manifest and the blobs it holds: layers, config, and
	// child manifests of an index. Idempotent on (repo, digest).
	Put(ctx context.Context, repo string, m Manifest, holds []Digest) error
	Get(ctx context.Context, repo string, d Digest) (Manifest, error)
	// Erase removes the manifest and returns the blobs no other manifest in
	// the repository still holds, for the caller to erase from flob.
	Erase(ctx context.Context, repo string, d Digest) (released []Digest, err error)
	// Referrers is end-12: descriptors of the manifests whose subject is d,
	// narrowed by artifactType when it is not "", built from the row alone.
	Referrers(ctx context.Context, repo string, subject Digest, artifactType string) ([]Descriptor, error)
	// Holds answers blob DELETE: does any manifest in repo still need d?
	Holds(ctx context.Context, repo string, blob Digest) (bool, error)
	// Untagged is GC input: manifests no tag points at, older than before,
	// and not held by an index or named as a subject.
	Untagged(ctx context.Context, repo string, before time.Time) ([]Digest, error)
	// Marks is everything the index believes repo holds, manifests and the
	// blobs they hold: the mark phase of the sweep, and repository deletion.
	Marks(ctx context.Context, repo string) iter.Seq2[Digest, error]
}

type Tags interface {
	// Set points name at d. from is what the caller resolved a moment ago,
	// "" for a tag it believes is new; a tag that moved in between fails
	// with ErrTagMoved, so an immutable or protected tag is checked and
	// written under the same row lock rather than in two steps.
	Set(ctx context.Context, repo, name string, d, from Digest) error
	Resolve(ctx context.Context, repo, name string) (Digest, error)
	Erase(ctx context.Context, repo, name string) error
	// List is end-8: lexical order, after p.Last.
	List(ctx context.Context, repo string, p Page) ([]string, error)
	// Of lists the tags pointing at d, for GC and for the management page.
	Of(ctx context.Context, repo string, d Digest) ([]string, error)
}
```

Verbs: `Put` where the row is named by its content and writing it twice is
the same as once; `Set` where it is a pointer; `Erase` because that is the
word flob and payday both use; `Get`, `List`, `Search`, `Resolve` as they
read. Bindings and tag rules are not here on purpose: they are policy, read
through `Authorizer` and `TagPolicy` (§5), whose ent implementations share
the client but not the port.

Record types are plain structs: `Repo{Name, Description, Visibility,
Tenant}`, `Manifest{Digest, MediaType, ArtifactType, Subject, Size,
Annotations, CreatedAt}`, `RepoSummary{Name, Description}`, and
`Descriptor` is the image-spec one. `entindex` implements `Tx` with the
fork's shared `dialect.Tx` and rebinds each accessor onto it, which is what
the generated clients' `WithDriver` exists for.

Tables:

```
repositories   (name, description, tenant, visibility)
manifests      (repo, digest, media_type, artifact_type, subject, size, annotations, created_at)
                 index (repo, subject)               ← referrers is one range scan
manifest_blobs (repo, manifest_digest, blob_digest)   ← what a manifest holds; what a delete releases
tags           (repo, name, digest, updated_at)       pk (repo, name)
bindings       (subject | group, repo_pattern, actions)          §5
tag_rules      (repo_pattern, tag_pattern, kind, params)         §5
```

`manifest_blobs` is what makes deletion local: deleting a manifest releases
each layer no other manifest in the repository still holds, cr calls `Erase`,
and `nlink` does the rest. It is a cheap table, and with GC relaxed (§7) it is
what keeps "delete a repository and the space comes back now" true. Referrers
descriptors are served from the row without opening manifests. Both list
endpoints paginate by `(last, n)`; the fork's `sqlpage` keyset cursors fit.

**Ordering.** Write: flob first, then the index transaction; a crash between
leaves an untagged manifest, which retention covers. Delete: index first, then
`Erase`; the other order can leave a tag pointing at nothing.

**Manifest writes are serialized per repository.** Without that, a delete
of manifest M that releases layer L can commit before a concurrent put of
manifest N that holds L, and then erase L from flob after N is indexed: a
loss, not a leak. Deferring the erase or re-checking after commit only
narrows that window; a lock closes it. `Tx` for `Manifest().Put` and
`Manifest().Erase` takes `pg_advisory_xact_lock(hash(repo))` on Postgres;
SQLite is one writer anyway. This is the only lock in cr and it is not
zot's: one repository, one transaction, no I/O inside, manifest writes
only, never a read and never a blob.

**Rebuild.** `cr index rebuild` walks every namespace with flob's
`Namespacer` and `Walker`, re-parses each manifest, and recreates the rows.
Tags are the one thing a walk cannot recover unless they are also written as
a flob label on the manifest in its namespace, which is cheap and done from
phase 1: `Tag: <name>` as one value per tag, replaced on every move.

## 4. Blobs

- **HEAD** = flob `Stat`, which returns an `Info` with digest and size and
  loads labels only if asked (`info.go:12-16`). One stat on `os`.
- **GET** = flob `Open`, served with `http.ServeContent` for `Range` and
  conditionals. `OsStore` hands back the `*os.File`; `S3Store` and
  `HttpStore` now read lazily through ranged requests (flob `f4b3ef8`), so
  proxying from S3 is viable, and if `AsPresigner` succeeds cr answers 307
  with `Location` and `Docker-Content-Digest` instead. `CacheStore` and
  `FallbackStore` unwrap to their primary, so the capability is visible
  through a cache; a miss there is `ErrNotExist` from `PresignOpen` and falls
  back to streaming. `S3Config.PublicEndpoint` is where a CDN or public
  hostname goes. Only blob `GET` redirects: `HEAD` is answered from `Stat`,
  and manifests always stream, because clients expect their media type as
  `Content-Type` and the digest header beside the body.
- **Monolithic** = `Add(ctx, Meta{Digest: d}, body)`; flob verifies and commits
  atomically. When the digest already exists in the namespace, `Add` returns
  before reading the body (`os.go:69-83`); cr drains the request or the
  connection stalls.
- **Chunked** = flob's `Stager` (`store.go:200-300`, flob `69f0a5a`). cr
  keeps no upload state at all: `POST` is `Begin(algo)` and the stage id is
  the `<reference>` in `Location`; `PATCH` is `Append(expectedOffset, body)`
  with `ErrOffsetMismatch` answered as `416` and the current offset in
  `Range`; `GET` is `Stage.Stat`; `PUT ?digest=` is `Commit(Meta{Digest})`,
  which verifies against the hash flob kept incrementally; `DELETE` is
  `Abort`. Stage ids are scoped to the namespace, so the repository in the URL
  is the authorization boundary. Stages expire on flob's `StageConfig.TTL`
  (24 h by default) and `serve` calls `StageCleaner.PruneStages` on its ticker,
  since flob starts no sweeper of its own. The hash checkpoint makes this
  resumable across a restart on `os` and S3.
- **Mount** = `AsLinker(store).Link(ctx, d, from)` (`store.go:107-123`, flob
  `2a9903b`): one link on `os`, one marker on S3, no bytes read. `ErrNotExist`
  from the source means "fall back to a normal upload", which the spec
  provides for. `Open`+`Add` remains the path when the two repositories sit
  on different backing pools (`ErrIncompatibleStore`), which the prefix router
  can produce. Authorize `pull` on `from` first.
- **Pull-through** = `flob.NewCacheStores(primary, origin)` with
  `blob.OciOrigin`, a `flob.Store` over a remote registry's blob API: `Stat`
  is a `HEAD`, `Open` is a `GET`, the writes return `ErrUnimplemented`. The
  first caller streams while the primary fills, later callers for the same
  digest wait for that fill instead of hitting upstream, and the tap survives
  `ServeContent`'s seek probe, so the blob handler is the same code with and
  without a cache. It is on demand at every level: a tag fetch brings the
  index alone, the client's next request brings that platform's manifest,
  and only its layers follow. Nothing pre-fetches the other platforms. For a
  proxied repository the manifest handler therefore skips the "referenced
  blobs must exist" check, `Manifest().Put` may record `holds` that are not
  local, and GC never tries to erase what was never fetched. Tags carry a
  TTL and are revalidated upstream with a `HEAD`; digests are never
  revalidated. Cache capacity is a `retention` by last pull, which is why
  manifest `GET` records `last_pulled_at`, batched and asynchronous.
- **Blob DELETE.** The spec allows `405 UNSUPPORTED` or a delete. cr deletes
  when `Manifest().Holds` is false and answers `DENIED` when a manifest in the
  repository still holds the blob; deleting it would make that manifest
  unpullable, which is worse than refusing.
- **Well-known blobs** never reach flob. A handful of digests name constant
  content and are asked for constantly: `{}` (`sha256:44136fa3…`, 2 bytes,
  the OCI 1.1 empty descriptor that every cosign signature, attestation and
  `oras` artifact uses as its config), Docker's empty layer
  (`sha256:a3ed95ca…`, 32 bytes of gzip) and its uncompressed form
  (`sha256:5f70bf18…`, 1024 bytes), and the empty blob (`sha256:e3b0c442…`).
  cr keeps them in a table in code: `HEAD` and `GET` answer from memory after
  the usual authorization, the manifest validator counts them as present,
  mount and `DELETE` are no-ops, and `Manifest().Put` still records them in
  `holds` so `Holds`, `Marks` and rebuild need no special case. Since clients
  `HEAD` before they upload, these are rarely even pushed. Observed on the
  zot deployment in #1 as a surprisingly large share of blob requests.
- **Errors.** flob's errors map onto the spec's envelope like this. The
  spec's `BLOB_UPLOAD_UNKNOWN` is its name for "no such upload session", not
  for an unclassified failure; which of the three it was goes in `detail`.

  | flob | when | answer |
  | --- | --- | --- |
  | `ErrOffsetMismatch` | `PATCH` at an offset other than the current one | `416`, `Range: 0-<offset-1>` |
  | `ErrStageExpired`, `ErrStageClosed`, `ErrNotExist` from `Resume` | the session expired, was aborted or committed, or never existed | `404 BLOB_UPLOAD_UNKNOWN` |
  | `ErrStageConflict` | a commit retried with different parameters, or concurrent writers | `400 BLOB_UPLOAD_INVALID` |
  | `ErrDigestMismatch` | the declared digest is not what was appended | `400 DIGEST_INVALID` |
  | `ErrInvalidDigest` | unparsable digest or unsupported algorithm | `400 DIGEST_INVALID` |
  | `ErrStageFormat` | the stage record on disk is corrupt | `500` |
  | `ErrNotExist` on a blob | `HEAD`/`GET` of a blob the repository lacks | `404 BLOB_UNKNOWN` |
  | `ErrNotExist` from `Link` | the mount source lacks the blob | `202`, fall back to a normal upload |
  | `ErrIncompatibleStore` | the router put `from` on another pool | not an answer; `Open`+`Add` instead |
  | `ErrAlreadyExists` | a blob pushed again | success, `201` |

- **Placement.** `blob.Router` implements `flob.Stores`, longest-prefix on the
  repository name to one of several backing `Stores`. zot's `SubPaths`; closes
  that question in #1.

## 5. Auth and policy

Enforcement is a layer between the handler and the ports; storage and editing
are the management plane. Three interfaces:

```go
type Authenticator interface {
	Authenticate(ctx, username, password string) (Subject, error)   // Subject: id, groups
}
type Authorizer interface {
	Allow(ctx, s Subject, repo string, actions []Action) ([]Action, error)
}
type TagPolicy interface {
	Check(ctx, repo, tag string, op TagOp, current, next Digest) error   // create, move, delete
}
```

**Actions.** The distribution `scope` grammar carries `pull`, `push`,
`delete` and `*`. cr issues and verifies its own tokens, so the `access` claim
may carry more: `pull`, `push`, `delete`, `tag` (create or move a tag; `push`
without it is push-by-digest only), `mount` (implies `pull` on the source),
`catalog`, `search`, `admin`. A `docker push` asks for `pull,push` and gets
what the bindings allow; the response tells the client which subset it got,
as the spec provides.

**Bindings.** `(subject or group) × repository glob × actions`, union only, no
deny. Globs: `acme/*`, `acme/app`, `*`. Anonymous is a subject like any other
(`anonymous`), so public pull is a binding. Groups are whatever the
authenticator reports: roster teams, OIDC `groups`, or names from config.

A binding may also carry `when`, a set of claim conditions that must all
hold, values as globs. That is what lets one GitHub Actions workflow, and no
other, push a repository:

```yaml
- repo: acme/app
  actions: [pull, push, tag]
  when:
    iss: https://token.actions.githubusercontent.com
    repository: acme/app
    workflow_ref: acme/app/.github/workflows/release.yml@refs/heads/main
```

The job asks GitHub for an ID token with cr's hostname as audience and runs
`docker login -u oidc -p "$ID_TOKEN"`; cr verifies it against GitHub's
JWKS, offline. GitHub's token lives minutes and the Docker CLI replays the
stored password on every push, so long jobs use the exchange first:
`POST /token/exchange` with the ID token returns a cr-issued token bound to
the same claims for a configurable lifetime, and that is what goes into
`docker login`. This is Fulcio's and AWS's trust-policy shape applied to a
registry, and it is the answer to CI push, not anonymous push.

**Tag rules.** Matched by repository glob and tag pattern, checked on end-7
and end-9 before anything is written, and read by GC:

| kind | means |
| --- | --- |
| `immutable` | once set, the tag may not move or be deleted |
| `protected` | moving or deleting needs the `admin` action, or a named group |
| `pattern` | the tag must match a regex, e.g. semver, or the push is `DENIED` |
| `retention` | keep the newest N matching tags; older ones are GC input, not a push-time check |

**The token endpoint.** `GET /token?service=&scope=…` with Basic credentials
runs the authenticator chain, narrows every requested scope through
`Authorizer.Allow`, signs ES256 with keys from config, and publishes JWKS.
Bearer verification on `/v2/` is offline. `/v2/` also accepts Basic directly,
since many private deployments never run the token flow.

**Authenticators**, in order of arrival:

| | verifies | subject and groups |
| --- | --- | --- |
| `htpasswd` | a bcrypt file, reloaded on change | username; groups from config |
| `static` | long-lived tokens in config, for CI **without roster** | the token's name |
| `oidc` | a JWT pasted as the password: issuer, audience, signature | `sub`, `groups`, and every claim as a `name=value` group |
| `roster` | `rt_` keys via `payday.TokenService/Introspect`; passwords via `roster.VouchService/Verify`; teams via `HolderService/Reaches` | `Holder.id`, tenant, team ids |

roster's `Verify` answers `ok=false` plus a continuation when the holder has a
second factor, and a Docker password prompt cannot carry a TOTP; roster's own
answer for LDAP applies, the person pastes an `rt_` app password. Since roster
`2be81fb` that key can come from the terminal itself: `roster sign-in --name
docker --out …` runs the device grant (RFC 8628) against the account app, and
the approved `rt_` needs no methods at all, because cr asks about it with its
own `rk_`. The deployment has to turn on `account.terminal`. cr's `rk_`
needs `/roster.VouchService/Verify`, `/payday.TokenService/Introspect`,
`/roster.HolderService/Reaches`, `/roster.SyncService/Watch`, the last to drop
cached decisions when a holder is disabled. **No roster change is needed for
any of this**, and the two things that would shorten cr's list, minting the
registry JWT and holding per-repository permissions, are both things roster
refuses by design; moving that line would cost roster its reason to exist.

**Robots and projects are roster's.** A CI robot is a roster holder with
no credential and an `rt_` key (`roster holder add @acme/ci`, `roster key
add --tenant acme --holder ci`): printed once, `date_expires`, `date_used`,
narrowing, revocation by deletion, audited. cr has no Robot entity; what a
robot may do in cr is a binding on its holder id, as for anyone. A project
is a roster tenant with its teams and groups; cr adds only the bindings,
tag rules and, later, quotas. So the management plane is `Repository`,
`Binding` and `TagRule`, and there are two modes rather than two copies:
without roster, htpasswd users and `static` tokens and one tenant; with
roster, people, robots and tenants come from it.

The management plane's own authentication is payday's: `auth.Bearer` over a
`TokenStore`, or `auth.Remote` to roster, or sessions for a browser. The
`docker login` path and the console path share subjects and bindings, not
credential shapes.

## 6. Search

`docker search cr.example/term` is answered by the registry named in the term:
moby splits at the first `/` when the left part has a `.` or `:`
(`daemon/pkg/registry/search.go`, `splitReposSearchTerm`), pings
`GET /v1/_ping`, then calls

```
GET /v1/search?q=<term>&n=<limit>          n ∈ [1, 100], default 25
X-Docker-Token: true
```

with Basic when logged in, or a token with scope `registry:catalog:search`
when the login left an identity token. The response is

```json
{"query":"term","num_results":2,"results":[
  {"name":"acme/app","description":"…","star_count":0,"is_official":false,"is_automated":false}]}
```

The CLI filters `is-official`, `is-automated` and `stars` on its side. That is
also the shape registry-ui's client speaks as `ext.search.V1`
(`src/search.ts`), so cr answering it makes the UI's search box work with no
UI change.

cr: `Repo().Search` is a `LIKE` on name and description, filtered by what
the subject may `pull`, capped at `n`. `description` is a `Repository` field
set through the management plane, and falls back to the
`org.opencontainers.image.description` annotation of the manifest the newest
tag points at. Full-text comes if `LIKE` ever hurts; at 226 repositories it
does not.

## 7. GC

Not strongly consistent, by decision. Two tiers.

**Online, always running, never loses data.** Expired stages, through
`PruneStages`; untagged manifests past the retention window, per repository, unless an index
or a referrer's `subject` relationship holds them; `retention` tag rules.
Deleting a manifest releases its layers through `manifest_blobs`. A race with a
concurrent push can leak a blob, never lose one, because every step is
"remove a reference the index no longer has", and flob's own `Erase` is the
same shape (`README.md`, "Deliberate tolerance of leaks").

**Mark-and-sweep, operator-scheduled, one repository at a time.** `cr gc
--full`, or `POST /admin/gc` from a scheduler, and for each repository:

1. take that repository's manifest-write lock (§3), the same one a manifest
   `PUT` takes, so nothing changes what the repository holds while it is
   measured; reads, blob uploads and every other repository continue;
2. mark: `Index.Manifest().Marks(repo)`, which is `manifests` ∪
   `manifest_blobs`;
3. sweep: `Walk` that one flob namespace and `Erase` what is not marked;
   report index rows whose blob is missing;
4. release the lock, record the result, move to the next repository.

A manifest `PUT` that arrives during a repository's sweep waits for the
lock; if the wait would exceed a bound it answers `503` with `Retry-After`.
There is no global read-only mode, because flob's `Erase` is namespace-local
and so is the sweep. `Walk` promises no snapshot and may omit concurrent
changes, which is what the lock is for. The same walk over every namespace
is `cr index rebuild`. Expired stages are not part of this:
`StageCleaner.PruneStages` runs on the tier-one ticker.

On S3 flob never removes the shared object on `Erase`; that sweep is
flob's, out of band, and unchanged by any of this.

## 8. What cr needed from flob and payday, and got

All ten flob issues were filed on 2026-09-13 and closed the same day, each
with a commit on `main`. #6, #8 and #10 were reproduced with tests before
filing. The API cr builds against is flob `69f0a5a` or later.

| issue | landed as | what cr uses |
| --- | --- | --- |
| [#3](https://github.com/lesomnus/flob/issues/3) | `69f0a5a` `Stager`, `Stage`, `StageCleaner`, `StageConfig` | the whole upload flow, §4 |
| [#4](https://github.com/lesomnus/flob/issues/4) | `7be5002` `Walker`, `Namespacer` | mark-and-sweep and `index rebuild`, §7 |
| [#5](https://github.com/lesomnus/flob/issues/5) | `2a9903b` `Linker` | mount, §4 |
| [#6](https://github.com/lesomnus/flob/issues/6) | `9821686` `Unwrap` on `CacheStore`, `CacheStores`, `FallbackStore` | 307 through a cache |
| [#7](https://github.com/lesomnus/flob/issues/7) | `f4b3ef8` lazy ranged reads on S3 and HTTP | proxying from S3 |
| [#8](https://github.com/lesomnus/flob/issues/8) | `7b0208c` tap survives seek probes | `ServeContent` on a miss |
| [#9](https://github.com/lesomnus/flob/issues/9) | `cfe82fb` shared cache fills per namespace and digest | one upstream fetch per herd |
| [#10](https://github.com/lesomnus/flob/issues/10) | `052f489` namespace ids encoded as one segment | no name mapping in cr |
| [#11](https://github.com/lesomnus/flob/issues/11) | `48c0c8d` **`Get` replaced by `Stat` returning `Info`**, breaking | `HEAD`; labels only when asked |
| [#12](https://github.com/lesomnus/flob/issues/12) | `9821686` sha256, sha384, sha512 | any of the three on push |

The one break to carry: `Store.Get` is gone, `Stat` and `Open` return an
`Info` whose `Labels(ctx)` is a separate, memoized call (`info.go`). Nothing in
cr wanted labels on the blob path anyway.

payday: [#17](https://github.com/lesomnus/payday/issues/17) and
[#18](https://github.com/lesomnus/payday/issues/18), the sandbox template,
fixed in `d571a79` with the rendered template now built in CI.

roster: nothing was needed. See §5.

## 9. Phases

Each phase ends green in CI. The conformance suite
(`opencontainers/distribution-spec/conformance`) is in CI from phase 0 and its
four workflows are turned on as they pass.

| phase | delivers | done when |
| --- | --- | --- |
| 0 | `pd new`, `pd sandbox init`, the `/v2/` handler mounted on `web.Mux`, config, telemetry, bake, `pd gen --check` and the conformance job in CI | `GET /v2/` answers 200 |
| 1 | blobs (HEAD, GET with Range, monolithic, chunked on `Stager`, status, cancel), manifests PUT/GET/HEAD with validation, tags/list, `entindex` on sqlite; tag written as a flob label | conformance **pull** and **push** |
| 2 | referrers with `OCI-Subject`, catalog, manifest and blob DELETE, mount | conformance **content discovery** and **content management** |
| 3 | tenancy decision; `Repository`, `Binding`, `TagRule` entities and their generated services and commands; Basic, token endpoint, JWKS, `htpasswd`, `static`; anonymous pull as a binding; tag rules enforced | `docker login`, a denied push, a refused move of an immutable tag |
| 4 | tier-one GC with `PruneStages`, health, metrics and traces on the handler, prefix router, S3 presign redirect, postgres in CI; `/v1/search` | a compose of cr on S3; `docker search` answers |
| 5 | per-repository mark-and-sweep and `index rebuild` on flob's `Walker`; the admin trigger and run history | a scheduled full GC on the compose while pushes to other repositories continue |
| 6 | pull-through proxy for blobs, then manifests and tags with TTL | a Docker Hub mirror serving `library/ubuntu` |
| 7 | `oidc` and `roster` authenticators; `cr export` to an OCI layout; the management side of registry-ui on the TS client | `docker login` with an `rt_` key |

Phases 0–2 do not depend on payday beyond the schema: the handler is
`net/http` against two ports either way, so the fallback, should one ever be
needed, is go-app `main` with the same handler, the same ports, hand-written
ent schemas on the same fork, and a policy file instead of a management API.

## 10. Scale-out

More than one cr replica is supported on **S3 and Postgres**, and on nothing
else. Where the state is:

- The index is Postgres; transactions and the per-repository lock above make
  concurrent writers safe. payday's `Watch` needs `brokerpg`.
- Blobs are S3. flob's S3 backend was written for shared use: stage records
  live in the bucket under `stages/<ns>/<id>/manifest` with the ETag as a
  fencing token and conditional writes (`If-Match`, `If-None-Match: *`), so a
  `PATCH` that the load balancer sends to another replica is a `Resume` there.
  `Erase` never removes the shared object; concurrent `Add`s of one digest
  both succeed.
- Tokens are JWTs under a shared key; nothing is remembered per replica.
  htpasswd ships with the config.
- Upload `Location`s are relative, or built from the configured public URL,
  never from the replica's own address.
- The cache's shared fill is per process. Two replicas may fetch the same
  blob from upstream once each; both `Add`, one wins, nothing breaks.

One runner only, chosen with a Postgres advisory lock: GC, `PruneStages`,
the sync scheduler, the webhook dispatcher if one exists. GC's per-repository
lock is the same advisory lock manifest writes take, so a replica pushing to
a repository being swept waits on the database, not on a flag.

The `os` backend on a shared filesystem is not a scale-out path: flob's
per-digest `flock` and `nlink` reclamation cannot be trusted on NFS. That is
the answer to the multi-writer question in #1: yes, on S3 and Postgres.

## 11. Non-goals, still

Replication and vulnerability scanning. A web UI is not a non-goal any
more: registry-ui exists, and the management plane is the half it lacks.
