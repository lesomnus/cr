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

`${env:NAME}` anywhere in the file is the variable's value, and
`${env:NAME:-default}` is `default` when it is not set; one that is neither set
nor given a default fails the start rather than read as empty. It is replaced
in the file's text before the file is parsed, so quote a value YAML could
misread, and write `$$` for a literal `$`. `cr config` prints the result, value
and all. A credential that should stay out of that output, or change without a
restart, is a file: see [Credentials](#credentials).

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

With no policy file -- `cr.auth.yaml` beside `cr.yaml` -- the registry is open
to every request, and the log says so at startup; [access.md](access.md) turns
the guard on.

`init` puts up the tenant `operator` and the holder `admin` in it (`--tenant`,
`--holder`). They own what the management plane writes -- tag rules -- and `cr <entity> ...` acts as that holder.

**The image** is `ghcr.io/lesomnus/cr`, for linux/amd64 and linux/arm64. It
runs `cr serve` as a non-root user with no shell, exposes the registry on port
5000, and takes its configuration from `--config` or `CR_*` variables. Every
commit on `main` that passes CI is pushed under four tags, and a release --
a git tag `vX.Y.Z` -- under a fifth:

| tag | |
| --- | --- |
| `:edge` | the latest build of `main`; moves |
| `:vX.Y.Z` | a release, the build of that git tag; never moves, and `cr version` says it |
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
      secret_access_key: ${env:S3_SECRET_ACCESS_KEY}
      session_token: ""
      credentials_file: ""                # the three above as one set, re-read; see below
      path_style: true                    # MinIO and most S3-compatible servers
      part_size: 16777216                 # buffered per part of an upload
      spool_dir: ""                       # where a blob waits to be uploaded; empty is the temporary directory
    redirect:
      enabled: true
      ttl: 15m
```

The service needs conditional writes (`If-Match`, `If-None-Match: *`) and strong
ETags; AWS S3 and MinIO have both. With `redirect.enabled`, a blob `GET` is
answered `307` with a URL signed for `public_endpoint`, so the bytes go from the
bucket to the client, which has to be able to reach that endpoint. `HEAD` and
manifests are always answered by cr.

A blob whose digest and size cr knows before it reads it is streamed to the
bucket as it is read, with its SHA-256 as `x-amz-checksum-sha256` so that the
service refuses bytes that do not match. That is every blob a pull-through
cache fills, a monolithic upload that sends its `Content-Length`, a manifest,
and a blob copied from another repository. Some S3-compatible services ignore
that header, so the first such upload checks: it writes a probe object under
`<prefix>probe/` with a wrong checksum, which must be refused, and with the
right one, and deletes it.

Anything else is written **whole** to a file in `spool_dir` before it goes to
the bucket, because the upload is signed with its hash: an upload that sends
no length, and any upload before the probe has answered or on a service that
ignores the header. The directory holds the largest such blob times however
many are being added at once. Put it on a disk: if it is a tmpfs -- `/tmp` in
many images, or a Kubernetes `emptyDir` with `medium: Memory` -- those files
are memory charged to cr, and a few concurrent large uploads can take a
container past its limit while cr's own memory looks small. Empty is the
system's temporary directory, `$TMPDIR` or `/tmp`. A chunked upload is
neither: it goes to the bucket a part at a time, `part_size` in memory each.

The keys are either written into the configuration, where `${env:...}` keeps
them out of the file and is read once, or kept in `credentials_file` instead:
the JSON an AWS `credential_process` prints.

```json
{"Version": 1, "AccessKeyId": "...", "SecretAccessKey": "...", "SessionToken": "...", "Expiration": "2026-10-01T12:00:00Z"}
```

`SessionToken` and `Expiration` are for temporary credentials, and `Version`
may be left out. The file is **re-read when it changes**, by the same rules as a
proxy's `${file:...}` (see [Credentials](#credentials)), so temporary
credentials are replaced without a restart. The three keys are in one file
because they are replaced together: three files would be read apart, and a
request signed between two of the renames would pair a key with another
key's secret. For the same reason `${file:...}` is refused in the three fields.

With `Expiration`, a redirect's presigned URL is not made to outlive it, and a
request after it fails naming the time instead of a `403` from the service.
Replace the file before then: the set in it is what every request is signed
with, expired or not.

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

## Listeners

`server.http.addr` is one address for the registry, its token endpoint and the
management page. `listeners` are more, each with TLS of its own or none, all
serving the same thing behind the same policy:

```yaml
listeners:
  - addr: ":5000"                     # plain HTTP
  - addr: ":5443"
    tls:
      cert_file: /etc/cr/tls/cr.crt
      key_file: /etc/cr/tls/cr.key
      client_ca_file: /etc/cr/tls/engines.crt   # verify client certificates against these
      client_cert_optional: true                # and serve a caller without one, as anonymous
```

`server.http.addr` may be left empty when `listeners` say where to listen.
With `client_ca_file`, a certificate that does not verify is a failed
handshake, and one that does is a caller the policy's `mtls` provider vouches
for ([access.md](access.md#client-certificates-mtls)). Without
`client_cert_optional`, a caller with no certificate is refused at the
handshake too -- leave it off only where nothing else is meant to connect.
A client offers only a certificate whose issuer the listener names, so one
signed by somebody else is not sent at all, and the caller is anonymous.

The files are read once, when `serve` starts.

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
    delay: 1h          # a blob younger than this is left by a sweep
    full_every: 168h   # the full collection; zero never
```

**Online**, every `every`, on one replica:

- uploads past `upload.ttl`;
- tags that every `retention` rule matching them puts past its `keep`, newest
  first by when they last moved; an `immutable` tag outlives retention;
- manifests older than `untagged` that no tag points at, no index holds, no
  pull touched within `untagged`, and that are not referrers of a manifest
  still in the repository (in a pull-through cache, unless the upstream's
  list omits them; see "Referrers"); their blobs go when nothing else holds
  them. Pull
  times are written in batches, so a pull in the last few seconds before a run
  may not count yet.

Every step removes a reference the index no longer has, so a push racing it can
leave a blob behind and cannot lose one.

**Full** is the online collection and then a mark-and-sweep of every
repository, one at a time. It walks the repository's store and marks
everything the index says the repository holds, both without the lock, and
then holds the repository's lock for a batch of the rest at a time, looking at
each again and erasing what nothing refers to. It reclaims what the online
collection leaks, including a repository the store has and the index does not.
There is no read-only window. Pulls, blob uploads and every other repository
carry on; a manifest push to the repository being swept waits for a batch, not
for the walk, and past `lock_wait` is answered `503` with `Retry-After`. A blob
that entered the repository within `delay` is left for a later sweep, which
keeps the blobs of a push whose manifest has not arrived; a push that takes
longer than `delay` to put its manifest can find its blobs gone, fails with
`MANIFEST_BLOB_UNKNOWN`, and uploads again. A repository still busy after
`lock_wait` is tried once more at the end. On S3 a sweep lists the
repository's markers, a request per thousand, and sends a `HEAD` for each blob
it erases.

A sweep also reports the manifests the index has and the store does not, as
`repo@digest` in the run's `missing`, leaving out what was pushed in the
minute before the walk; nothing repairs those.

Run one when you like:

```sh
curl -X POST -u oidc:$ID_TOKEN https://cr.example.com/admin/gc     # 202 and the run
curl -u oidc:$ID_TOKEN https://cr.example.com/admin/gc/<id>        # how it went
curl -u oidc:$ID_TOKEN https://cr.example.com/admin/gc             # the recent runs
cr gc --full                                                  # from the host
```

The endpoints need `admin` from a permission over `**` alone. A second `POST` while this
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
tag rules. A rebuilt index answers by digest until tags are
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
      hosts: []                             # hosts a mirror is reached by; see "A mirror for several registries"
      auth:                                 # none is anonymous; see "Credentials" below
        kind: password                      # or bearer
        username: someone
        password: ${file:/run/credentials/dockerhub}
      tag_ttl: 5m
      tag_max_stale: 0                      # zero serves a cached tag for as long as the upstream is down
      referrers_ttl: 5m                     # zero is tag_ttl; see "Referrers" below
      referrers_max_stale: 1h               # zero is an hour
      retention: 720h                       # what nobody uses within this goes; zero keeps it
      verify:                               # none serves what the upstream has; see "Signatures" below
        roots: [/etc/cr/trust/root.crt]
        identities: ["C=KR, ST=Seoul, O=Holiday Robotics, CN=Kamino Image Signer"]
```

The repositories under `prefix` are a cache of `upstream`, read on demand at
every level. A tag's manifest is fetched when a client asks for the tag; for a
multi-platform image that is the index alone, and one platform's manifest
follows when the client asks for it, and only that platform's layers after it.
A blob is fetched from the upstream once, and every client asking for it
while it arrives -- the first and any that join -- reads the fill as it
lands, from its first byte, rather than waiting for the whole blob or asking
the upstream again. On `os` and `memory` storage that is; on `s3` a client
that joins waits for the fill to finish. The cache asks the upstream for every manifest type cr stores,
whatever the client accepts, so a tag is one manifest in the cache for every
client.

A tag is answered from the cache for `tag_ttl`, then checked with a `HEAD`
upstream, which Docker Hub does not count against its pull limits, and fetched
again only when it moved. When a tag was last checked is kept in the database
beside the tag, so replicas share it: the upstream is asked once per
`tag_ttl` however many there are, and a replica that starts is not cold.
Requests for one manifest that arrive together -- a fleet of CI jobs starting
on the same cold base image -- make one check or fetch between them in each
replica, and the rest wait for it; Docker Hub counts every manifest `GET`. A
request that gives up waiting leaves the fetch to finish for the cache. A
digest is never checked again. When the upstream cannot be reached, a tag
already cached is served as it is, and a tag never cached fails with `504`.
`tag_max_stale` bounds the first: past it since the last check, the pull fails
too. The default is no bound, since serving pulls while the upstream is down
is what a cache is for. An upstream that answers with an
error, or refuses the cache's credential, is `502`: a failure behind the
registry, and not a `401` that would send the client to authenticate again.

A cache takes no pushes (`405 UNSUPPORTED`); deletes are allowed and evict --
an index with the manifests it lists that nothing else holds or tags, and
their layers, without waiting for `retention` ([registry-api.md](registry-api.md#deleting)). A
tag list is what the cache holds, not what the upstream has. The collection
keeps a cache to `retention` by two rules that do not look at each other: a tag goes when nobody pulled it by name within `retention` and it did
not move, and a manifest goes when nothing holds or tags it and nobody pulled
it, by name or by digest, within `retention`. A manifest still pulled by
digest outlives its tag, and a tag pulled again is fetched again from the
upstream. With a zero `retention` the cache keeps everything a full collection
does not find unreferenced.

### Referrers

A referrers query against a cache is answered from what **the upstream**
lists, never from the manifests the cache happens to hold. Those were fetched
one digest at a time and were never seen to be the whole list, so a signature
nobody has pulled through the cache yet would be missing from it; and the
upstream owns the list, so a signature it has removed must not come back from
the cache's copy.

The upstream's answer is kept in the database and served for `referrers_ttl`,
which is also how long a signature the upstream removed can still be listed:
tune `tag_ttl` for tag traffic and this for revocation, apart. The list is
always asked for unfiltered, every page of it, and `artifactType` is applied
by cr, so `OCI-Filters-Applied` is true whenever it is set. The referrers
themselves are not fetched with the list; a client pulls them by digest like
any other manifest. As with tags, when a list was last checked is kept in the
database, so every replica goes by the same check.

The answers a client can get are kept apart:

| | |
| --- | --- |
| `200` | the upstream's list, possibly empty; carries `Age` when it is from an earlier check |
| `200` with `Cr-Stale: true` | the upstream is failing, and this is what it said within `referrers_max_stale`; a verifier may refuse it |
| `404` | the upstream has no referrers API; a client falls back to the `sha256-<digest>` tag schema, which the cache serves like any tag |
| `502`, `504` | the upstream failed, and there is nothing it said recently enough |

The collection follows the same authority: in a cache, a referrer whose
subject is still there goes when the upstream's last list, taken after the
referrer arrived, omits it. With no list for the subject, or an upstream
without the API, the referrer is kept, since not knowing is not "none".

### Signatures

```yaml
verify:
  mode: require                    # the default; or audit
  roots: [/etc/cr/trust/root.crt]  # PEM; the root CAs a signer's chain ends at
  identities:                      # the signer's subject; `*` is anybody the roots issued to
    - "C=KR, ST=Seoul, O=Holiday Robotics, CN=Kamino Image Signer"
  tsa_roots: []                    # PEM; the root CAs of a timestamp authority
```

A cache with `verify` serves a manifest from its upstream only when somebody
it trusts signed it: a [Notary Project](https://notaryproject.dev/) signature,
as `notation sign` makes, whose certificate chain ends at one of `roots` and
whose signer's subject is one of `identities`. It is notation's own verifier
at its `strict` level, so what `notation verify` accepts under the same roots
and identities, this accepts -- with one exception: revocation is not checked,
since a cache may stand where an OCSP responder cannot be reached, and a
check that cannot reach one fails. An identity is a distinguished name with at
least `C`, `ST` and `O`, which notation requires; it is matched on the
attributes it gives.

Without `tsa_roots`, a signature is good for as long as its certificate is.
With them, a signature whose certificate has expired is still good when it
carries a timestamp they vouch for, from within the certificate's validity; one
whose certificate is valid needs no timestamp.

What is checked is a manifest the client asks for, by tag or by digest, `GET`
or `HEAD`; blobs are not, since nothing reaches one but through a manifest. The
signatures are what the upstream lists for the manifest -- its referrers, or
the referrers tag schema (`sha256-<hex>`) where it has no referrers API -- read
through the cache as a client would read them, so they are cached too. A
release is signed by its index, and a client asks for the index and then the
one platform it runs by digest, which nobody signed on its own: a manifest
that an index in the cache holds is served when that index is signed.
Signatures themselves, and the tag schema's list of them, are served without
one; they are what a verifier reads, and nothing runs them.

| | |
| --- | --- |
| `403 DENIED` | the upstream lists no signature for it, or none of them verifies; the message says which, and why |
| `502`, `504` | its signatures could not be read: the upstream failed, and the cache has no copy of them. Not the image's fault, and it is served once they can be |

Docker shows the message of the first: `denied: dist/app@sha256:… is not
signed by anybody this registry trusts: not signed`.

When the upstream cannot be reached, the referrers it last listed are read
from the database however old they are -- past `referrers_max_stale`, which
bounds only what a client is handed as the list -- and the signatures from the
cache. A cache that verified a release goes on serving it without its
upstream. A signature is withdrawn by removing it upstream: it counts until the
cache next hears the list, `referrers_ttl` after it last did, and not after.

What was decided is remembered in memory, by the manifest and the signatures
listed for it, so a pull does not verify the same signature again, and a
changed list is decided again. `verify` is read at start; a change is a
restart, which forgets everything decided.

`mode: audit` serves what would be refused, logs it, and counts it in
`cr.verify.requests`: for finding out what `require` would refuse before
anything is.

### A mirror for several registries

[mirror.md](mirror.md) is a whole deployment of this, with the clients'
configuration.

A client configured with a mirror asks it for the name as the registry knows
it, `library/alpine` and not `docker.io/library/alpine`. containerd, buildkit
and Docker on the containerd image store say which registry in `ns`:

```
GET /v2/library/alpine/manifests/3.20?ns=docker.io
```

When a proxy's `prefix` is what `ns` names, the name is taken to be under it:
that request is for `docker.io/library/alpine`, from the policy's view as
from the cache's, so one cr with a proxy per registry is a mirror for all of
them, and a blob two registries share is stored once. A name a prefix already
covers keeps its prefix whatever `ns` says, and an `ns` no prefix is leaves
the name as it is.

`ns` only picks among the proxies configured: cr never asks a registry that is
not an `upstream`. A prefix that is a registry's name says the proxy stands for
that registry, whatever its `upstream` is -- `prefix: ghcr.io` in front of
another mirror of ghcr.io answers `ns=ghcr.io` from that mirror. A prefix that
is no registry's name, `dist`, is never what `ns` names.

```yaml
# containerd, /etc/containerd/certs.d/ghcr.io/hosts.toml
server = "https://ghcr.io"
[host."https://cr.example.com"]
  capabilities = ["pull", "resolve"]
```

```toml
# buildkitd.toml
[registry."docker.io"]
  mirrors = ["cr.example.com"]
```

A client that does not send `ns` can be given a host per registry instead,
and a proxy lists the hosts that are its own:

```yaml
registry:
  proxies:
    - prefix: docker.io
      upstream: https://registry-1.docker.io
      hosts: [dockerhub.example.com]
```

A request that came in on `dockerhub.example.com` for `library/alpine` is for
`docker.io/library/alpine`. The host is only asked when there is no `ns`: a
client that says which registry it wants is given that one or none, and a
name a prefix covers keeps its prefix over both. The port is not compared, and
the reverse proxy in front has to pass the `Host` the client asked for. A host
belongs to one proxy, and the empty prefix lists none.

An empty `prefix` makes every repository a cache, which is what a daemon's
`registry-mirrors` expects of a mirror: it asks for `library/ubuntu`, without
`ns`, and not for a prefixed name. It takes what no prefix or host did, and a request
with `ns` only when `ns` is its `upstream`'s host -- `docker.io` for
`registry-1.docker.io`. Anything else is `404 NAME_UNKNOWN`, and the client
goes to that registry itself: two registries can each have an `acme/app`,
kept by different people, and a mirror of one that answered for the other
would hand out the wrong image without an error anywhere.

### Credentials

A proxy's `auth` says how its upstream is asked, by `kind`; without one, it is
asked anonymously.

```yaml
registry:
  proxies:
    - prefix: docker.io
      upstream: https://registry-1.docker.io
      auth:
        kind: password
        username: someone
        password: ${file:/run/credentials/dockerhub}
    - prefix: dist
      upstream: https://registry.example.com
      auth:
        kind: bearer
        token: ${file:/run/credentials/registry-token}
```

**`password`** answers whatever the upstream challenges with — Basic, or the
token endpoint its `WWW-Authenticate` names. Docker Hub's access tokens,
`dckr_pat_...`, are a password here.

**`bearer`** is for the other kind: a credential the upstream never issued,
minted somewhere else for this deployment and replaced before it expires. A
machine that proves what it is to an authority and is handed a registry token
gets one of these. It is sent as `Authorization: Bearer` **from the first
request**, not after a 401: there is nothing to exchange, and waiting for the
challenge would refuse a request for every manifest and blob before the one
that worked.

A field that does not belong to the kind, a kind with a field missing, and a
credential without a kind are refused where the configuration is read rather
than at the first pull. So are `username`, `password` and `token_file` beside
`auth`, where they used to be.

#### `${file:...}`

`password` and `token` are each either the value itself or `${file:/path}`: the
file's content, without the whitespace around it. That keeps the list of
proxies in a plain ConfigMap and only the secret in a Secret, mounted as a file:

```yaml
volumeMounts:
  - { name: dockerhub, mountPath: /run/credentials, readOnly: true }
```

**The file is re-read when it changes**, so a rotated credential is used
without a restart: a token a machine renews every few days, or a password
replaced in the Secret. Unlike `${env:...}`, it is only understood in these two
fields, and `cr config` prints the reference rather than what the file holds.

A `password` is read when a challenge is answered. A token the upstream issued
from the old one is used until it expires or is refused; a refusal is answered
again with the password as it is then, so revoking the old one after the new one
is in place is the whole rotation. A `bearer` token is read for every request.

Reading it is a `stat` beside a request cr was making anyway — the file's
identity first, because publishing a credential means writing a temporary name
and renaming it into place, and a rename always puts a different file there
whatever the clock says. That is also how Kubernetes updates a mounted Secret,
except one mounted with `subPath`, which is never updated at all. A writer that rewrites the file in place with a value of the same length inside
one filesystem tick is not noticed; publishing by rename is the contract.

A read that fails **after** a good one keeps the value it has and says nothing:
rename is atomic for content and not for permissions, so between the rename and
the chown that follows it the file is there and unreadable, and failing a pull
for that would be failing it because the credential was being renewed. A first
read that fails, or finds the file empty, has nothing to fall back on, and the
pull fails naming the file.

A value that begins `${file:` is always a reference; there is no escaping it.

## Health and telemetry

`/healthz` answers 200 while the process runs. `/readyz` answers 200 when the
database answers within a second and the process is not stopping, and 503
otherwise; it checks nothing else.

Telemetry is OpenTelemetry, configured under `otel:` in the collector's own
language. With nothing there, the log is printed and nothing else leaves the
process; an exporter, and the providers that use it, send the signals away:

```yaml
otel:
  exporters:
    otlp:
      endpoint: collector:4317      # gRPC; or `protocol: http/protobuf` and a URL
  providers:
    tracer: {exporters: [otlp]}
    meter: {exporters: [otlp]}
    logger: {exporters: [otlp]}
```

What is measured:

| | |
| --- | --- |
| `http.server.request.duration` | a histogram, in seconds, of every request to `/v2/`, `/v1/`, `/admin/`, `/token`, `/token/exchange` and the key set, by `http.request.method`, `http.route` and `http.response.status_code`. The route is the pattern, `/v2/{name}/blobs/{digest}`, so the series do not grow with the repositories |
| `http.server.request.body.size`, `http.server.response.body.size` | histograms, in bytes, of what those requests read and wrote, by the same attributes. A blob `GET` answered with a redirect wrote a `307` and no bytes; the bucket sent them |
| `http.server.active_requests` | the requests in flight, by method and route |
| `cr.registry.errors` | every error envelope the registry answered, by `cr.error.code` -- `MANIFEST_BLOB_UNKNOWN`, `DENIED`, `NAME_UNKNOWN` and the rest -- with the route and the status: what a `400` or a `404` was |
| `cr.repository.lock.wait`, `cr.repository.lock.timeouts` | how long a write waited for its repository's lock, and how often it gave up after `lock_wait`, by `cr.lock.for`: `manifest push`, `manifest delete`, `blob delete`, `cache fetch`, `referrers fetch`, `release`, `collection`, `sweep`, `bookkeeping`. The one thing cr serializes, measured |
| `cr.store.operation.duration` | each call to a blob store, by `cr.store.driver` (`os`, `s3`, `memory`), `cr.store.operation` (`add`, `stat`, `open`, `label`, `erase`) and `cr.store.outcome` (`ok`, `not_found`, `exists`, `error`). `add` includes reading what it stores, so an upload's `add` is as long as the upload; mounts, presigned URLs and the collection's walk reach the store beneath and are not measured |
| `cr.cache.requests` | manifest requests to a pull-through cache, by `cr.cache.proxy` (the prefix, `*` for the empty one) and `cr.cache.outcome`: `hit` from the cache alone, `revalidated` after the upstream said the tag had not moved, `refreshed` after it had, `miss` for what was not cached, `joined` for a request that waited on another's check or fetch of the same manifest, `stale` for a cached tag served because the upstream failed, `unknown` for what the upstream does not have, `error` for the rest. `hit` and `joined` over everything is the hit ratio, and `miss`, `refreshed` and `revalidated` are what reached the upstream |
| `cr.verify.requests` | manifest requests to a cache with `verify`, by `cr.cache.proxy`, `cr.verify.mode` and `cr.verify.outcome`: `verified`, `unsigned` when the upstream lists no signature, `invalid` when none verifies, `unchecked` when they could not be read. In `audit` mode the last three are served |
| `cr.cache.referrers` | referrers requests to a pull-through cache, by `cr.cache.proxy` and `cr.cache.outcome`: `hit`, `revalidated` when the upstream listed the same, `refreshed` when it did not, `miss` for a list never asked for, `stale` for one served because the upstream failed, `error` for the rest |
| `cr.cache.upstream.duration`, `cr.cache.upstream.bytes` | every request a cache made to its upstream, by `cr.cache.upstream` (its host), `cr.cache.operation` (`manifest head`, `manifest get`, `blob head`, `blob get`, `referrers get`) and the status, a challenge answered on the way included; and the bytes it read, manifests and blobs apart |
| `cr.gc.runs`, `cr.gc.run.duration` | every collection, by `cr.gc.kind` (`online`, `full`), `cr.gc.trigger` (`schedule`, `admin`, `cli`) and `cr.gc.state` (`done`, `failed`), and how long each took |
| `cr.gc.reclaimed`, `cr.gc.reclaimed.bytes` | what collections removed, by kind and `cr.gc.what`: `uploads` that expired, `tags` past retention, `manifests` nothing needed, `blobs` a sweep erased; and the bytes of those blobs |
| `cr.gc.missing` | after a full collection, the manifests the index has and the store does not |
| `cr.repositories`, `cr.manifests`, `cr.tags` | how much the index holds, counted after every collection |
| `cr.index.pulls.pending`, `cr.index.pulls.dropped`, `cr.index.pulls.flush.duration` | the pull bookkeeping: rows waiting when a flush began, touches dropped for there being more than 100,000 waiting, and how long a flush took |
| `cr.uploads` | blob uploads that ended, by `cr.upload.outcome`: `completed`, `exists` for a digest the repository already had, `mounted` from another repository, `cancelled`. The ones that expired are `cr.gc.reclaimed` with `cr.gc.what=uploads` |
| `cr.auth.logins` | credentials checked, by `cr.auth.authenticator` (`oidc`, `exchange`, or `none` when nobody accepted) and `cr.auth.outcome` (`ok`, `refused`); a burst of refusals is a leaked key or a broken configuration |
| `cr.auth.denied` | actions refused, by `cr.auth.action` and `cr.auth.at`: `token` when a token was issued without them, `request` when a request asked for them without a token |
| `cr.auth.policy.age`, `cr.auth.policy.refresh.errors` | seconds since the policy file and the tag rules in the database were last loaded, and the loads that failed; a load that fails keeps the policy in force, and the age is how that shows |
| `cr.auth.policy.revision` | 1, with the revision of the policy file in force as `cr.auth.policy.revision`: replicas with different ones have not all read the same file yet |
| `go.memory.*`, `go.goroutine.count`, and the rest of OpenTelemetry's Go runtime instrumentation | the process itself |
| `rpc.server.call.duration` | the management API's calls, over gRPC and Connect alike, by service, method and status code: the OpenTelemetry gRPC instrumentation's own |
| spans | one server span per registry request, named for its route, and one per management call, continuing a trace the client started |

What is not measured: the database's own health and the bucket's, which have
telemetry of their own, and what a store holds beyond what a sweep erased.
Every attribute above is bounded by the configuration or by the code -- a
route pattern, a driver, a proxy's prefix, an error code -- and never a
repository's name or a digest, so the series do not grow with the registry.

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
