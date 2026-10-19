# Access

Who may use the registry and what they may do: the policy file, providers,
permissions and matches, the globs they are written in, testing a policy,
tokens, tag rules, and the management API.

## The policy file

What the registry grants, and to whom, is a file of its own, `cr.auth.yaml`
beside `cr.yaml`:

```yaml
# cr.auth.yaml
providers:
  github:
    kind: oidc
    issuer: https://token.actions.githubusercontent.com
    audience: cr.example.com
    exchange: 1h

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

`cr.yaml` says where the file is and what the server is -- the keys it signs
tokens with -- and nothing about who may do what:

```yaml
# cr.yaml
auth:
  policy: cr.auth.yaml          # the default; relative to this file
  enabled: true                 # refuse to start without the policy file
  refresh: 5s                   # how often the file is read again
  token:
    keys: [/etc/cr/token.pem]   # P-256; the first signs, all verify
    ttl: 5m
```

**The file is the policy.** It is read again every `auth.refresh`, and when
its content changed and has read the same twice running, what it says is in
force; nothing restarts. The second read is so that a file caught while it is
being written is not taken for the policy: one cut short where a rule was says
something else, and may say it validly. A file that does not parse, does not
check, or is gone keeps the policy in force, and the log says why at every read
from then on. Once the guard is on, nothing while the registry runs turns it
off. The revision in force, the start of the file's SHA-256, is logged when it
changes and reported as `cr.auth.policy.revision`, which is how replicas are
seen to agree.

The file is read the way `cr.yaml` is: a key nothing reads is refused, keys
starting `x-` are left alone for anchors, and `${env:NAME}` is a variable of the
process wherever it is written.

| | |
| --- | --- |
| no policy file where it is by default | the registry is open -- every request may pull, push and delete -- and the log says so |
| `auth.policy` names a file that is not there | `cr serve` does not start |
| `auth.enabled: true`, and no policy file | `cr serve` does not start |
| no `cr.yaml`, configured from `CR_*` alone | there is no default: `CR_AUTH_POLICY` names the file |

A token cr issued keeps what it was issued with until it expires, so what a
change takes away is gone within `auth.token.ttl` of it being read, and from
an exchanged token only when that expires.

The file is checked whole, and a policy that does not check is not used: a
field nothing reads, a provider of a kind there is not, two providers with one
issuer, a match `for` a provider there is not or that grants a permission there
is not, a match for a provider with no `when` of its own or the provider's, or
for `anyone` with one, a match that says a claim otherwise than its provider,
an action there is not, a glob that does not parse, or a permission whose
every pattern takes away. `cr auth check` says the same before the file is
deployed.

A key under `auth:` in `cr.yaml` that nothing reads stops `cr serve` too,
rather than being ignored -- as a key anywhere in `cr.yaml` does, named with its
line.

## Providers

```yaml
providers:
  github:
    kind: oidc
    issuer: https://token.actions.githubusercontent.com
    audience: cr.example.com
    subject_claim: sub          # the default
    exchange: 1h                # none by default
```

The name is what a match's `for` names it by, and what goes in front of every
subject it vouches for in the logs: `github:repo:acme/app:ref:refs/heads/main`.
It is lowercase letters, digits, `-` and `_`, and it is not `anyone`.

`oidc` is the kind for CI and for people signing in. A caller gives an ID token
the provider issued as the password, and cr checks it offline, against the keys
the provider publishes: its signature, its `iss`, that its `aud` is `audience`,
and that it has not expired. `audience` is required: without it, a token the
provider issued for any other service would be good here. Every claim of the
token is the caller's, for a match's `when`.

`exchange` is how long a token `POST /token/exchange` trades one of the
provider's credentials for lasts; see [below](#ci-without-secrets-openid-connect).
A provider that does not say trades none: a CI provider whose jobs outlive
their ID tokens wants one, and a provider people sign in with may well not.

```yaml
providers:
  github:
    kind: oidc
    issuer: https://token.actions.githubusercontent.com
    audience: cr.example.com
    when:
      repository_owner: acme
```

`when` on a provider is claims every match for it requires, beside the
match's own, each value a glob as in a match. It is what is true of every
caller the policy lets in by this provider -- the organization a CI provider's
repositories belong to, say -- written once, and not left out of the next match
someone adds. A match for a provider with a `when` needs none of its own. A
match may say a claim the provider says only as the provider says it: both
would have to hold, so a different value is a match nobody is ever under, or
one that reads as if it relaxed the provider's claim, which it would not; it
is refused. Every provider may have one, `mtls` included.

### Client certificates: `mtls`

```yaml
providers:
  engines:
    kind: mtls
```

`mtls` vouches for the client certificate a caller's connection verified, on a
listener with `tls.client_ca_file` (see
[operating.md](operating.md#listeners)). Which certificates are good is the
listener's to say, before there is a request; this names whoever presented
one. So it has nothing to configure, and a policy has one of it or none.

It is the credential for a client that cannot be told to send one. A container
runtime sends a password only to the registry an image is named after, never to
a mirror in front of it; a client certificate is part of the connection to the
mirror, configured for that host -- `client` in containerd's `hosts.toml` --
so an engine that only ever reaches cr as a mirror can still be told apart.

The caller is `<provider>:<common name>`, and its claims are:

| claim | |
| --- | --- |
| `cn` | the subject's common name |
| `o`, `ou` | the subject's organizations and units, lists |
| `dns`, `ip`, `uri`, `email` | the subject alternative names, lists |
| `fingerprint` | the SHA-256 of the certificate, lowercase hex: `openssl x509 -outform der -in c.crt \| sha256sum` |

```yaml
matches:
  engines:
    for: engines
    grant: [releases]
    when:
      cn: "engine-*"
```

`when` narrows what the listener's CA already allows. With a CA that signs only
for this, `cn` is enough; with a bundle of self-signed certificates the bundle
is the pin. A request with an `Authorization` header is who the header says,
certificate or not, and a certificate on a listener without
`client_ca_file` is never asked for. A runtime that is challenged asks
`/token` without a password; the token it gets is for its certificate.

### A password: `secret`

```yaml
# cr.auth.yaml
providers:
  breakglass:
    kind: secret
    username: admin
    password: ${file:/etc/cr/breakglass/password}

matches:
  breakglass:
    for: breakglass
    grant:
      - repos: ["**"]
        actions: ["*"]
```

`secret` vouches for one caller, by a username and a password: the way in for
an administrator when nothing else can be asked -- the CI provider is down, or
a mistake in the policy locked everybody else out. It is a credential that
lives as long as it is not changed, so it is for that and little else.

The password stays out of the policy. `${file:/path}` is the file's content,
without the whitespace around it, and the file is read again when it is
replaced; `${env:NAME}` is a variable of the process. A password written as it
is works too, but the policy is a file many people read, and one written into it
is one they all have. A reference is a reference only as the whole value.

The caller is `<provider>:<username>`, `breakglass:admin`, and its one claim is
`username`. Being one caller, a match for it needs no `when`. It has no
`exchange`: a password can be given again. Every login by it is logged as a
warning, and every wrong password; `cr.auth.logins` counts the logins with
`cr.auth.authenticator` `secret`.

A password that cannot be read -- the file is not there, or is empty -- lets
nobody in by this provider, and is logged when somebody tries; the rest of the
policy stands, so a Secret that did not arrive does not lock out everybody
else. Removing the provider, or replacing the file, takes the password away
within `auth.refresh`; a token issued before lives its `auth.token.ttl` out.

Another username is not this provider's, and the next provider is asked: a CI
job that logs in as `-u oidc` goes on to `github`. So a `secret` provider's
username should be none a client uses for another, such as `oidc`.

On Kubernetes, the password is a Secret, mounted beside the policy:

```sh
kubectl -n registry create secret generic cr-breakglass \
  --from-literal=password="$(openssl rand -base64 32)"
```

```yaml
# the cr Deployment's pod
volumes:
  - name: breakglass
    secret:
      secretName: cr-breakglass
containers:
  - name: cr
    volumeMounts:
      - name: breakglass
        mountPath: /etc/cr/breakglass
        readOnly: true
```

Kubernetes replaces a mounted Secret by a rename, which is what cr watches for,
so `kubectl apply` of a new password is in force without a restart. A
`subPath` mount is never updated; mount the directory.

Using it is `docker login`:

```sh
kubectl -n registry get secret cr-breakglass -o jsonpath='{.data.password}' | base64 -d \
  | docker login cr.example.com -u admin --password-stdin
```

and a test case names it as any provider:

```yaml
cases:
  - name: the administrator does anything
    as:
      provider: breakglass
      claims: {username: admin}
    repo: acme/app
    allow: [pull, push, tag, delete]
```

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
- **`grant`** is the permissions granted: by name, or written out in place
  (below).
- **`when`** is claims of the credential that must all hold, each value a
  glob. A claim that is a list holds when any of its values does, and a number
  or a boolean is matched as it is written. A match for a provider must have
  one, or its provider one (above) -- without either, every credential the
  provider issues to anybody is under it -- and one for `anyone` cannot, since
  a caller with no credential has no claims.

A permission that one match grants and no other is written in the match:

```yaml
matches:
  cr-release:
    for: github
    grant:
      - repos: ["lesomnus/cr", "lesomnus/cr/**"]
        actions: [pull, push, tag]
    when:
      repository_id: "123456789"
      workflow_ref: "lesomnus/cr/.github/workflows/release.yml@refs/heads/main"
```

Which repositories and which callers are then read together, where a
permission named in another part of the file would be read apart from the
claims it is granted under. It has the fields a permission has, checked the
same way, and is named by where it is -- `cr-release.grant[0]` -- in what
`cr auth explain` and `cr auth test` say and in errors. It is the match's own:
another match grants it by writing it again, or a permission both name. A
`grant` may mix both forms.

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
same provider with the same claims, for the provider's `exchange`. It is a
password and never an access token, and it cannot be exchanged again, or it
would never expire. The exchange also takes the credential as Basic, or as an
RFC 8693 `subject_token`. It answers `403` for a credential whose provider
trades none, and `404` when no provider does.

**What to pin.** cr checks that every claim in `when` holds, and nothing more:
which claims to ask for is the policy's to say. A match is only as
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
  own repository needs a match of its own, with its permission written in it.

## Testing a policy

A policy is tested offline, the way `cr serve` would read it: no database, and
no provider is asked for anything.

```sh
cr auth check cr.auth.yaml
cr auth test cr.auth.yaml cr.auth.test.yaml
cr auth test cr.auth.yaml auth-tests/registry.yaml auth-tests/releases.yaml
cr auth test cr.auth.yaml auth-tests/*.yaml
cr auth explain --as github --claim repository_id=123456789 --repo lesomnus/cr cr.auth.yaml
```

A test file is cases, each a caller, a place, and what it must and must not be
allowed there:

```yaml
# cr.auth.test.yaml
cases:
  - name: the release workflow pushes cr
    as:
      provider: github
      claims:
        repository_id: "123456789"
        workflow_ref: lesomnus/cr/.github/workflows/release.yml@refs/heads/main
    repo: lesomnus/cr
    allow: [pull, push, tag]
    deny: [delete]

  - name: another branch's release.yml does not push
    as:
      provider: github
      claims:
        repository_id: "123456789"
        workflow_ref: lesomnus/cr/.github/workflows/release.yml@refs/heads/evil
    repo: lesomnus/cr
    deny: [push]

  - name: busybox is not public
    as: anyone
    repo: library/busybox
    deny: [pull]

  - name: nobody lists the registry
    as: anyone
    registry: true
    deny: [catalog, search]
```

- **`as`** is `anyone`, a caller with no credential, or a provider the policy
  has and the claims of a credential it issued.
- **`repo`** is a repository, or **`registry: true`** the registry as a whole,
  where `catalog`, `search` and registry-wide `admin` are asked.
- **`allow`** must all be granted, and **`deny`** must all be refused. An
  action in neither is not looked at.

Repeated callers can be defined once in `subjects`. A case's `as` accepts a
subject name, `anyone`, or the inline provider/claims mapping above. Both `as`
and `repo` also accept non-empty lists; every caller is checked against every
repository. Two declarations below run the same 16 cases as four callers
written out separately for each of four repositories:

```yaml
subjects:
  release:
    provider: github
    claims:
      repository_id: "123456789"
      workflow_ref: lesomnus/cr/.github/workflows/release.yml@refs/heads/main
  other-branch:
    extends: release
    claims:
      workflow_ref: lesomnus/cr/.github/workflows/release.yml@refs/heads/feature
  other-workflow:
    extends: release
    claims:
      workflow_ref: lesomnus/cr/.github/workflows/other.yml@refs/heads/main

cases:
  - name: the release workflow can publish
    as: release
    repo: &repos [lesomnus/cr, lesomnus/cr/cache, lesomnus/cr/stage, lesomnus/cr/prod]
    allow: [pull, push, tag]
    deny: [delete, admin]

  - name: other callers cannot publish
    as: [other-branch, other-workflow, anyone]
    repo: *repos
    deny: [push, tag, delete, admin]
```

`extends` names one parent in `subjects`, which may be declared later and may
itself extend another subject. The provider is inherited unless overridden.
Claims are inherited by key: a child replaces only the values it supplies.
Lists and nested objects are replaced as whole values, with no recursive merge;
an explicit `null` is a null claim value, not a request to remove the claim.
Parent and sibling definitions are unchanged. `anyone` is reserved for the
anonymous caller and cannot be defined or inherited as a subject name.

Lists may mix subject names, `anyone` and inline caller mappings. A list of
callers also works with `registry: true`, which cannot be combined with `repo`.
Repository entries are concrete names, not patterns to expand. Empty lists,
unknown subjects/providers, inheritance cycles and unknown fields are errors,
including errors in unused subject definitions. All supplied files are validated
before any cases run. Expectations remain explicit in `allow` and `deny`;
they are never derived from the policy being tested.

Cases expand in file order, then caller-list order, then repository-list order.
Named subjects and list combinations include the subject and target in their
output, for example:

```text
ok    the release workflow can publish [subject=release, repo=lesomnus/cr]
FAIL  other callers cannot publish [subject=other-branch, repo=lesomnus/cr/prod]
```

Inline callers in lists are identified by their zero-based position and
provider (`subject=as[0]:github`). Existing scalar `anyone` and inline cases
keep their original names. The final passed/failed count includes every
expanded combination.

Split related tests into separate files and pass one or more paths after the
policy path. Each file is independent: its `subjects`, `extends` references and
YAML anchors refer only to definitions in that file. The same subject name may
be used differently in another file. Any file can also be run on its own.
The shell expands `auth-tests/*.yaml`; cr reads the paths in argument order.
Every file must contain cases, so an empty file or an unmatched glob is an
error. A validation/read error in any file prevents all cases from running.

With multiple files, each result includes its path, for example
`ok    auth-tests/releases.yaml: publish [subject=release, repo=lesomnus/cr]`.
Assertion failures do not stop the remaining files: the final passed/failed
count covers all expanded cases, and any failure makes the command exit
non-zero. Supplying a single file keeps the output shown above.

`cr auth test` prints a line for each case and exits non-zero when one fails,
with what it got wrong and why:

```
ok    the release workflow pushes cr
FAIL  busybox is public
      pull: denied, and it should be allowed
      match cr-release (for github): does not hold: for github, and the caller gave no credential
        cr-release: not over it: pull, push, tag
      match public (for anyone): holds
        library-read: not over it, by "!library/busybox": pull
      granted: nothing
```

`cr auth explain` prints the same for one caller: every match, whether it held
or what kept it from holding, and for each permission it grants, whether it is
over the repository and the pattern that decided. Without `--repo` it is the
registry as a whole; its flags come before the file.

A policy kept in a repository of its own is tested there, and deployed when
its tests pass.

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
# cr.yaml
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

Tag rules are the policy file's `tag_rules`.

Tag rules can also be rows in the database, written through the management
API. On the host, `cr` reaches that API in-process as the holder
`management.as` names (`@operator/admin` by default, which `cr init` puts up):

```sh
cr tag-rule add @operator/semver '{"repo": "acme/**", "tag": "*", "kind": "pattern", "pattern": "v\\d+\\.\\d+\\.\\d+|latest"}'
cr tag-rule ls -o table
cr tag-rule erase @operator/semver
```

The registry reads rows again every `auth.refresh`, with the policy file. A
decision never waits on the database: requests read a snapshot, and when a
reload fails, the snapshot in force stays in force. The file's rules and the
rows are both in force, and the file's never become rows.

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

Configuration tokens can read without creating a holder or tenant row. A
missing tenant has no tenant-owned read scope; manifests and other global
entities are still readable when their methods are allowed. Writes require a
registered holder and otherwise return `permission_denied` with the holder's
name. Configuration is never copied into database rows.

Give one token an `allow` list, or share a role through a group:

```yaml
management:
  tokens:
    - holder: '@operator/kamino'
      token_sha256: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
  policy:
    roles:
      '@operator/artifact-reader':
        methods:
          - /app.ManifestService/List
          - /app.ManifestService/Get
          - /payday.ResourceService/Watch
    groups:
      '@operator/readers':
        members: ['@operator/kamino']
    bindings:
      - role: '@operator/artifact-reader'
        group: '@operator/readers'
```

Bindings name either `holder` or `group`. Groups are explicit, unnested lists;
all names in a binding belong to the same tenant. These are payday's shared
RBAC rules, also used by roster's file provider. They are evaluated read-only,
independently of any database groups. A token's optional `allow` further narrows
its roles; it cannot add permissions. `allow: []` grants nothing. With `policy`
present, an unbound holder gets nothing. For compatibility, omitting both
`policy` and `allow` retains the whole management grant.

Method patterns use `/package.Service/Method`, with `*` as a whole component:
`/app.ManifestService/*`, `/app.*/Get`, or `/*.*/*`. They do not use the
registry's repository globs. A method outside the grant returns
`permission_denied`; an unknown token returns `unauthenticated`. Batch checks
each operation, and Watch checks the entity's Get permission for each image.

For live updates, move `tokens` and `policy` together into a separate file:

```yaml
management:
  file: management.yaml
  refresh: 5s
```

The path is relative to cr's config file. The document has `tokens` and `policy`
at its root, in the same shape as above. Inline tokens and policy are loaded
only at startup and cannot accompany `file`. Invalid initial files prevent
startup. Reload waits for two identical reads, then replaces credentials and
policy atomically; invalid or missing files log an error and retain the last
valid revision. A valid empty policy revokes its grants. An accepted change
ends existing configured-token streams with `unauthenticated`, prompting a
reconnect. Removing a token also prevents subsequent authentication.

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

### Finding new artifacts

`ManifestService.List` accepts an exact `artifact_type` filter across all
repositories. Fields in one filter are combined with AND; multiple filters
are alternatives. For example, this request to `/app.ManifestService/List`
finds cosign bundles in creation order, a page at a time:

```json
{"filters":[{"artifactType":"application/vnd.dev.sigstore.bundle.v0.3+json"}],"size":100}
```

Pass a response's `next` as `after` to read the next page. `next` is empty on
the final page; it is a page cursor, not a persistent polling checkpoint.

Subscribe to `/payday.ResourceService/Watch` with a CEL expression for new
manifests of that type:

```json
{"filter":"has(event.value) && type(event.value) == app.Manifest && event.kind == payday.CHANGE_KIND_CREATED && event.value.artifact_type == \"application/vnd.dev.sigstore.bundle.v0.3+json\""}
```

CEL can combine fields with `&&` and alternatives with `||`. Add
`event.value.repo == "stage"` or a digest/subject condition to narrow the
subscription. An empty filter accepts all events the caller can read, across
resource types. Guard dynamic payload fields with presence and type checks as
above. The existing `ManifestService.Watch` keeps its ref-based current-state
behavior.

The subscription is installed before response headers are sent and has no
initial snapshot. Each `WatchEvent` carries the manifest in `value` (a protobuf
`Any` whose type is `app.Manifest`) and `kind: "CHANGE_KIND_CREATED"`. Its `id`
identifies the event; the manifest's ID is inside `value`. `dateCreated` is the
time the event was recorded inside the write transaction, not its commit time
or the manifest's creation time. Registry and CLI index insertions name
`/cr.index.Manifest/Put` in `method` and `callMethod`; this identifies a direct
index operation, not a callable RPC.

Events are published only after the inserting transaction commits. Re-pushing
the same digest into the same repository sends nothing; copying it into a new
repository creates a new row and sends a notification. The index publishes
manifest creations only; pull bookkeeping and registry removals do not emit
resource events. Management writes to other resources use payday's generated
recorder. Updates include their new value; previous update values are disabled.

Delivery uses `watch.broker`: `memory` within one process, or `postgres`
across replicas. With `watch.outbox: true`, the exact event ID, timestamp and
payload are saved in the inserting transaction and published by the drainer.
Later edits or deletion cannot change that creation payload. Failed publication
is retried with the same event ID. Delivery may repeat and does not provide a
global event order or replay for disconnected subscribers.

To reconcile on startup or after a disconnect, open the stream, await its
response headers, then List the type while retaining subsequent events. Merge
List results and creation events by manifest ID; deduplicate event retries by
event ID. A resource stream is not a durable subscription or polling checkpoint.

Credentials need `/payday.ResourceService/Watch` and `/app.ManifestService/Get`
for manifest notifications, plus `/app.ManifestService/List` for reconciliation.
The current Get policy and read scope are applied to each recorded image before
CEL evaluation. Manifests are global management entities: allowing their reads
permits access across repositories; registry per-repository permissions do not
narrow management reads.

Existing databases need the new manifest artifact-type index, nullable
`Outbox.event` column, and schema changes included in the payday upgrade
(the `Archived` table and audit index). Run the normal schema migration before
deploying, and
upgrade all replicas and outbox drainers before relying on resource events.
Legacy outbox entries continue to serve key-based watches but carry no resource
payload for this stream.

`/payday.ResourceService/Batch` provides the existing transactional batch API.
`/payday.BatchService/Do` remains available for existing clients and is deprecated.

## The management page

`ts/` holds a page over the management API: repositories and their
descriptions, tag rules, and collection runs, with a button that starts a full
collection. It is not part of the image, and it is not yet something to point
at a deployment: it signs in with payday's development scheme, which takes the
caller's word for who they are and which `cr serve` does not accept. See
[development.md](development.md#the-page).
