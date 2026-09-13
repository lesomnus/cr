# cr

An OCI container registry.

Storage is a content-addressable store — [flob](https://github.com/lesomnus/flob) —
rather than a directory tree shaped like the OCI image layout. Blobs are kept once
and referenced by every repository that holds them, so there is no deduplication
pass to run and nothing that has to hold a lock to answer whether a blob exists.

Pull-through caching streams: a blob is served to the client while it is being
written to the cache, not after.

Status: design done, implementation starting. The storage model is argued in
[issue #1](https://github.com/lesomnus/cr/issues/1); what is built on it, and
in what order, is [docs/plan.md](docs/plan.md).

## Why not one of the existing ones

Registries that store each repository as a standalone OCI image layout have to
reunite identical blobs afterwards, which means a digest-to-location index, a
materialisation step on a path that looks like a read, and a background pass to
catch what the request path missed. Registries that store blobs in one global
bucket avoid all of that but give up per-repository deletion, needing a
mark-and-sweep across every repository before any blob can go.

flob keeps one copy of a blob under `share/` and gives each namespace a hard link
to it, so membership is per-repository while the bytes are shared, and the link
count is the reference count — a repository can be deleted on its own and the
storage it alone held is reclaimed immediately.

## License

Apache-2.0
