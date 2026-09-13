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
| 5 | [#7](https://github.com/lesomnus/cr/issues/7) mark-and-sweep, rebuild | done; on the compose, full collections every 20 seconds erased stray blobs while 14 pushes to another repository all landed, and `cr index rebuild` from MinIO alone gave back every tag and manifest |
| 6 | [#8](https://github.com/lesomnus/cr/issues/8) pull-through | done; `library/ubuntu` pulled through the compose's Docker Hub cache fetched the index, the amd64 manifest and its attestation and nothing else, and later pulls came from MinIO |
| 7 | [#9](https://github.com/lesomnus/cr/issues/9) OIDC, roster, export, UI | done; GitHub's own ID tokens let `oidc-release.yml` push and refused `oidc-sibling.yml`, and against a real roster `docker login` took an `rt_` key and a password, a pull-only key pushed nothing, and a revoked key stopped working |

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

### Phase 5

38. **The sweep holds the repository lock outside a transaction**
    (`Index.Lock`, now on the port): the session form of the advisory lock on
    PostgreSQL, the process's stripe on SQLite, so it does not take SQLite's
    one writer and other repositories keep writing through a walk.
39. **A blob uploaded during its repository's sweep can be erased** before the
    manifest that names it arrives, and that push fails and retries. flob's
    `Info` carries no time, so a fresh upload cannot be told from a leak; the
    alternative is a read-only window, which the principle rules out.
40. **A full collection sweeps what the store lists and what the index has**,
    so a namespace holding blobs and no manifest is swept too, and a
    repository whose rows point at a store with nothing is reported. A
    repository busy past the bound is tried once more at the end.
41. **Runs are `GcRun` rows**, global and read-only over the API, online runs
    included. A run whose process died stays `running`; its start time says
    how stale it is.
42. **`POST /admin/gc` starts the run in the background** and answers 202 with
    it, or 409 with the one this replica is running. Across replicas the run
    goes to whoever takes the advisory lock, and the other records
    `failed: another replica is collecting`.
43. **`cr gc` refuses on SQLite without `--offline`**, since the lock there is
    the serving process's; on PostgreSQL it takes the server's own locks.
44. **`cr index rebuild` only adds.** A manifest is a blob up to
    `max_manifest_size` that starts with `{` and parses as one; its tags come
    from its labels, the first manifest found wins a tag two of them claim, and
    that is reported. Push and pull times are not in the store and become now
    and never.
45. **Tag labels moved to `blob`** (`blob.LabelTag`), so retention removes the
    label with the tag; otherwise a rebuild would bring back a tag retention
    had deleted.
46. **Found by the compose run: a run could not be recorded.** The collector's
    run rows have required counters, and the collector's own tests used the
    memory recorder, so the first scheduled collection on PostgreSQL failed
    to start. The counters start at zero now, and the ent recorder has a test
    of its own.
47. **Found by the rebuild: S3 joins a label's values.** flob keeps labels as
    object metadata, one header per label, so a manifest's `Tag` label came
    back as `t1,t2,...` and rebuild found no valid tag in it. A tag cannot
    contain a comma, so `blob.TagsOf` splits them, and editing a label reads
    them the same way. S3 caps user metadata at 2 KB, so a manifest with very
    many tags keeps only as many labels as fit; the index is what answers, and
    a rebuild of such a manifest recovers fewer tags.

### Phase 6

48. **A cache is a prefix.** The upstream name is what follows the prefix, or
    `remote` in front of it; an empty prefix makes every repository a cache,
    which is the shape a daemon's `registry-mirrors` wants.
49. **A cache asks upstream for every manifest type cr stores**, whatever the
    client sent, so a tag is one manifest in the cache for every client.
50. **Tag freshness is kept in memory.** A tag is checked again with a `HEAD`
    after `tag_ttl`; when it last was is per process, so a restart or another
    replica checks once more, and nothing about it is stored. A digest is
    never checked again.
51. **A cached tag outlives its upstream.** When the upstream cannot be
    reached, or the fetch fails, a tag already cached is served and the
    failure logged; one never cached is a 500.
52. **A cache takes deletes and no pushes** (`405 UNSUPPORTED`). Tag lists and
    referrers are what the cache holds, not the upstream's.
53. **Only what is asked is fetched, checked on Docker Hub.** The `ubuntu`
    index lists six platforms and six attestation manifests; a `docker pull`
    through the cache fetched the index, the linux/amd64 manifest and the
    attestation the daemon asked for by digest, and no other.
54. **Found by the Docker Hub run: a fill into S3 died with its request.**
    flob ties a cache fill to the context `Open` was given, and `net/http`
    cancels a request's context as its handler returns, before a bucket has
    finished writing what the client already has; no layer reached MinIO. cr
    opens a cached blob with that context's values and not its cancellation,
    and closing the reader still stops a fill that did not read every byte.
    Filed as [flob#24](https://github.com/lesomnus/flob/issues/24).
55. **Found by the tests: a tag taken for a digest.** `digest.Parse` answers
    its input even when it is not a digest, so a fetch by tag was checked
    against the tag as if it were one. A reference is a digest only when it
    has a colon.
56. **A cache's retention counts every use.** A tag's last use is the latest of
    its pull, its move, and a pull of the manifest it points at, and a `HEAD`
    counts as a pull, since that is how a pull resolves a tag.
57. **The daemon mirror was exercised by name.** The Docker engine here is
    shared with other projects, so it was not given `registry-mirrors`; the
    pulls named the cache (`localhost:5000/docker.io/library/ubuntu`), which
    is the same code path with a prefix.

### Phase 7

58. **An ID token's claims are the subject's, for `when`.** OIDC is checked
    offline against the keys the provider publishes, and a binding can name
    any claim of the token, with a glob for its value. GitHub's
    `workflow_ref` is what tells a release workflow from its siblings in the
    same repository; `repository` alone cannot.
59. **The exchange hands out a password, not an access token.**
    `POST /token/exchange` trades a credential for a token cr signs that
    stands for the same subject, claims included, for `auth.exchange.ttl`.
    It is refused as a bearer token, and a token from the exchange cannot be
    exchanged again, or it would never expire.
60. **Checked on GitHub with GitHub's tokens.** `oidc-release.yml` and
    `oidc-sibling.yml` call one reusable workflow that serves cr on the runner
    with a single binding naming the release workflow. Each caller's ID token
    carries its own `workflow_ref`, so the release pushed and the sibling was
    refused, with the ID token and with the exchanged token alike.
61. **roster is spoken to over Connect's JSON, not through its Go module.** cr
    carries none of roster's generated code, so the two need not agree on the
    version of anything but the wire.
62. **Found by the real roster: the data plane, not the control plane.** A key
    made with `roster key add --service` is a row of the control plane, and
    the first reading was that cr calls the control plane with it. That
    listener knows none of the data plane's holders and keys
    (`ApiKey not found`); a service calls the data plane, `server.http`. The
    fake the tests used had been written from the same wrong reading, and was
    rewritten from what the real one answered.
63. **Found by the real roster: introspection names nobody by alias.** The
    data plane answers with the holder's and the tenant's identifiers and
    empty aliases, so `@acme/ci` matched no binding and the management API's
    mirror had no name to put a row up under. cr reads the aliases with
    `HolderService/Get` and `TenantService/Get`, and keeps a name for a
    minute.
64. **A team is `@tenant/site/team`.** A team's alias is unique within its
    site and not within its tenant, so `@tenant/team` could be two teams at
    once. A team in no site, which roster itself names by identifier alone,
    is its identifier.
65. **A roster key is used for what it was made for.** A key lists methods, so
    the registry's actions are given method names, `/cr.Registry/Pull` and so
    on, matched with payday's own rule for method patterns. What a key allows
    narrows what the bindings grant and never widens it, and a key that allows
    none of them is refused at login rather than logging in to be refused
    everything. The narrowing rides in access and login tokens with a
    `narrowed` claim of its own, since a list that allows nothing and a list
    that is not there look alike once `omitempty` is done with them. A
    password is the whole of its holder.
66. **A second factor sends a person to a key.** `docker login` carries a
    username and a password and nothing else, so a holder roster asks a second
    factor of is refused with a message naming `rt_` keys, which
    `roster sign-in` mints.
67. **What roster accepted is remembered for `auth.roster.remember`.** The sync
    stream forgets a holder at once, and a stream that ends forgets everybody,
    so a revoked key stops working within `remember` at the latest.
68. **The management API mirrors roster's tenants and holders.** payday's
    `auth.Remote` introspects a key roster issued on every request, and the
    tenant and holder it names are put up here under roster's identifiers the
    first time they are seen, so bindings can belong to roster's tenants.
69. **Checked against a real roster** (7cca74b, built from source) and the
    shared Docker engine, which reached cr through a forwarder on its host
    network. `docker login` took an `rt_` key and a person's password;
    `@acme/ci` pushed `acme/*` and was refused elsewhere; a member of team
    `devs` in site `main` pushed through `@acme/main/devs`; a pull-only key
    pulled and was refused a push its binding allowed; a key for roster alone,
    a wrong password, an unknown key and a revoked key were refused; and a key
    allowing `/app.*/*` listed repositories and the mirrored holder over the
    management API, where a pull-only key was refused.
70. **`cr export` writes an OCI image layout.** Each tag is a manifest in
    `index.json` named with `org.opencontainers.image.ref.name`, referrers are
    written without a name, and a blob that is not there -- a layer a cache
    never fetched, or a non-distributable one -- is reported rather than
    failing the export.
71. **The management page is cr's own, in `ts/`.** It is built on the
    TypeScript client payday generates, and the sandbox runs it against the
    whole of cr compiled to WebAssembly. registry-ui was not forked: it
    browses a registry through the distribution API and does not manage one.
    Whether cr should also offer a browsing page of that kind is left open.

