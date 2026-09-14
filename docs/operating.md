# Operating cr

How to run a deployment: configuration, storage, deploying one process or
several, garbage collection, the index, pull-through caches, health and export.
[access.md](access.md) covers who may do what, and
[how-it-works.md](how-it-works.md) why cr behaves the way it does.

## Running

`compose.yaml` stands up cr on MinIO and PostgreSQL, with a pull-through cache
of Docker Hub, an `admin` token and anonymous pull, catalog and search; its
header says how to push to it.

The binary reads `cr.yaml` from the working directory, or the file `--config`
names, and every setting can be overridden by a `CR_*` environment variable:

```sh
cr --config cr.yaml config       # the configuration, as it was read
cr --config cr.yaml config env   # every variable that overrides it
cr --config cr.yaml init         # the operator tenant and its first holder
cr --config cr.yaml serve
```

A minimal configuration, for one process on a local disk:

```yaml
db:
  driver: sqlite3
  dsn: "file:/var/lib/cr/cr.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)"
  migrate: true
server:
  addr: "127.0.0.1:50051"   # the management API over gRPC
  http:
    addr: ":5000"           # the registry
watch:
  broker: memory
registry:
  storage:
    driver: os
    os:
      root: /var/lib/cr/data
```

With no `auth:` block the registry is open to every request, and the log says
so at startup; [access.md](access.md) turns the guard on.

`init` puts up the tenant `operator` and the holder `admin` in it (`--tenant`,
`--holder`). They own what the management plane writes -- bindings and tag
rules -- and `cr <entity> ...` acts as that holder.

**The image** is `ghcr.io/lesomnus/cr`, for linux/amd64 and linux/arm64. It
runs `cr serve` as a non-root user with no shell, exposes the registry on port
5000, and takes its configuration from `--config` or `CR_*` variables. Every
commit on `main` that passes CI is pushed under four tags:

| tag | |
| --- | --- |
| `:edge` | the latest build of `main`; moves |
| `:r<run>` | one CI run's build |
| `:YYMMDD` | the last build of that day; moves |
| `:YYMMDD-r<run>` | one build, and never moves: the one a deployment pins |

**The database** is `db.driver: sqlite3` for one process, or `pgx` for
PostgreSQL and any number of them. With `db.migrate: true`, `serve` creates and
upgrades its tables as it starts. Without it, `serve` refuses a database whose
tables are not the shape this build expects and prints the SQL that is
missing, so that a migration is applied on purpose.

## Storage

```yaml
registry:
  storage:
    driver: os              # os, s3, or memory for a throwaway
    os:
      root: /var/lib/cr/data
    upload:
      ttl: 24h              # an upload nobody appends to
      retention: 24h        # a finished upload's answer, for a retried final PUT
  max_manifest_size: 4194304
  lock_wait: 30s            # a manifest write waiting for its repository, before 503
  disable_well_known: false # store the constant blobs rather than answer them from memory
```

`os` wants a local filesystem; see [Deploying](#deploying).

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
      session_token: ""
      path_style: true                    # MinIO and most S3-compatible servers
      part_size: 16777216                 # buffered per part of an upload
    redirect:
      enabled: true
      ttl: 15m
```

The service needs conditional writes (`If-Match`, `If-None-Match: *`) and strong
ETags; AWS S3 and MinIO have both. With `redirect.enabled`, a blob `GET` is
answered `307` with a URL signed for `public_endpoint`, so the bytes go from the
bucket to the client, which has to be able to reach that endpoint. `HEAD` and
manifests are always answered by cr.

On S3 a delete removes a repository's reference to a blob and leaves the one
shared copy in the bucket, so neither deleting nor collecting garbage shrinks
the bucket yet; see [how-it-works.md](how-it-works.md#storage).

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

## Deploying

**One process: SQLite, or the `os` store.** The lock that keeps a manifest
write and a sweep of the same repository apart, and the choice of who runs the
collection, live inside the process. So exactly one `cr serve` may use a SQLite
database, and the `os` store belongs on a local disk: its per-digest file locks
and link counts cannot be trusted on NFS.

On Kubernetes that is one replica whose old pod is gone before the new one
starts, with its data on a volume that pod alone mounts:

```yaml
spec:
  replicas: 1
  strategy:
    type: Recreate
```

The default rolling update starts the new pod first, and the two would share
the database and the store without sharing the lock. An update is therefore a
short outage, from the old pod stopping to the new one answering `/readyz`.

**Several replicas: PostgreSQL and S3.**

```yaml
db:
  driver: pgx
  dsn: postgres://cr:...@postgres:5432/cr
watch:
  broker: postgres
registry:
  storage:
    driver: s3
auth:
  token:
    keys: [/etc/cr/token.pem]   # the same key on every replica
```

Any replica can take any request. A repository's lock is a PostgreSQL advisory
lock, an upload continues on whichever replica its next chunk reaches, and a
token one replica signed verifies on the others. The collections run on one
replica at a time, whichever takes the lock for the run. A schema change in an
upgrade has to suit the replicas still running the older version until they
are replaced.

### Stopping

```yaml
shutdown:
  drain: 5s      # /readyz fails, and both listeners go on answering
  timeout: 20s   # then requests in flight get this long before they are cut
```

`SIGTERM`, which Docker and Kubernetes send, and `SIGINT` start the same stop.
`/readyz` answers `503` at once, and both listeners go on answering for
`drain`, so whatever routes by readiness takes the server out of rotation while
it can still take requests. Then the listeners close, a push or a pull in
flight gets `timeout` to finish, and whatever is still running after that is
cut. An open Connect stream -- the management page's `Watch` -- does not hold
the stop: it ends when the listeners close, and its client connects again
wherever it is sent. A second signal ends the process at once. `drain` is zero
unless set, which closes the listeners straight away, and `timeout` is twenty
seconds.

On Kubernetes, give `drain` a little longer than the readiness probe takes to
notice, and the pod longer than `drain` and `timeout` together:

```yaml
spec:
  terminationGracePeriodSeconds: 40   # more than drain + timeout
  containers:
    - name: cr
      readinessProbe:
        httpGet: {path: /readyz, port: 5000}
        periodSeconds: 2
        failureThreshold: 1
```

With several replicas, a rolling update then moves traffic to the new pods
without cutting what is in flight, unless it runs longer than `timeout`. With
one replica, `Recreate` still means an outage while the new pod starts.

## Garbage collection

```yaml
registry:
  gc:
    every: 1h          # the online collection; negative never runs it
    untagged: 168h     # zero keeps untagged manifests
    full_every: 168h   # the full collection; zero never
```

**Online**, every `every`, on one replica:

- uploads past `upload.ttl`;
- tags that every `retention` rule matching them puts past its `keep`, newest
  first by when they last moved; an `immutable` tag outlives retention;
- manifests older than `untagged` that no tag points at, no index holds, no
  pull touched within `untagged`, and that are not referrers of a manifest
  still in the repository; their blobs go when nothing else holds them. Pull
  times are written in batches, so a pull in the last few seconds before a run
  may not count yet.

Every step removes a reference the index no longer has, so a push racing it can
leave a blob behind and cannot lose one.

**Full** is the online collection and then a mark-and-sweep of every
repository, one at a time: holding that repository's lock, it marks everything
the index says the repository holds, walks the repository's store, and erases
the rest. It reclaims what the online collection leaks, including a repository
the store has and the index does not. There is no read-only window. Pulls, blob
uploads and every other repository carry on; a manifest push to the repository
being swept waits for the lock, and past `lock_wait` is answered `503` with
`Retry-After`. A blob uploaded to that repository during its sweep may be
erased before its manifest arrives, and that push fails with
`MANIFEST_BLOB_UNKNOWN` and uploads again. A repository still busy after
`lock_wait` is tried once more at the end.

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
replica runs one answers `409` with the run in progress; on several replicas the
run goes to whichever takes the lock, and the others record
`failed: another replica is collecting`. Every run, online and full, is a
`GcRun` row (`cr gc-run ls -o table`), and a run whose process died stays
`running`, with its start time saying how stale it is.

`cr gc` runs in its own process. On PostgreSQL it takes the same locks the
server does. On SQLite the locks are the serving process's, so `cr gc` refuses
unless the server is stopped and `--offline` says so.

## Rebuilding the index

```sh
cr index rebuild
```

reads every repository's namespace in the store and puts back what it finds:
repositories, manifests and what they hold. It only adds, so it runs over an
empty database or a partial one. What the store does not keep is lost: tags,
when a manifest was pushed and pulled, a repository's description, and
bindings and tag rules. A rebuilt index answers by digest until tags are
pushed again. A rebuilt manifest counts as pushed at the rebuild, so the
untagged collection takes it once `gc.untagged` has passed unless a tag points
at it by then; set `untagged: 0` until the tags are back. The database is what
to back up, and a rebuild is for when there is no backup.

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
      retention: 720h                       # what nobody uses within this goes; zero keeps it
```

The repositories under `prefix` are a cache of `upstream`, read on demand at
every level. A tag's manifest is fetched when a client asks for the tag; for a
multi-platform image that is the index alone, and one platform's manifest
follows when the client asks for it, and only that platform's layers after it.
A blob streams from the upstream while it fills the store, and clients asking
for the same blob meanwhile wait for that one fill rather than asking the
upstream again. The cache asks the upstream for every manifest type cr stores,
whatever the client accepts, so a tag is one manifest in the cache for every
client.

A tag is answered from the cache for `tag_ttl`, then checked with a `HEAD`
upstream, which Docker Hub does not count against its pull limits, and fetched
again only when it moved. When a tag was last checked is kept in memory, so a
restart or another replica checks once more. A digest is never checked again.
When the upstream cannot be reached, a tag already cached is served as it is,
and a tag never cached fails.

A cache takes no pushes (`405 UNSUPPORTED`); deletes are allowed and evict. Tag
lists and referrers are what the cache holds, not what the upstream has. The
collection keeps a cache to `retention`: tags nobody used within it -- a pull,
a `HEAD`, a pull of the manifest a tag points at, or the tag moving -- and then
the manifests nothing needs any more, whether they were tagged or not. With a
zero `retention` the cache keeps everything a full collection does not find
unreferenced.

An empty `prefix` makes every repository a cache, which is what a daemon's
`registry-mirrors` expects of a mirror: it asks for `library/ubuntu` and not
for a prefixed name.

## Health and telemetry

`/healthz` answers 200 while the process runs. `/readyz` answers 200 when the
database answers within a second, and 503 otherwise; it checks nothing else.

Requests to `/v2/`, `/v1/`, `/admin/`, `/token` and the key set get a server
span named for their route (`GET /v2/{name}/blobs/{digest}`) and a
`http.server.request.duration` histogram by method, route and status, through
the `otel:` configuration.

## Export

```sh
cr export acme/app ./acme-app --tags v1,latest
```

writes the tags -- every tag when `--tags` is left out -- every manifest and
blob they need, and the signatures, attestations and SBOMs that refer to them
(`--no-referrers` leaves those out) as an OCI image layout, which `oras`,
`skopeo`, `crane` and containerd read. Each tag is named in `index.json` with
`org.opencontainers.image.ref.name`, and referrers are written without a name.
A layer a pull-through cache never fetched, or one that is not distributable,
is listed as skipped rather than failing the export.
