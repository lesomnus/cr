# cr

An OCI container registry with per-repository permissions, tag rules,
pull-through caches, and garbage collection that never stops it.

- **The OCI distribution specification v1.1**, referrers included. Its
  conformance suite passes in CI, and Docker, oras, cosign and notation push
  and find images, artifacts and signatures.
- **Access per repository.** Bindings grant actions on repository globs to
  subjects and groups, optionally only for credentials whose claims match, and
  tag rules make tags immutable, protected, patterned or retained.
- **Credentials from where they already are**: htpasswd, static tokens,
  OpenID Connect ID tokens -- so a GitHub Actions workflow pushes with no
  secret -- and [roster](https://github.com/lesomnus/roster).
- **Pull-through caches** that stream while they fill, fetch only what is
  asked for, and keep serving cached tags when the upstream is down.
- **Garbage collection without a read-only window**, and an index the store
  alone can rebuild.
- **A management API** for bindings, tag rules, repositories and collection
  runs. SQLite or PostgreSQL; a local disk or S3.

## Quick start

```sh
docker compose up -d
docker login localhost:5000 -u admin -p admin
docker tag alpine localhost:5000/acme/alpine
docker push localhost:5000/acme/alpine
docker pull localhost:5000/docker.io/library/ubuntu   # through the Docker Hub cache
```

`compose.yaml` builds the image and runs cr on PostgreSQL and MinIO. Blob pulls
are redirected to MinIO, so a client pulling has to reach `localhost:9000` as
well. [docs/operating.md](docs/operating.md#running) starts from the binary
instead.

Every commit on `main` that passes CI is published as `ghcr.io/lesomnus/cr`:
`:edge` follows `main`, and `:YYMMDD-r<run>` is the tag that never moves.

## Documentation

- [How cr works](docs/how-it-works.md) -- the consistency trade, storage, the
  index, garbage collection, and what several replicas take.
- [Operating](docs/operating.md) -- configuration, storage, deploying, garbage
  collection, rebuilding the index, pull-through caches, export.
- [Access](docs/access.md) -- credentials, bindings, tag rules, OpenID Connect,
  roster, the management API.
- [The registry API](docs/registry-api.md) -- endpoints, pushes and deletes,
  artifacts and signatures, errors.
- [Developing](docs/development.md) -- how the code is laid out and generated.

## Why not one of the existing ones

Registries that store each repository as a standalone OCI image layout have to
reunite identical blobs afterwards, which means a digest-to-location index, a
materialisation step on a path that looks like a read, and a background pass to
catch what the request path missed. Registries that store blobs in one global
bucket avoid all of that but give up per-repository deletion, needing a
mark-and-sweep across every repository before any blob can go.

cr keeps blobs in [flob](https://github.com/lesomnus/flob), which stores one
copy of a blob and gives each repository a reference to it, so membership is
per repository while the bytes are shared. On a local disk the reference is a
hard link and the link count is the reference count: a repository can be
deleted on its own, and the storage it alone held comes back immediately. And
no request waits on anything store-wide -- a blob request takes no lock and
reads no index -- which is the failure cr was started to get away from
([#1](https://github.com/lesomnus/cr/issues/1)).

## License

Apache-2.0
