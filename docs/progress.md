# Progress

What has been built against [the plan](plan.md), phase by phase, and the
decisions implementation forced that the plan did not settle. Each phase's
checklist lives in its issue; this page is the running record beside them.

## Status

| phase | issue | state |
| --- | --- | --- |
| 0 | [#2](https://github.com/lesomnus/cr/issues/2) scaffold | done |
| 1 | [#3](https://github.com/lesomnus/cr/issues/3) blobs, manifests, tags | done |
| 2 | [#4](https://github.com/lesomnus/cr/issues/4) referrers, catalog, deletes, mount | done, Postgres lock untested until phase 4 |
| 3 | [#5](https://github.com/lesomnus/cr/issues/5) auth and policy | not started |
| 4 | [#6](https://github.com/lesomnus/cr/issues/6) operations | not started |
| 5 | [#7](https://github.com/lesomnus/cr/issues/7) mark-and-sweep, rebuild | not started |
| 6 | [#8](https://github.com/lesomnus/cr/issues/8) pull-through | not started |
| 7 | [#9](https://github.com/lesomnus/cr/issues/9) OIDC, roster, export, UI | not started |

## Conformance

`scripts/conformance.sh` builds cr, serves it on SQLite and a directory, and
runs `opencontainers/distribution-spec/conformance` at `9727462` with every
API and data set the suite has turned on, plus the digest-header and
upload-cancel checks that are off by default.

| date | result |
| --- | --- |
| 2026-09-13 | **Pass**: 840 pass, 18 skip, 0 fail. The skips are anonymous mount, which cr answers with a session as the spec allows, and blob delete of the constant blobs (below). |

## Decisions made while implementing

Each of these is a place the plan was silent or had to bend. They are stated
the way the code does them; where the plan now reads differently, the plan was
changed to match.

### Phases 0–2

1. **The conformance suite is not four workflows any more.** It was redesigned
   in April 2026 (`a221f94`): one binary, with each API and data set a switch.
   The plan's "turn on pull, push, discovery, management one phase at a time"
   became "everything on from the first run", because everything passed by
   the end of phase 2, and the suite is pinned by commit in the script.
2. **Generated messages live in `api/`**, not at the module root
   (`option go_package = "github.com/lesomnus/cr/api"`). The root would have
   held forty generated files beside `oci/` and `registry/`.
3. **The hot tables are four entities**: `Repository` (domain 8),
   `Manifest` (9), `ManifestBlob` (10), `Tag` (11), each `global`, hard
   erase, UUID key minted with the entity's domain by `entindex`, unique index
   on the natural key. `Tag` carries `date_moved` apart from the version
   `date_updated`, so a pull or a label never reorders retention.
4. **`Index.Tx` takes the repository** (`Tx(ctx, repo, fn)`), so a write that
   needs the lock cannot forget to take it. An empty repository is a
   transaction without a lock.
5. **A manifest push stats its blobs inside the repository lock.** The plan
   said the lock has no I/O inside it; that turned out to be the race it
   exists to close. Blob delete (and the phase 5 sweep) erase under the same
   lock, so a blob that a push saw cannot vanish before the push commits, and
   one erased before the push looked is reported as `MANIFEST_BLOB_UNKNOWN`
   instead of being lost. The stats run eight at a time; a child manifest of
   an index is checked in the index, not the store.
6. **Delete by digest takes the manifest's tags with it** instead of being
   refused while tags point at it. That is what distribution and zot do, and
   the conformance suite deletes that way when tag delete is off. Tag rules
   (phase 3) are checked for each tag it removes.
7. **Releasing blobs is a second transaction.** Delete commits the index change
   first, then takes the lock again, checks each released digest is still
   unreferenced, and erases it. A push that landed in between keeps its blobs;
   a crash in between leaks to the sweep.
8. **Constant blobs cannot be deleted**: `DELETE` of `{}`, the empty layer,
   the empty tar or the empty blob is `405 UNSUPPORTED`. They are answered from
   memory in every repository, so a `202` would be contradicted by the next
   `HEAD`; the suite counts the `405` as a skip.
9. **A chunked upload whose algorithm is named only at the final `PUT`** is
   hashed twice. flob fixes a session's algorithm at `Begin`, and the spec
   gives a client no way to say it before the end, so cr begins with sha256;
   if the `PUT` names sha512, cr commits under the sha256 digest and reads the
   bytes back in under the sha512 one. The sha256 copy is left for the sweep.
   A client may say `?digest-algorithm=` on the `POST` and skip all of it.
10. **The lock on SQLite is in the process**: a striped per-repository lock
    plus one writer at a time, since two SQLite transactions that both upgrade
    to write deadlock. On Postgres it is `pg_advisory_xact_lock` under
    `SET LOCAL lock_timeout`, and a timeout is `503` with `Retry-After`.
11. **Names are validated with the spec's own grammar**, not
    `distribution/reference`, whose name grammar admits a hostname and port.
12. **`Accept` refuses only a request that names manifest types and not the
    stored one**; one that names none, or only `application/json`, gets the
    manifest.
13. **A blob `DELETE` of a digest that is a manifest** is `DENIED`, pointing at
    the manifests endpoint, since erasing the bytes would leave the row.
14. **CI builds the image and does not push it.** Publishing is a release
    decision.
