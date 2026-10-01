# A cache for other registries

How to run cr as nothing but a pull-through cache: one deployment,
`cache.example.com` here, in front of Docker Hub, GitHub, NVIDIA and the rest,
and Docker, containerd and BuildKit configured to pull through it. The
machinery behind it is in [operating.md](operating.md#pull-through-caches).

What it buys:

- a pull of a cached image leaves the network once, whoever pulls it next;
- Docker Hub's pull limits count cr's pulls, not every machine's;
- a layer two registries share -- `docker.io/library/alpine` and
  `public.ecr.aws/docker/library/alpine` are the same image -- is stored once;
- pulls go on while an upstream is down, for what is already cached.

## cr

`cr.yaml`:

```yaml
db:
  driver: sqlite3             # one process; pgx and a PostgreSQL DSN for several
  dsn: "file:/var/lib/cr/cr.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)"
  migrate: true

server:
  addr: "127.0.0.1:50051"
  http:
    addr: ":5000"

watch:
  broker: memory              # one process

registry:
  storage:
    driver: os                # or s3
    os: { root: /var/lib/cr/data }

  proxies:
    - prefix: docker.io
      upstream: https://registry-1.docker.io
      hosts: [cache.example.com]  # a request with neither prefix nor ns is Docker Hub's
      auth:                       # a Docker Hub account raises its pull limit
        kind: password
        username: someone
        password: ${file:/run/credentials/dockerhub}  # an access token, dckr_pat_...
      retention: 720h
    - { prefix: ghcr.io,           upstream: https://ghcr.io,           retention: 720h }
    - { prefix: nvcr.io,           upstream: https://nvcr.io,           retention: 720h }
    - { prefix: quay.io,           upstream: https://quay.io,           retention: 720h }
    - { prefix: registry.k8s.io,   upstream: https://registry.k8s.io,   retention: 720h }
    - { prefix: gcr.io,            upstream: https://gcr.io,            retention: 720h }
    - { prefix: mcr.microsoft.com, upstream: https://mcr.microsoft.com, retention: 720h }
    - { prefix: public.ecr.aws,    upstream: https://public.ecr.aws,    retention: 720h }

  gc:
    every: 1h
    untagged: 168h
    full_every: 168h
```

**A prefix is the registry's name**, exactly as it is in an image reference:
a client mirroring `ghcr.io` asks for `lesomnus/cr` with `ns=ghcr.io`, and the
proxy whose prefix is `ghcr.io` answers. A registry with no proxy here is
`404`, and the client goes to it directly.

**`hosts`** is for the one client that sends no `ns`: Docker on its classic
image store, which mirrors Docker Hub alone. With it, the deployment is a cache
and nothing else -- a push to a name with no prefix is a push to `docker.io/`,
which a cache refuses. Leave it out, and give Docker Hub a host of its own,
if the same deployment also takes pushes.

**Credentials** are only for what an upstream will not give anyone: a Docker
Hub account for its limits, and `username: $oauthtoken` with an NGC API key
for private images on `nvcr.io`. Public images on every registry above pull
without one. `${file:...}` keeps the secret out of the configuration and is
re-read when it is rotated, and `${env:...}` works too; see
[operating.md](operating.md#credentials).

**`retention`** is how long a tag or manifest nobody pulled stays; see
[operating.md](operating.md#pull-through-caches) for the two rules and
[operating.md](operating.md#garbage-collection) for `gc`.

`cr.auth.yaml`, beside `cr.yaml`:

```yaml
permissions:
  pull:
    repos: ["**"]
    actions: [pull]
matches:
  everyone:
    for: anyone
    grant: [pull]
```

Without a policy file every request is allowed, deleting from the cache and
`/admin/gc` included. This one allows pulls and nothing else;
`cr auth check cr.auth.yaml` checks it, and [access.md](access.md) adds
callers who may do more.

Running it from the image, which runs as UID 65532:

```sh
install -d -o 65532 -g 65532 /srv/cr
docker run -d --name cr --restart unless-stopped -p 5000:5000 \
  -v /etc/cr:/etc/cr:ro -v /srv/cr:/var/lib/cr \
  ghcr.io/lesomnus/cr:edge --config /etc/cr/cr.yaml serve
```

**In front of it**, TLS for `cache.example.com`, and the `Host` the client
sent passed on as it is -- `hosts` compares it, and the token endpoint a
client is sent to is built from it:

```nginx
server {
  listen 443 ssl;
  server_name cache.example.com;
  client_max_body_size 0;
  location / {
    proxy_pass http://127.0.0.1:5000;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto https;
    proxy_buffering off;
    proxy_read_timeout 900s;
  }
}
```

## Docker

`docker info -f '{{.DriverStatus}}'` says which image store a daemon is on:
`io.containerd.snapshotter.v1` is containerd's, the default on a new install
of Docker 29.

**Docker Hub**, on either store, in `/etc/docker/daemon.json`:

```json
{ "registry-mirrors": ["https://cache.example.com"] }
```

**Every other registry**, on the containerd image store and Docker 28 or
later, in `/etc/docker/certs.d/_default/hosts.toml`:

```toml
[host."https://cache.example.com"]
  capabilities = ["pull", "resolve"]
```

`_default` is every registry without a directory of its own under `certs.d`;
one that cr does not cache answers `404`, and the pull goes to the registry.
For some registries only, put the same file in `certs.d/ghcr.io/hosts.toml`
and so on instead. Restart the daemon after either change.

On the classic store Docker mirrors Docker Hub and nothing else. Other
registries go through cr only by naming it:
`docker pull cache.example.com/ghcr.io/lesomnus/cr:edge`.

`docker build` with the default builder uses the daemon's configuration.

## containerd

The same `hosts.toml`, under containerd's own directory, which its
configuration has to name:

```toml
# /etc/containerd/config.toml, containerd 2
[plugins."io.containerd.cri.v1.images".registry]
  config_path = "/etc/containerd/certs.d"
```

```toml
# /etc/containerd/certs.d/_default/hosts.toml
[host."https://cache.example.com"]
  capabilities = ["pull", "resolve"]
```

## BuildKit

A builder of its own -- `docker buildx create --driver docker-container`, or
buildkitd anywhere -- reads `buildkitd.toml`, one entry per registry:

```toml
[registry."docker.io"]
  mirrors = ["cache.example.com"]
[registry."ghcr.io"]
  mirrors = ["cache.example.com"]
[registry."nvcr.io"]
  mirrors = ["cache.example.com"]
[registry."quay.io"]
  mirrors = ["cache.example.com"]
[registry."registry.k8s.io"]
  mirrors = ["cache.example.com"]
```

```sh
docker buildx create --use --driver docker-container --buildkitd-config /etc/buildkitd.toml
```

In GitHub Actions, the same file goes in `docker/setup-buildx-action`'s
`buildkitd-config-inline`.

## Checking it works

Pull something not pulled before, and cr says it fetched it:

```
pull-through: fetched - repo=ghcr.io/lesomnus/cr upstream=https://ghcr.io/lesomnus/cr reference=edge ...
```

A mirror that fails is not an error to a client: Docker, containerd and
BuildKit go to the registry itself and the pull succeeds anyway. No such line
for a pull means it did not come through cr; Docker's daemon log says why
(`Attempting next endpoint`). `cr.cache.requests`, by prefix and outcome, is
the hit ratio ([operating.md](operating.md#health-and-telemetry)).
