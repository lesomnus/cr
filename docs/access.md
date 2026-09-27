# Access

Who may use the registry and what they may do: providers, permissions and
matches, the globs they are written in, tokens, tag rules, and the management
API.

## Turning the guard on

With no `auth:` block the registry is open: every request may pull, push and
delete, and the log says so at startup. Any provider, permission, match or tag
rule turns the guard on, and `auth.enabled: true` turns it on when every tag
rule is a row in the database.

```yaml
auth:
  token:
    keys: [/etc/cr/token.pem]   # P-256; the first signs, all verify
    ttl: 5m
  exchange:
    ttl: 1h

  providers:
    github:
      kind: oidc
      issuer: https://token.actions.githubusercontent.com
      audience: cr.example.com

  permissions:
    library-read:
      repos:
        - "library/**"
        - "!library/busybox"
      actions: [pull]
    cr-release:
      repos:
        - "lesomnus/cr"
        - "lesomnus/cr/**"
      actions: [pull, push, tag]

  matches:
    public:
      for: anyone
      grant: [library-read]
    cr-release:
      for: github
      grant: [cr-release]
      when:
        repository_id: "123456789"
        workflow_ref: "lesomnus/cr/.github/workflows/release.yml@refs/heads/main"

  tag_rules:
    - name: releases
      repo: "**"
      tag: "v*"
      kind: immutable
```

Three things, each by name:

- **A provider** vouches for a caller: who they are, and what their credential
  says about them.
- **A permission** is actions on repositories.
- **A match** grants permissions to the callers a provider vouches for, when
  their credential says what `when` asks.

What a caller may do is the union of what every match it is under grants.
Matches only add; a caller under none may do nothing.

The configuration is checked when `cr serve` starts, and it does not start
when something does not check: a provider of a kind there is not, two
providers with one issuer, a match `for` a provider there is not or that grants
a permission there is not, a match for a provider with no `when` or for
`anyone` with one, an action there is not, a glob that does not parse, or a
permission whose every pattern takes away.

## Providers

```yaml
providers:
  github:
    kind: oidc
    issuer: https://token.actions.githubusercontent.com
    audience: cr.example.com
    subject_claim: sub          # the default
```

The name is what a match's `for` names it by, and what goes in front of every
subject it vouches for in the logs: `github:repo:acme/app:ref:refs/heads/main`.
It is lowercase letters, digits, `-` and `_`, and it is not `anyone`.

`oidc` is the kind there is. A caller gives an ID token the provider issued as
the password, and cr checks it offline, against the keys the provider
publishes: its signature, its `iss`, that its `aud` is `audience`, and that it
has not expired. `audience` is required: without it, a token the provider
issued for any other service would be good here. Every claim of the token is
the caller's, for a match's `when`.

## Permissions

```yaml
permissions:
  library-read:
    repos:
      - "library/**"
      - "!library/busybox"
    actions: [pull]
```

`repos` are globs read in order, and the last that matches a repository
decides, as in a `.gitignore`: one with `!` in front takes back what it
matches, and one after that can give it again. A permission is read on its
own, and what it gives is what it gives wherever it is granted; a `!` never
takes away what another permission gives.

**The actions** are `pull`, `push`, `delete`, `tag` (create or move a tag; a
push without it is by digest only), `catalog`, `search`, `admin` (move a
protected tag, and the operator's endpoints), and `*` for all of them.
Distribution clients know nothing of `tag`, so a token request that asks for
`push` gets `tag` asked for too.

**`catalog`, `search` and registry-wide `admin` come only from a permission
whose `repos` is `**` and nothing else.** `admin` over `acme/**` moves
protected tags in `acme/**` and reaches nothing registry-wide.

**A mount** needs `push` on the target and `pull` on the source, or it becomes
an ordinary upload.

## Matches

```yaml
matches:
  public:
    for: anyone
    grant: [library-read]
  cr-release:
    for: github
    grant: [cr-release]
    when:
      repository_id: "123456789"
      workflow_ref: "lesomnus/cr/.github/workflows/release.yml@refs/heads/main"
```

- **`for`** is a provider, or `anyone`: every caller, with a credential or
  without, so logging in never takes away what a public repository allows. A
  repository is public by a match for `anyone`; there is no separate
  visibility setting.
- **`grant`** is the permissions granted, by name.
- **`when`** is claims of the credential that must all hold, each value a
  glob. A claim that is a list holds when any of its values does, and a number
  or a boolean is matched as it is written. A match for a provider must have
  one -- without it, every credential the provider issues to anybody is under
  it -- and one for `anyone` cannot, since a caller with no credential has no
  claims.

## Globs

Repositories, tags and the values of `when` are all globs, and a glob is the
same everywhere:

| | |
| --- | --- |
| `*` | any run of characters within one `/`-separated segment, none included |
| `**` | a segment of its own: any number of whole segments -- none in the middle or at the start, at least one at the end |
| `\` | makes the character after it plain: `\*` is a star |
| anything else | itself; a glob matches a name whole, and case matters |

| glob | matches | does not match |
| --- | --- | --- |
| `acme/*` | `acme/app` | `acme`, `acme/team/app` |
| `acme/**` | `acme/app`, `acme/team/app` | `acme` |
| `acme/**/app` | `acme/app`, `acme/x/y/app` | `acme/apps` |
| `**` | every repository | |
| `v*` | `v1`, `v1.2.3` | `1.2.3` |
| `refs/heads/*` | `refs/heads/main` | `refs/heads/feature/x` |

A repository and what is under it is two globs: `acme/app` and `acme/app/**`.

## CI without secrets: OpenID Connect

A job asks its provider for an ID token for cr's audience and gives it as the
password:

```yaml
permissions:
  id-token: write
steps:
  - run: |
      token=$(curl -sS -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
        "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=cr.example.com" | jq -r .value)
      echo "$token" | docker login cr.example.com -u oidc --password-stdin
```

An ID token lives minutes and the Docker CLI replays the stored password on
every push, so a long job trades it first:

```sh
login=$(curl -sS -X POST -H "Authorization: Bearer $token" https://cr.example.com/token/exchange | jq -r .access_token)
echo "$login" | docker login cr.example.com -u oidc --password-stdin
```

What comes back is a token cr signed that stands for the same caller, from the
same provider with the same claims, for `exchange.ttl`. It is a password and
never an access token, and it cannot be exchanged again, or it would never
expire. The exchange also takes the credential as Basic, or as an RFC 8693
`subject_token`.

**What to pin.** cr checks that every claim in `when` holds, and nothing more:
which claims to ask for is the configuration's to say. A match is only as
narrow as its `when`:

- **Identifiers as well as names.** A repository or an owner can be renamed
  and the old name taken by somebody else; `repository_id` and
  `repository_owner_id` cannot, so a `when` naming them stays with the
  repository it was written for.
- **The ref, not only the file.** `workflow_ref` is the workflow file at the
  ref it ran from. With `release.yml@refs/**`, anybody who can push a branch
  can edit `release.yml` on that branch and push images with it; pin
  `@refs/heads/main` or `@refs/tags/v*`, and protect those refs.
- **The caller, or the reusable workflow.** When a workflow calls a reusable
  one, `workflow_ref` names the caller and `job_workflow_ref` names the
  reusable workflow. To trust a shared build workflow wherever it is called
  from, match `job_workflow_ref`.
- **An environment.** `environment: production` makes GitHub's approvals and
  protection rules for that environment a condition too.
- **One match per mapping.** A permission's `repos` and a match's `when` are
  read separately, and nothing carries a claim's value into a repository: a
  permission over `acme/**` granted `for` every `acme` repository lets the
  workflows of each push to all of them. A workflow that should reach only its
  own repository needs a permission and a match of its own.

## Tokens

The registry answers `/v2/` with a challenge naming `/token`, even where
anonymous pulls are allowed, since that answer is how a client learns where
tokens come from and how `docker login` checks a password. Clients fetch a
token from `/token` with their credentials, and every later request is checked
offline against it; `/v2/` also takes Basic credentials directly. A token says
what it grants and what was asked for and refused, so a request for an action
the token was never asked for is answered `401` with `insufficient_scope` --
the client fetches a token that asks -- and one that was refused is `403
DENIED`. The public keys are at `/.well-known/jwks.json`.

```yaml
auth:
  token:
    service: cr.example.com     # the tokens' audience; `cr` by default
    issuer: cr.example.com      # their `iss`; `cr` by default
    realm: https://cr.example.com/token
    keys: [/etc/cr/token.pem]
    ttl: 5m
```

Without `token.keys` a key is made at startup. Tokens then die with the
process and no second replica accepts them, so a real deployment names one:

```sh
openssl ecparam -name prime256v1 -genkey -noout -out token.pem
```

A token cr issued keeps the access it was issued with until it expires, so a
match removed from the configuration stops granting within `auth.token.ttl`
of every replica having restarted without it, and an exchanged token stands
for its caller for `exchange.ttl`.

## Tag rules

Tag rules are checked on a manifest push and delete before anything is
written, whoever the caller is:

| kind | refuses |
| --- | --- |
| `immutable` | moving or deleting a tag once it is set; pushing the same digest again is fine |
| `protected` | moving or deleting, unless the caller has `admin` |
| `pattern` | creating or moving a tag that does not wholly match `pattern` (RE2) |
| `retention` | nothing at push; garbage collection keeps the newest `keep` |

A delete by digest removes the tags pointing at the manifest, and is refused
when any of them may not be deleted. A `pattern` that does not compile, and a
`repo` or `tag` glob that does not parse, refuse every tag they cover, except
in a `retention` rule, which then deletes nothing. Retention deletes a tag only
when every `retention` rule matching it agrees, and never an `immutable` one.
Tags that clients use to store signatures are tags like any other; see
[registry-api.md](registry-api.md#artifacts-signatures-and-sboms).

A `protected` rule's `groups` is from before there were providers: there are no
groups to name, so the configuration refuses one, and a row's is not read.

Tag rules can also be rows in the database, written through the management
API. On the host, `cr` reaches that API in-process as the holder
`management.as` names (`@operator/admin` by default, which `cr init` puts up):

```sh
cr tag-rule add @operator/semver '{"repo": "acme/**", "tag": "*", "kind": "pattern", "pattern": "v\\d+\\.\\d+\\.\\d+|latest"}'
cr tag-rule ls -o table
cr tag-rule erase @operator/semver
```

The registry reads rows again every `auth.refresh` (five seconds). A decision
never waits on the database: requests read a snapshot, and when a reload
fails, the snapshot in force stays in force. The configuration's rules and the
rows are both in force; the configuration's are read once, when `cr serve`
starts, and never become rows.

## The management API

Every entity service, over gRPC on `server.addr`, and -- with
`server.http.allow_web: true` -- over HTTP beside the registry, as Connect and
gRPC-Web. It takes bearer tokens from the configuration, each acting as a
holder:

```yaml
management:
  tokens:
    - holder: "@operator/admin"
      token_sha256: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
```

```sh
curl -sX POST https://cr.example.com/app.TagRuleService/List \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' -d '{}'
```

With no tokens and no roster, nothing over the network may use it; `cr <entity>
...` on the host still can. With `management.roster`, a key
[roster](https://github.com/lesomnus/roster) issued is a caller too, for the
methods it allows -- `/app.TagRuleService/*` and the like -- and the tenant and
holder it names are put up here the first time they are seen, which is how
tag rules come to belong to roster's tenants:

```yaml
management:
  roster:
    url: https://roster.example.com   # roster's data plane over HTTP, its `server.http`
    key: rk_...
```

```sh
roster key add --service cr --allow '/payday.TokenService/Introspect,/roster.HolderService/Get,/roster.TenantService/Get'
```

Tag rules are written through it. Repositories, manifests, tags and collection
runs are the registry's to write, and read-only there, except a repository's
description:

```sh
cr repository ls -o table
cr repository patch <id> '{"desc": "the storefront"}'
```

## The management page

`ts/` holds a page over the management API: repositories and their
descriptions, tag rules, and collection runs, with a button that starts a full
collection. It is not part of the image, and it is not yet something to point
at a deployment: it signs in with payday's development scheme, which takes the
caller's word for who they are and which `cr serve` does not accept. See
[development.md](development.md#the-page).
