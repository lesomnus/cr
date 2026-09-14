# Access

Who may use the registry and what they may do: credentials and tokens,
bindings, tag rules, OpenID Connect, roster, and the management API.

## Turning the guard on

With no `auth:` block the registry is open: every request may pull, push and
delete, and the log says so at startup. Anything configured below turns the
guard on, and `auth.enabled: true` turns it on when every binding is a row in
the database.

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

## Credentials and tokens

`docker login` sends a username and a password, and the authenticators that
are configured are asked in turn; the first that knows the credential decides.

| | the password | the subject, and its groups |
| --- | --- | --- |
| `htpasswd` | checked with bcrypt against the file | the username; `htpasswd.groups` |
| `static` | one of the tokens, with any username | the token's `name`; its `groups` |
| `oidc` | an ID token from a configured provider | the `sub` claim, or `subject_claim`; `groups_claim` |
| `roster` | an `rt_` key, or the roster password of `tenant/holder` | the holder; its tenant and teams |
| the exchange | a token `POST /token/exchange` issued | whoever it was exchanged for |

A static token is written as `token`, or as `token_sha256`, its SHA-256 in hex,
which is what a file that is not itself secret should carry.

The registry answers `/v2/` with a challenge naming `/token`, even where
anonymous pulls are allowed, since that answer is how a client learns where
tokens come from and how `docker login` checks a password. Clients fetch a
token from `/token` with their credentials, and every later request is checked
offline against it; `/v2/` also takes Basic credentials directly. A token says
what it grants and what was asked for and refused, so a request for an action
the token was never asked for is answered `401` with `insufficient_scope` --
the client fetches a token that asks -- and one that was refused is `403
DENIED`. The public keys are at `/.well-known/jwks.json`.

Without `token.keys` a key is made at startup. Tokens then die with the
process and no second replica accepts them, so a real deployment names one:

```sh
openssl ecparam -name prime256v1 -genkey -noout -out token.pem
```

## Bindings

A binding grants `actions` on the repositories `repo` matches to a `subject`
or a `group` -- and, with `when`, only to a credential whose claims match.

- **Bindings add and never subtract.** There is no deny: a caller may do
  whatever any binding matching it grants.
- **`anonymous` is everyone**, with a credential or without, so logging in never
  takes away what a public repository allows. A repository is public by a
  binding to `anonymous`; there is no separate visibility setting. The group
  `authenticated` is every caller whose credential checked.
- **A glob's `*` matches any run of characters, slashes included**: `acme/*`
  covers `acme/team/app`.
- **The actions** are `pull`, `push`, `delete`, `tag` (create or move a tag; a
  push without it is by digest only), `catalog`, `search`, `admin` (move a
  protected tag, and the operator's endpoints), and `*` for all of them.
  Distribution clients know nothing of `tag`, so a token request that asks for
  `push` gets `tag` asked for too.
- **`catalog`, `search` and registry-wide `admin` come only from a binding over
  `*`.** `admin` over `acme/*` moves protected tags in `acme/*` and reaches
  nothing registry-wide.
- **A mount** needs `push` on the target and `pull` on the source, or it
  becomes an ordinary upload.

## Tag rules

Tag rules are checked on a manifest push and delete before anything is
written, whoever the caller is:

| kind | refuses |
| --- | --- |
| `immutable` | moving or deleting a tag once it is set; pushing the same digest again is fine |
| `protected` | moving or deleting, unless the caller has `admin` or is in `groups` |
| `pattern` | creating or moving a tag that does not wholly match `pattern` (RE2) |
| `retention` | nothing at push; garbage collection keeps the newest `keep` |

A delete by digest removes the tags pointing at the manifest, and is refused
when any of them may not be deleted. A `pattern` that does not compile refuses
every tag it covers. Retention deletes a tag only when every `retention` rule
matching it agrees, and never an `immutable` one. Tags that clients use to
store signatures are tags like any other; see
[registry-api.md](registry-api.md#artifacts-signatures-and-sboms).

## Rows beside the configuration

Bindings and tag rules can also be rows in the database, written through the
management API. On the host, `cr` reaches that API in-process as the holder
`management.as` names (`@operator/admin` by default, which `cr init` puts up):

```sh
cr binding add @operator/ci-push '{"group": "ci", "repo": "acme/*", "actions": ["pull", "push", "tag"]}'
cr tag-rule add @operator/semver '{"repo": "acme/*", "tag": "*", "kind": "pattern", "pattern": "v\\d+\\.\\d+\\.\\d+|latest"}'
cr binding ls -o table
cr binding erase @operator/ci-push
```

The registry reads rows again every `auth.refresh` (five seconds). A decision
never waits on the database: requests read a snapshot, and when a reload
fails, the snapshot in force stays in force.

The configuration and the rows are two sources of one policy, and a binding in
either is in force. The configuration's are read once, when `cr serve` starts:
they never become rows, `cr binding ls` does not show them, and removing one
from the file takes effect when every replica has restarted without it. A row
takes effect, or stops, within `auth.refresh`. A binding written in both places
is in force until it is gone from both, and a token `/token` already issued
keeps the access it was issued with until it expires, `auth.token.ttl` at the
latest.

## CI without secrets: OpenID Connect

```yaml
auth:
  oidc:
    - issuer: https://token.actions.githubusercontent.com
      audience: cr.example.com
      subject_claim: sub          # the default
      groups_claim: ""            # a claim holding groups, when the provider has one
      prefix: "github:"           # in front of every subject from this provider
  exchange:
    ttl: 1h
  bindings:
    - group: authenticated
      repo: acme/app
      actions: [pull, push, tag]
      when:
        repository: acme/app
        workflow_ref: acme/app/.github/workflows/release.yml@refs/heads/main
```

A job asks its provider for an ID token for cr's audience and gives it as the
password; cr checks it offline against the keys the provider publishes, and
every claim of it is the subject's for a binding's `when`, whose values are
globs. That is what lets one workflow, and no other, push a repository:

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

What comes back is a token cr signed that stands for the same subject with the
same claims for `exchange.ttl`. It is a password and never an access token,
and it cannot be exchanged again, or it would never expire. The exchange also
takes the credential as Basic, or as an RFC 8693 `subject_token`.

**What to pin.** A binding is only as narrow as its `when`:

- **The ref, not only the file.** `workflow_ref` is the workflow file at the
  ref it ran from. With `release.yml@*`, anybody who can push a branch can edit
  `release.yml` on that branch and push images with it; pin `@refs/heads/main`
  or `@refs/tags/v*`, and protect those refs.
- **The caller, or the reusable workflow.** When a workflow calls a reusable
  one, `workflow_ref` names the caller and `job_workflow_ref` names the
  reusable workflow. To trust a shared build workflow wherever it is called
  from, bind `job_workflow_ref`.
- **Identifiers as well as names.** A repository or an owner can be renamed and
  the old name taken by somebody else; `repository_id` and
  `repository_owner_id` cannot, so a `when` naming them stays with the
  repository it was written for.
- **One binding per mapping.** `repo` and `when` are matched separately, and
  nothing carries a claim's value into `repo`: `repo: acme/*` with
  `repository: acme/*` lets the workflows of every `acme` repository push to
  every `acme/*` repository here. A workflow that should reach only its own
  repository needs a binding that names both.

## roster

```yaml
auth:
  roster:
    url: https://roster.example.com   # roster's data plane over HTTP, its `server.http`
    key: rk_...
    remember: 1m
management:
  roster:
    url: https://roster.example.com
    key: rk_...
```

cr calls roster where roster's people and apps do, with a key made for cr as a
service:

```sh
roster key add --service cr --allow '/payday.TokenService/Introspect,/roster.VouchService/Verify,/roster.HolderService/Get,/roster.TenantService/Get,/roster.TeamMembershipService/List,/roster.TeamService/Get,/roster.SiteService/Get,/roster.SyncService/Watch'
```

A robot is a roster holder with an `rt_` key, which is its password:

```sh
roster holder add @acme/ci
RT=$(roster key add --tenant acme --holder ci --name docker --allow '/cr.Registry/*')
echo "$RT" | docker login cr.example.com -u ci --password-stdin
```

A key is used for what it was made for. The registry's actions go by method
names a key can list -- `/cr.Registry/Pull`, `Push`, `Tag`, `Delete`,
`Catalog`, `Search` and `Admin` -- and `/cr.Registry/*` is all of them. A key
made with `--allow /cr.Registry/Pull` pulls whatever the bindings let its
holder pull and pushes nothing, and a key that allows none of them is refused
at login. The bindings still decide; a key only narrows, and the narrowing
holds through the tokens and the exchange.

A person signs in with `acme/alice` and their roster password, and is the whole
of themselves. With a second factor, which a password prompt cannot carry, they
use an `rt_` key instead, and `roster sign-in` mints one from the terminal.

The subject is the holder's identifier, and `@acme/alice` is its alias. Its
groups are `@acme` and one for each team it is in: `@acme/eu/ops` for the team
`ops` in the site `eu`, since a team's name is unique only within its site, and
the team's identifier for a team in no site, which roster names by identifier
alone. What roster accepted is remembered for `remember`, and forgotten at once
when roster's sync stream says the holder changed, so a revoked key stops
working within `remember` at the latest.

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
curl -sX POST https://cr.example.com/app.BindingService/List \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' -d '{}'
```

With no tokens and no roster, nothing over the network may use it; `cr <entity>
...` on the host still can. With `management.roster`, a key roster issued is a
caller too, for the methods it allows -- `/app.BindingService/*` and the like --
and the tenant and holder it names are put up here the first time they are
seen, which is how bindings come to belong to roster's tenants.

Bindings and tag rules are written through it. Repositories, manifests, tags
and collection runs are the registry's to write, and read-only there, except a
repository's description:

```sh
cr repository ls -o table
cr repository patch <id> '{"desc": "the storefront"}'
```

## The management page

`ts/` holds a page over the management API: repositories and their
descriptions, bindings, tag rules, and collection runs, with a button that
starts a full collection. It is not part of the image, and it is not yet
something to point at a deployment: it signs in with payday's development
scheme, which takes the caller's word for who they are and which `cr serve`
does not accept. See [development.md](development.md#the-page).
