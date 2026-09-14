# The registry API

What clients see: the endpoints cr serves, how pushes, pulls and deletes
behave, and how artifacts and signatures are stored and found.

## Conformance

cr implements the [OCI distribution specification](https://github.com/opencontainers/distribution-spec)
v1.1. CI runs the specification's conformance suite on every push, pinned at
`9727462` with every API and data set turned on (`scripts/conformance.sh`). On
2026-09-13 it passed 840 checks, skipped 18 and failed none. The skips are a
mount without credentials, which cr answers with an upload session as the
specification allows, and deleting the constant blobs (below).

## Endpoints

| method | path | |
| --- | --- | --- |
| GET | `/v2/` | `200`, or a challenge naming the token endpoint |
| GET, HEAD | `/v2/<name>/blobs/<digest>` | `Range`; a `307` to the bucket when redirects are on |
| DELETE | `/v2/<name>/blobs/<digest>` | refused while a manifest holds the blob |
| POST | `/v2/<name>/blobs/uploads/` | starts an upload; `?digest=` uploads in one request, `?mount=&from=` mounts |
| PATCH, PUT, GET, DELETE | `/v2/<name>/blobs/uploads/<id>` | a chunk, the last chunk, the upload's progress, cancelling it |
| GET, HEAD, PUT, DELETE | `/v2/<name>/manifests/<reference>` | by tag or by digest |
| GET | `/v2/<name>/tags/list` | `n` and `last`, with `Link` |
| GET | `/v2/<name>/referrers/<digest>` | `?artifactType=` filters |
| GET | `/v2/_catalog` | `n` and `last`; only what the caller may pull |
| GET | `/v1/_ping`, `/v1/search` | what `docker search` asks |
| GET, POST | `/token` | the distribution token flow |
| POST | `/token/exchange` | a credential traded for one cr signs; see [access.md](access.md#ci-without-secrets-openid-connect) |
| GET | `/.well-known/jwks.json` | the keys tokens are signed with |
| GET, POST | `/admin/gc`, `/admin/gc/<id>` | garbage collection runs; see [operating.md](operating.md#garbage-collection) |
| GET | `/healthz`, `/readyz` | whether the process runs, and whether the database answers |

The management API is served beside these; see
[access.md](access.md#the-management-api).

## Pushing

**Manifests.** cr stores OCI image manifests and indexes, and Docker's v2
manifests and manifest lists. Docker's schema 1 is `MANIFEST_INVALID`, and a
manifest larger than `registry.max_manifest_size` (4 MiB) is `SIZE_INVALID`.
Every blob and child manifest a manifest names must already be in the
repository, or the push is `MANIFEST_BLOB_UNKNOWN` with the missing digests in
`detail`; a pull-through cache is the exception. A `subject` need not exist
yet, since a signature may arrive before its image, and a push that names one
is answered with `OCI-Subject`, which tells a client the referrers API is
there.

**Tags.** A push by tag is checked against the tag rules before anything is
written (see [access.md](access.md#tag-rules)). A tag that another push moves
between the check and the write is checked again, so a race cannot slip past a
rule.

**Blobs.** A blob is pushed in one request or in chunks. A chunk at the wrong
offset is `416` with the current `Range`. An upload nobody appends to for
`registry.storage.upload.ttl` (24 hours) expires, and a final `PUT` that is
retried gets the same answer for `upload.retention`. sha256, sha384 and sha512
digests are accepted. A chunked upload whose algorithm is named only by its
final `PUT` is hashed twice; a client that says `?digest-algorithm=` on the
`POST` avoids that.

**Mounts.** A mount needs `push` on the target and `pull` on the source.
Without the second, or when the source lacks the blob, it becomes an ordinary
upload, as the specification provides. Between repositories on different
stores it is a copy.

## Pulling

A manifest is answered as it was pushed, with its media type in `Content-Type`,
whatever `Accept` names: cr never converts between formats, so there is nothing
to choose, and a client that cannot use the type learns it from the header.
Manifests and `HEAD` requests are always answered by cr; only a blob `GET` is
redirected to a bucket.

## Deleting

- **A tag.** `DELETE /v2/<name>/manifests/<tag>` removes the tag and nothing
  else.
- **A manifest.** By digest, it removes the manifest and every tag pointing at
  it -- refused when a tag rule forbids deleting any of them -- and then the
  blobs no other manifest in the repository holds.
- **A blob.** Refused with `DENIED` while a manifest in the repository holds
  it, and when the digest is a manifest's.
- **The constant blobs** are `405 UNSUPPORTED`. `{}`, Docker's empty layer, the
  empty tar and the empty blob are answered from memory in every repository, so
  a delete would be contradicted by the next `HEAD`.
  `registry.disable_well_known` stores them like any other blob.

In a pull-through cache a delete evicts.

## Artifacts, signatures and SBOMs

A manifest with a `subject` is a referrer of that subject, listed by
`GET /v2/<name>/referrers/<digest>` and filtered by `?artifactType=`, which the
answer confirms with `OCI-Filters-Applied: artifactType`. Referrers are
untagged manifests, and the collection keeps them for as long as their subject
is in the repository. Deleting one takes it off the list.

Checked on 2026-09-14 with oras 1.3.4, cosign 3.1.3 and notation 1.3.2 against a
multi-platform image, before and after a full collection:

| client | what happened |
| --- | --- |
| `oras attach`, `discover`, `pull` | attaches through the referrers API; nested referrers and `--artifact-type` list as expected |
| `cosign sign`, `attest`, `verify`, `verify-attestation`, `tree` | cosign 3 stores signatures and attestations as referrers, with or without `--registry-referrers-mode=oci-1-1` |
| `notation sign`, `ls`, `verify` | uses the referrers API, and by default also pushes a `sha256-<digest>` tag, which `--force-referrers-tag=false` leaves out |

A scheme that keeps signatures under tags -- notation's default, cosign 2's
`sha256-<digest>.sig` and `.att` -- works, because those are ordinary tags, and
for the same reason needs the `tag` action and meets the tag rules: a `pattern`
rule refuses such a tag, and an `immutable` rule refuses the move that the next
signature makes. With notation, `--force-referrers-tag=false` avoids both.

A pull-through cache takes no pushes, so a cached image is signed after copying
it into a repository that is not a cache, with `oras copy` or the like.

## Listing and search

When the registry is guarded, `/v2/_catalog` and `/v1/search` need the
`catalog` and `search` actions from a binding over every repository (`*`), and
answer only with repositories the caller may pull. `docker search
cr.example.com/term` asks `/v1/_ping` and then `/v1/search`, which answers in
Docker Hub's shape. A repository's description is its `desc`, set with
`cr repository patch`, or else the `org.opencontainers.image.description`
annotation of the manifest its most recently moved tag points at.

## Errors

Errors come in the specification's JSON envelope, with its codes. A few answers
are worth knowing:

- **`503` with `Retry-After`**: a write waited `registry.lock_wait` for its
  repository, which is being swept or written. Retry.
- **`401` with `error="insufficient_scope"`**: the token was never asked for the
  action, and a client fetches one that asks. **`403 DENIED`**: the action was
  asked for and refused, and `detail` names it.
- **`405 UNSUPPORTED`**: a push to a pull-through cache, or a delete of a
  constant blob.
