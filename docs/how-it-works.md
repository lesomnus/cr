# How cr works

What a deployment can rely on, and what cr trades for it.
[operating.md](operating.md) says how to configure each part.

## Availability over strong consistency

cr started from a registry that took a store-wide exclusive lock on every blob
`HEAD` and kept clients waiting on disks that were idle
([#1](https://github.com/lesomnus/cr/issues/1)). So one rule outranks the rest:
a request never waits on anything store-wide.

- **Blob requests touch the store and nothing else.** `HEAD`, `GET`, uploads
  and their chunks take no lock, open no transaction and read no index. Reads
  that need names -- a tag, a list, referrers -- read the index and lock
  nothing.
- **Leaks are tolerated and swept later; losses are not.** Every step that
  reclaims space removes a reference the index no longer has, so a race can
  leave bytes behind and never takes away bytes something still points to.
- **One thing is serialized: the writes that change what a repository
  references**, one repository at a time. A manifest push or delete, a blob
  delete and that repository's sweep take the repository's lock; nothing else
  does, and no other repository notices. A write that would wait longer than
  `registry.lock_wait` (thirty seconds) is answered `503` with `Retry-After`.
- **Bookkeeping stays off the request.** When a tag or a manifest was last
  pulled is queued and written in batches; losing a batch nudges a retention
  decision and nothing else.

There is no store-wide lock and no read-only window, for garbage collection or
anything else.

## Storage

Blobs and manifests are kept in [flob](https://github.com/lesomnus/flob), a
content-addressed store: each digest is stored once, and each repository is a
namespace that references it. A repository sees only what was pushed or
mounted into it, while identical layers take the space of one.

- **`os`** keeps one copy of a blob and gives each repository a hard link to
  it, so the link count is the reference count: when the last repository
  holding a blob lets go of it, the space comes back at once. Writes of one
  digest are serialized with a file lock, which is why it wants a local
  filesystem.
- **`s3`** keeps one object per digest, `blob/<algo>/<hex>`, and a small marker
  per repository, `refs/<algo>/<hex>/<repository>`. Deleting removes the
  marker and leaves the shared object, and nothing yet removes shared objects
  that no marker refers to: on S3, deleting images and collecting garbage stop
  them being served, and do not shrink the bucket. An upload in progress is a
  record in the bucket, written with conditional requests, so any replica can
  continue it.

A blob `GET` can be answered with a redirect to a presigned URL on S3, so the
bytes go from the bucket to the client. A handful of constant blobs -- `{}`,
the config of every OCI artifact and signature, Docker's empty layer, the empty
tar and the empty blob -- are answered from memory in every repository and
never reach the store, unless `registry.disable_well_known` says otherwise.

## The index

Names live in a database: repositories, manifests with their media type,
artifact type and subject, the blobs each manifest holds, and tags. It is
derived from what the store holds, and it is what answers for names.

- **A push writes the store first, then the index.** A crash in between leaves
  bytes no manifest names, which the collection finds. A delete changes the
  index first and erases after, so a tag never points at nothing.
- **What a manifest holds is recorded**, so deleting one releases exactly the
  blobs no other manifest in the repository still holds, and a blob `DELETE` is
  refused while a manifest holds the blob.
- **Referrers are a query** over the manifests whose subject is a digest, and
  the referrers API answers from rows without opening manifests.
- **The store can rebuild it.** Every tag is also written as a label on its
  manifest in the store, so `cr index rebuild` recovers repositories,
  manifests, what they hold and their tags from the store alone. What the store
  does not keep -- when things were pushed and pulled, descriptions, bindings
  and tag rules -- a rebuild cannot bring back.

## Garbage collection

Two tiers, and neither stops the registry.

- **Online**, on a timer: expired uploads, tags past a `retention` rule, and
  untagged manifests past a grace period. Untagged is not unused: a manifest is
  kept while a pull has touched it within the grace period, while an index
  holds it, and while it refers to a subject still in the repository, which is
  what keeps signatures, attestations and SBOMs.
- **Full**, when scheduled or asked for: the online collection, then a
  mark-and-sweep of each repository in turn under that repository's lock. It
  reclaims what the online collection leaked, while pulls, uploads and every
  other repository carry on. The one cost: a blob uploaded to a repository
  during its sweep, before the manifest naming it arrives, can be erased, and
  that push fails with `MANIFEST_BLOB_UNKNOWN` and uploads again. The store
  records no time for a blob, so without a read-only window a fresh upload
  cannot be told from a leak.

## Who may do what

- **cr stores no passwords of its own.** Authenticators name the caller: an
  htpasswd file, tokens in the configuration, OpenID Connect ID tokens, roster,
  or a token cr exchanged for one of those.
- **Authorization is cr's.** Bindings grant actions on repositories to a
  subject or a group, optionally only when the credential's claims match; they
  only add, and nothing denies. Tag rules constrain what happens to tags,
  whoever asks.
- **Decisions never wait on the database.** Bindings and tag rules, from the
  configuration and from the database, are loaded into a snapshot every
  `auth.refresh`, and requests read the snapshot. A reload that fails keeps the
  policy in force.
- **Tokens are verified offline.** cr signs ES256 JWTs saying what was granted
  and what was refused, and publishes the keys.

## One process or several

Several replicas are supported on **PostgreSQL and S3**, and on nothing else.

- On PostgreSQL the repository lock is an advisory lock, so replicas writing
  one repository wait on the database, and the collection and the other
  background work run on whichever replica takes the lock for that run.
- On S3 an upload that a load balancer sends to another replica continues
  there, and two replicas filling a cache with the same blob both succeed.
- Tokens verify on any replica that has the same `auth.token.keys`.

With SQLite, the lock and the choice of who collects are inside the process,
so exactly one process may use a database. The `os` store on a shared
filesystem is not a way to scale out either: its per-digest file locks and link
counts cannot be trusted on NFS.

## What cr does not do

Replicate between registries, scan for vulnerabilities, or convert between
manifest formats.
