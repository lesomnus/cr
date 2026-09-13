# Progress

What has been built against [the plan](plan.md), phase by phase, and the
decisions implementation forced that the plan did not settle. Each phase's
checklist lives in its issue; this page is the running record beside them.

## Status

| phase | issue | state |
| --- | --- | --- |
| 0 | [#2](https://github.com/lesomnus/cr/issues/2) scaffold | done |
| 1 | [#3](https://github.com/lesomnus/cr/issues/3) blobs, manifests, tags | done |
| 2 | [#4](https://github.com/lesomnus/cr/issues/4) referrers, catalog, deletes, mount | done; the Postgres lock tested in phase 4 |
| 3 | [#5](https://github.com/lesomnus/cr/issues/5) auth and policy | done; checked with `docker login`, push and a refused tag move against a container |
| 4 | [#6](https://github.com/lesomnus/cr/issues/6) operations | done; the compose of cr on MinIO and PostgreSQL pushed, pulled through a 307 to MinIO, and answered `docker search` |
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

### Phase 3

15. **No `visibility` on a repository.** Public is a binding to `anonymous`,
    which is the plan's own model; a second switch meaning the same thing
    would be two answers to one question.
16. **`anonymous` is everyone.** A binding to it applies to every caller, with
    or without a credential, so logging in never takes away what a public
    repository allowed. The group `authenticated` is every caller whose
    credential checked.
17. **Registry-wide actions come only from a binding over `*`.** `catalog`,
    `search` and `admin` outside a repository are granted by a matching
    binding whose repository glob is exactly `*`; `acme/*` with `admin` is
    admin in `acme/*` and nowhere else.
18. **A glob's `*` crosses slashes.** `acme/*` covers `acme/team/app`.
    Namespaces nest, and a pattern that stopped at a slash would need a second
    syntax to say what everybody means.
19. **Tokens carry what was refused.** Clients ask for scopes as they go:
    pull for the checks, then pull and push for the write. A token missing an
    action it was never asked for is answered 401 with
    `error="insufficient_scope"`, so the client fetches one that asks; an
    action that was asked for and refused is 403 `DENIED` with the missing
    action in the detail. Anonymous callers get 401 either way. Found by the
    first `docker push` against the container, which a pull-only token had
    turned into a denial.
20. **`/v2/` challenges a request with no credential**, even where anonymous
    pulls are allowed, since that answer is how a client learns the token
    realm and how `docker login` checks a password. Any valid credential,
    an anonymous token included, gets 200.
21. **A push asks for `tag` implicitly.** Distribution's clients know nothing
    of it, so the token endpoint adds `tag` to a scope that asks for `push`.
22. **A mount needs `pull` on the source**; without it the request becomes an
    ordinary upload session, which is the spec's answer to a mount that
    cannot happen.
23. **The management API takes bearer tokens from configuration** in place of
    the template's `Plain`, which believes any caller. No tokens is an API
    closed to the network.
24. **`cr <entity> ...` runs the management API in-process** over a pipe, on
    the host's database, acting as `management.as` (`@operator/admin`), so an
    operator needs no token to write the first binding. `cr init` defaults to
    the tenant `operator` to match.
25. **Bindings and tag rules are reloaded on a timer** (`auth.refresh`, five
    seconds) into a snapshot requests read, instead of watching. It is correct
    across replicas without a broker, and a failed reload keeps the policy in
    force.

### Phase 4

26. **MinIO comes from `quay.io/minio/minio`.** The Docker Hub image is gone,
    and `compose.yaml` names the one that still pulls. MinIO also refuses a
    root user shorter than three characters, which the first compose run
    found.
27. **Untagged is not unused.** A manifest nothing tags is kept while a pull
    has touched it within the grace period, since deployments pin digests; a
    referrer is kept while its subject is in the repository, and an index's
    child while the index holds it. The pull times are the batched ones, so a
    pull in the last few seconds before a run may not count.
28. **Retention deletes a tag only when every rule matching it agrees.** A
    rule keeping `v*` protects releases from a rule keeping the newest ten of
    `*`, and an `immutable` tag is never deleted by retention.
29. **One release path.** The registry's delete and the collector share
    `gc.Release`: under the repository lock, each digest is checked again for
    a holder or a manifest of that name before it is erased. It does not skip
    the constant blobs any more: erasing a stored copy of `{}` is harmless
    when cr answers it from memory, and correct when it does not.
30. **One runner per run, not per process.** Each replica ticks, and the run
    goes to whoever takes `pg_try_advisory_lock` first; a replica that dies
    mid-run leaves the lock with its connection. On SQLite there is one
    process and it always runs.
31. **The repository lock exists outside a transaction too**
    (`entindex.Lock`): the session form of the same advisory lock on
    PostgreSQL, the process's stripe elsewhere. Nothing uses it yet; the
    phase 5 sweep holds it across a walk of the store without holding a
    transaction, or, on SQLite, the one writer.
32. **A redirect is for `GET` only, and a failed signature streams.** `HEAD`
    stays a stat, a manifest is always cr's, and a store behind a cache that
    does not yet hold a blob answers `ErrNotExist` to the signature, so the
    registry streams and the cache fills.
33. **Routes match at a slash**, the empty prefix is required, and a
    namespace a pool holds for a route that has since changed is not listed
    as the router's.
34. **Search needs `search` from a binding over `*`** and shows what the
    caller may pull; its description falls back to the image's own
    annotation. `/v1/_ping` says the registry is standalone, so the Docker
    CLI authenticates to cr rather than to an index.
35. **Requests carry the server's telemetry context.** The template's HTTP
    server had none, so a handler's log went nowhere; `BaseContext` now hands
    requests the context the server was built on, without its cancellation.
36. **`watch.broker` is `postgres`**, the name payday registers, not `pg`.
37. **The PostgreSQL tests run on a schema each** (`CR_TEST_POSTGRES`), so
    they need one database and nothing else, and they run in their own CI
    job.

