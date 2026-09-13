# Operating cr

How a deployment is configured and run. [plan.md](plan.md) says why things are
the way they are; this page says how to use them.

## Running

`compose.yaml` stands up cr on MinIO and PostgreSQL, with an `admin` user and
anonymous pull, catalog and search; its header says how to push to it.

```sh
cr --config cr.yaml init     # the operator tenant and its first holder
cr --config cr.yaml serve
```

`cr config` prints the configuration as it was read, and `cr config env` lists
every `CR_*` variable that overrides it. The image runs `cr serve` and exposes
the registry on port 5000.

`init` puts up the tenant `operator` and the holder `admin` in it by default
(`--tenant`, `--holder`). They own what the management plane writes: bindings
and tag rules.

## Storage

```yaml
registry:
  storage:
    driver: os          # os, s3, or memory for a throwaway
    os:
      root: /var/lib/cr/data
    upload:
      ttl: 24h          # an upload nobody appends to
      retention: 24h    # a finished upload's receipt
  max_manifest_size: 4194304
  lock_wait: 30s        # a manifest write waiting for its repository
```

The database is payday's `db:` block: `sqlite3` for one process, `pgx` for
PostgreSQL and more than one. The registry is served on `server.http.addr`,
which must be set.

### S3

```yaml
registry:
  storage:
    driver: s3
    s3:
      endpoint: http://minio:9000         # empty is AWS in `region`
      public_endpoint: https://blobs.example.com
      region: us-east-1
      bucket: cr-blobs
      prefix: ""                          # to share a bucket
      access_key_id: ...
      secret_access_key: ...
      path_style: true                    # MinIO and most S3-compatible servers
    redirect:
      enabled: true
      ttl: 15m
```

flob needs conditional writes (`If-Match`, `If-None-Match: *`) and strong ETags
from the service; AWS S3 and MinIO have both. With `redirect.enabled`, a blob
`GET` is answered `307` to a URL signed for `public_endpoint`, so the bytes go
from the object store to the client; a client has to be able to reach that
endpoint. `HEAD` and manifests are always answered by cr.

### Routes

```yaml
registry:
  storage:
    driver: os
    os: {root: /var/lib/cr/data}
    routes:
      - prefix: library
        driver: s3
        s3: {...}
```

A route puts the repositories under its prefix on another store: `library`
covers `library/ubuntu`, not `librarything`. The longest prefix wins, and the
store above holds everything no route covers. A mount between repositories on
different stores is a copy.

## Garbage collection

```yaml
registry:
  gc:
    every: 1h          # the online collection; negative never runs it
    untagged: 168h     # zero keeps untagged manifests
    full_every: 168h   # the full collection; zero never
```

**Online**, every `every`, on one replica chosen by a PostgreSQL advisory lock:

- uploads past `upload.ttl`;
- tags that every `retention` rule matching them puts past its `keep`, newest
  first by when they last moved; an `immutable` tag outlives retention;
- manifests older than `untagged` that no tag points at, no index holds, no
  pull touched within `untagged`, and that are not referrers of a manifest
  still in the repository; their blobs go when nothing else holds them.

Every step removes a reference the index no longer has, so a push racing it can
leave a blob behind and cannot lose one.

**Full** is the online collection and then a mark-and-sweep of every
repository, one at a time: holding that repository's lock, it marks everything
the index says the repository holds, walks the repository's store, and erases
the rest. It is what reclaims what the online collection leaks. There is no
read-only window. Pulls, blob uploads and every other repository carry on; a
manifest push to the repository being swept waits for the lock, and past
`lock_wait` is answered `503` with `Retry-After`. A blob uploaded to that
repository during its sweep may be erased before its manifest arrives, and that
push fails with `MANIFEST_BLOB_UNKNOWN` and uploads again.

A sweep also reports the manifests the index has and the store does not, as
`repo@digest` in the run's `missing`; nothing repairs those.

Run one when you like:

```sh
curl -X POST -u admin:... https://cr.example.com/admin/gc     # 202 and the run
curl -u admin:... https://cr.example.com/admin/gc/<id>        # how it went
curl -u admin:... https://cr.example.com/admin/gc             # the recent runs
cr gc --full                                                  # from the host
```

The endpoints need `admin` from a binding over `*`. A second `POST` while this
replica runs one answers `409` with the run in progress. Every run, online and
full, is a `GcRun` row: `cr gc-run ls -o table`.

`cr gc` runs in its own process. On PostgreSQL it takes the same locks the
server does. On SQLite the locks are the serving process's, so `cr gc` refuses
unless the server is stopped and `--offline` says so.

## Rebuilding the index

```sh
cr index rebuild
```

reads every repository's namespace in the store and puts back what it finds:
repositories, manifests and what they hold, and the tags each manifest carries
as labels. It only adds, so it runs over an empty database or a partial one.
What the store does not keep is lost: when a manifest was pushed and pulled, a
repository's description, and bindings and tag rules, which are rows of their
own. On S3 the tags live in object metadata, which AWS caps at 2 KB, so a
manifest with a great many tags gives back only the ones that fit.

## Pull-through caches

```yaml
registry:
  proxies:
    - prefix: docker.io                     # docker.io/library/ubuntu
      upstream: https://registry-1.docker.io
      remote: ""                            # the upstream name for the prefix; empty maps the rest as is
      username: ""                          # when the upstream wants one
      password: ""
      tag_ttl: 5m
      retention: 720h                       # what nobody pulls within this goes
```

The repositories under `prefix` are a cache of `upstream`, read on demand at
every level. A tag's manifest is fetched when a client asks for the tag; for a
multi-platform image that is the index alone, and the one platform's manifest
follows when the client asks for it by digest, and only that platform's layers
after it. A blob a client asks for streams from the upstream while it fills the
store, and clients asking for the same blob meanwhile wait for that one fill
rather than asking the upstream again.

A tag is answered from the cache for `tag_ttl`, then checked with a `HEAD`
upstream, which Docker Hub does not count against its pull limits, and fetched
again only when it moved. A digest is never checked again. When the upstream
cannot be reached, a tag already cached is served as it is.

A cache takes no pushes (`405 UNSUPPORTED`); deletes are allowed and evict. Tag
lists and referrers are what the cache holds, not what the upstream has. The
collection keeps a cache to `retention`: tags nobody pulled within it, and then
the manifests nothing needs any more, whether they were tagged or not.

An empty `prefix` makes every repository a cache, which is what a daemon's
`registry-mirrors` expects of a mirror: it asks for `library/ubuntu` and not
for a prefixed name.

## Health and telemetry

`/healthz` answers 200 while the process runs; `/readyz` answers 200 when the
database answers within a second, and 503 otherwise.

Requests to `/v2/`, `/v1/`, `/token` and the key set get a server span named for
their route (`GET /v2/{name}/blobs/{digest}`) and a
`http.server.request.duration` histogram by method, route and status, through
payday's `otel:` configuration.

## Search

`docker search cr.example.com/term` asks `/v1/_ping` and then `/v1/search`,
which answers in Docker Hub's shape and is what registry-ui's search speaks
too. It needs the `search` action from a binding over `*`, and shows only
repositories the caller may pull. A repository's description is its `desc`,
set through the management plane (`cr repository patch`), or else the
`org.opencontainers.image.description` annotation of the manifest its most
recently moved tag points at.

## Who may do what

With no `auth:` block the registry is open: every request may pull, push and
delete, and the log says so at startup. Anything configured below turns the
guard on; `auth.enabled: true` turns it on when every binding is a row.

```yaml
auth:
  htpasswd:
    path: /etc/cr/htpasswd      # htpasswd -B; read again when it changes
    groups:
      alice: [release]
  static:                       # long-lived tokens, for CI without roster
    - name: ci
      token_sha256: 5e884898da28047151d0e56f8dc6292773603d0d6aabbdd62a11ef721d1542d8
      groups: [ci]
  token:
    service: cr.example.com     # the tokens' audience; `cr` by default
    realm: https://cr.example.com/token
    keys: [/etc/cr/token.pem]   # P-256; the first signs, all verify
    ttl: 5m
  bindings:
    - subject: anonymous        # everyone
      repo: "library/*"
      actions: [pull]
    - group: ci
      repo: "acme/*"
      actions: [pull, push, tag]
    - subject: alice
      repo: "*"
      actions: ["*"]
  tag_rules:
    - name: releases
      repo: "*"
      tag: "v*"
      kind: immutable
```

**Credentials.** `docker login` sends a username and password. htpasswd users
are checked with bcrypt; a static token is accepted as the password with any
username. The registry answers `/v2/` with a Bearer challenge, clients fetch a
token from `/token` with their credentials, and every later request is checked
offline against that token. `/v2/` also takes Basic directly. The public keys
are at `/.well-known/jwks.json`.

Without `token.keys` a key is made at startup. Tokens then die with the
process and no second replica accepts them, so a real deployment names one:

```sh
openssl ecparam -name prime256v1 -genkey -noout -out token.pem
```

**Bindings** add and never subtract. A subject is what an authenticator names;
`anonymous` is every caller, credential or not; the group `authenticated` is
every caller whose credential checked. A repository glob's `*` matches any run
of characters, slashes included. The actions are `pull`, `push`, `delete`,
`tag` (create or move a tag; a push without it is by digest only), `catalog`,
`search`, `admin`, and `*`. `catalog`, `search` and registry-wide `admin` come
only from a binding whose repository is `*`. A mount needs `push` on the target
and `pull` on the source, or it becomes an ordinary upload.

**Tag rules** are checked on a manifest push and delete before anything is
written:

| kind | refuses |
| --- | --- |
| `immutable` | moving or deleting a tag once it is set; pushing the same digest again is fine |
| `protected` | moving or deleting, unless the caller has `admin` or is in `groups` |
| `pattern` | creating or moving a tag that does not wholly match `pattern` (RE2) |
| `retention` | nothing at push; garbage collection keeps the newest `keep` |

A delete by digest removes the tags pointing at the manifest, and is refused
when any of them may not be deleted. A `pattern` that does not compile refuses
every tag it covers.

Rows add to the configuration. `cr` talks to the management API in-process on
the host, as the holder `management.as` names (`@operator/admin` by default):

```sh
cr binding add @operator/ci-push '{"group": "ci", "repo": "acme/*", "actions": ["pull", "push", "tag"]}'
cr tag-rule add @operator/semver '{"repo": "acme/*", "tag": "*", "kind": "pattern", "pattern": "v\\d+\\.\\d+\\.\\d+|latest"}'
cr binding ls -o table
cr binding erase @operator/ci-push
```

The registry reads rows again every `auth.refresh` (five seconds). A decision
never waits on the database: requests read a snapshot, and when a reload
fails, the snapshot in force stays in force.

## The management API

Every entity service, over gRPC on `server.addr` and over HTTP beside the
registry when `server.http.allow_web` is on. It takes bearer tokens from
configuration, each acting as a payday holder:

```yaml
management:
  tokens:
    - holder: "@operator/admin"
      token_sha256: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
```

With no tokens nothing over the network may use it; `cr <entity> ...` on the
host still can. Repositories, manifests and tags are read-only there: the
registry writes them, and a row the API added or erased would disagree with
the store.
