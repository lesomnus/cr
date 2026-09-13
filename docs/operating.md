# Operating cr

How a deployment is configured and run. [plan.md](plan.md) says why things are
the way they are; this page says how to use them.

## Running

```sh
cr --config cr.yaml init     # the operator tenant and its first holder
cr --config cr.yaml serve
```

`cr config` prints the configuration as it was read, and `cr config env` lists
every `CR_*` variable that overrides it. The image runs `cr serve` and exposes
the registry on port 5000.

`init` puts up the tenant `operator` and the holder `admin` in it by default
(`--tenant`, `--holder`). They own what the management plane writes: bindings
and tag rules.

## Storage

```yaml
registry:
  storage:
    driver: os          # or memory, for a throwaway
    os:
      root: /var/lib/cr/data
    upload:
      ttl: 24h          # an upload nobody appends to
      retention: 24h    # a finished upload's receipt
  max_manifest_size: 4194304
  lock_wait: 30s        # a manifest write waiting for its repository
```

The database is payday's `db:` block: SQLite for one process, PostgreSQL for
more. The registry is served on `server.http.addr`, which must be set.

## Who may do what

With no `auth:` block the registry is open: every request may pull, push and
delete, and the log says so at startup. Anything configured below turns the
guard on; `auth.enabled: true` turns it on when every binding is a row.

```yaml
auth:
  htpasswd:
    path: /etc/cr/htpasswd      # htpasswd -B; read again when it changes
    groups:
      alice: [release]
  static:                       # long-lived tokens, for CI without roster
    - name: ci
      token_sha256: 5e884898da28047151d0e56f8dc6292773603d0d6aabbdd62a11ef721d1542d8
      groups: [ci]
  token:
    service: cr.example.com     # the tokens' audience; `cr` by default
    realm: https://cr.example.com/token
    keys: [/etc/cr/token.pem]   # P-256; the first signs, all verify
    ttl: 5m
  bindings:
    - subject: anonymous        # everyone
      repo: "library/*"
      actions: [pull]
    - group: ci
      repo: "acme/*"
      actions: [pull, push, tag]
    - subject: alice
      repo: "*"
      actions: ["*"]
  tag_rules:
    - name: releases
      repo: "*"
      tag: "v*"
      kind: immutable
```

**Credentials.** `docker login` sends a username and password. htpasswd users
are checked with bcrypt; a static token is accepted as the password with any
username. The registry answers `/v2/` with a Bearer challenge, clients fetch a
token from `/token` with their credentials, and every later request is checked
offline against that token. `/v2/` also takes Basic directly. The public keys
are at `/.well-known/jwks.json`.

Without `token.keys` a key is made at startup. Tokens then die with the
process and no second replica accepts them, so a real deployment names one:

```sh
openssl ecparam -name prime256v1 -genkey -noout -out token.pem
```

**Bindings** add and never subtract. A subject is what an authenticator names;
`anonymous` is every caller, credential or not; the group `authenticated` is
every caller whose credential checked. A repository glob's `*` matches any run
of characters, slashes included. The actions are `pull`, `push`, `delete`,
`tag` (create or move a tag; a push without it is by digest only), `catalog`,
`search`, `admin`, and `*`. `catalog`, `search` and registry-wide `admin` come
only from a binding whose repository is `*`. A mount needs `push` on the target
and `pull` on the source, or it becomes an ordinary upload.

**Tag rules** are checked on a manifest push and delete before anything is
written:

| kind | refuses |
| --- | --- |
| `immutable` | moving or deleting a tag once it is set; pushing the same digest again is fine |
| `protected` | moving or deleting, unless the caller has `admin` or is in `groups` |
| `pattern` | creating or moving a tag that does not wholly match `pattern` (RE2) |
| `retention` | nothing at push; garbage collection keeps the newest `keep` |

A delete by digest removes the tags pointing at the manifest, and is refused
when any of them may not be deleted. A `pattern` that does not compile refuses
every tag it covers.

Rows add to the configuration. `cr` talks to the management API in-process on
the host, as the holder `management.as` names (`@operator/admin` by default):

```sh
cr binding add @operator/ci-push '{"group": "ci", "repo": "acme/*", "actions": ["pull", "push", "tag"]}'
cr tag-rule add @operator/semver '{"repo": "acme/*", "tag": "*", "kind": "pattern", "pattern": "v\\d+\\.\\d+\\.\\d+|latest"}'
cr binding ls -o table
cr binding erase @operator/ci-push
```

The registry reads rows again every `auth.refresh` (five seconds). A decision
never waits on the database: requests read a snapshot, and when a reload
fails, the snapshot in force stays in force.

## The management API

Every entity service, over gRPC on `server.addr` and over HTTP beside the
registry when `server.http.allow_web` is on. It takes bearer tokens from
configuration, each acting as a payday holder:

```yaml
management:
  tokens:
    - holder: "@operator/admin"
      token_sha256: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
```

With no tokens nothing over the network may use it; `cr <entity> ...` on the
host still can. Repositories, manifests and tags are read-only there: the
registry writes them, and a row the API added or erased would disagree with
the store.
