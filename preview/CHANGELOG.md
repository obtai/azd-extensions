# Release History

## 0.6.0

### Breaking Changes

None. Every 0.5.0 `preview.yaml` loads and behaves as it did: the top-level
`previewApp`, `dockerfile` and `env` are now the single-service shorthand for
a `services` entry named after the azd project, pushing to the project's
repository, labelled `pr-<n>`.

### Features Added

- **`services`** — more than one container app per pull request, keyed by azd
  service name and minted in declaration order. Each has its own `app`,
  `dockerfile`, `context`, `image` (the registry repository, defaulting to the
  service name), `container` (which container in the template to replace,
  defaulting to the first), `env`, `healthPath` and `scale`. `${url:<service>}`
  and `${fqdn:<service>}` substitute another service's preview URL: every
  service's label is known before anything is built, so the api can be told
  the ui's origin and the ui the api's host. `up` builds and mints every
  service, then gates on all of them; one that never becomes healthy fails the
  whole pull request and deactivates what was minted. `url` prints
  `<service>=<url>` per line for more than one service; `--service` picks one.
- **`labels`** — a fixed pool of revision labels. A pull request borrows the
  first free one and keeps it across pushes until `down`, so an app that signs
  people in through Entra registers N callback URLs once. `up` prints the
  label and writes `label=` to `$GITHUB_OUTPUT` beside `url=`. A full pool is
  an error, `all N preview labels are in use`. `--label <name>` on `up`
  overrides both the pool and the `pr-<n>` default. With a pool the label is
  a fact about the app rather than the number, so `url`, `down` and `status`
  look it up by the revision name it points at — `url` no longer answers for
  a pull request that has no preview.
- **`prefix` and `outputs`** — the deployment values by prefix or by name.
  Each of `subscriptionId`, `resourceGroup`, `registry`, `domain`,
  `postgresServer` and `keyVault` accepts `${output:NAME}` and `${env:NAME}`;
  so does a service's `app`. Anything unset is read by its conventional name
  from the azd environment, then from the **process environment**, so
  `${output:X}` also falls through to an exported `X`.
- **`previewEnvironment` is optional.** Without it no azd environment is read
  and every value comes from `outputs` or the shell — which is how a
  repository whose infrastructure was not provisioned by azd runs previews.
- **`database.create`** creates the pull request's database over ARM before
  `provision` runs, idempotently (UTF8, `en_US.utf8`). A server behind a
  private endpoint cannot be reached from a runner with psql, and an app that
  migrates itself at boot has no provision step at all. `database.server`
  overrides the server; `database.name` overrides the name, with `${pr}` and
  `${label}`.
- **`healthPath`** per service, default `/api/health`, for the advisory probe
  against the preview URL.
- **`${output:NAME}` and `${env:NAME}` in `env`.** `${secret:name}` there is
  refused at load, with a message saying a revision template is plaintext.
- **`status <pr>`** — per service: the label, revision, URL and health, plus
  whether the database exists. Exit 1 when nothing exists.
- **`promote <pr>`** — moves the pull request's labelled revision to 100% of
  traffic on a shared app, the one azd deploys. Refused on a dedicated preview
  app with a message saying to deploy through azd instead. The label stays
  attached, as Plot's own promotion does. `--service` picks one.
- **`--output json`** on `up`, `url`, `down`, `status` and `promote`: one JSON
  object on stdout, with the progress moved to stderr so stdout parses.

### Bugs Fixed

- On a shared app with no revision carrying the live label, the base template
  came from the app's **latest revision** — which on an app that hosts previews
  is a preview, or the restore revision behind it, so the next pull request
  inherited the last one's database. It now comes from the revision holding
  100% of traffic: by name, or through `latestRevision: true` the way a fresh
  `azd deploy` leaves the split. A split is an error. The live label is still
  preferred when a revision carries it.
- A preview revision carried only the first container of the app's template.
  Init containers, volumes, service binds, the grace period and every other
  container now come along; only the named container's image and env change.

### Other Changes

- After a push is healthy and labelled, the pull request's **earlier** active
  revisions are deactivated and their image tags removed, keeping only the
  new ones. An app holds 100 revisions, active and inactive, and purges the
  oldest past that; a busy pull request must not be what evicts rollback
  history.
- Env overrides appended to a template are written in sorted order, so two
  runs with the same overrides produce the same template.
- The README now describes the extension rather than the scaffold it was
  generated from.

## 0.5.0

### Features Added

- `previewApp` in `preview.yaml` names the container app preview revisions are
  added to. Unset, it is the app the azd service deploys, which is 0.4.0's
  behaviour — so existing consumers are unaffected.

  Prefer a **dedicated** app, declared by the repository's own infrastructure.
  A revision is minted from its app's template, so a preview necessarily writes
  its own `APP_ENV` and database name onto whichever app hosts it. On an app of
  its own that is harmless: nothing else deploys there, and everything a preview
  changes, the next preview changes again. On the app serving production it is
  not.

- Two shapes now fall out of one mechanism — previews are labelled revisions of
  a named non-production app:
  - **prod + preview**, for an app that does not warrant its own environment: a
    bare preview app beside production, sharing the database server, registry,
    vault and identity, scaled to zero.
  - **prod and dev + preview**, for one that does: `previewEnvironment` already
    points previews at another azd environment, and dev is the preview host.

### Bugs Fixed

- The restore no longer reuses the live revision's suffix. Container Apps
  rejects a PUT naming a suffix that already exists — "revision with suffix X
  already exists" — rather than treating it as the no-op it looks like, so the
  restore failed and left the preview's database name on the app serving
  production. The suffix is cleared and Container Apps generates one.

### Other Changes

- The restore, and the check that it worked, are **skipped entirely when
  `previewApp` names an app of its own**. That also removes the revision it
  minted on every push, which on a shared app competes with `maxInactiveRevisions`
  for the slots holding rollback history.
- `Target.SourceApp` is now `ServiceApp` (what azd deploys) alongside
  `PreviewApp` (what previews are added to).

## 0.4.0

### Breaking Changes

- A preview is now a zero-traffic **revision** of the app it previews, labelled
  `pr-<n>`, rather than a container app of its own. The URL moves with it, from
  `https://ca-<project>-pr-<n>.<domain>` to
  `https://<app>---pr-<n>.<domain>` — three dashes, because it addresses a label.
- **The app previews are added to must be in multiple-revision mode.** The
  extension refuses to touch a single-revision app rather than mint a revision
  that would take production's traffic.
- `azd preview down` deactivates revisions instead of deleting an app. That
  stops the replicas and the billing but does not free a revision slot: an app
  holds 100, and `maxInactiveRevisions` is what stops pull request churn
  evicting rollback history.

### Features Added

- `liveLabel` in `preview.yaml` (default `live`) names the label carrying
  production traffic. It is the template a preview is derived from, and the one
  the app is restored to afterwards.
- `${label}` is substituted in `env` and `provisionEnv`.
- A preview revision is now gated on Container Apps reporting it healthy, and
  deactivated if it never does. The HTTP probe against the preview URL remains,
  and remains advisory — it cannot tell a slow revision from a dead one.
- First tests in this repo, covering name derivation, template inheritance and
  the traffic-label arithmetic.

### Bugs Fixed

- Environment overrides are applied on **every** push. They were previously
  applied only when a preview was first created, so a change to `env` in
  `preview.yaml` never reached an existing preview.
- The post-write assertion reads the revision it just created rather than the
  app's latest template, which under multiple-revision mode is not the same
  thing and made the check meaningless.

### Other Changes

- A revision is minted from the **app's** template, so writing a preview
  necessarily writes the preview's environment onto the shared app. The app is
  restored to the live revision's template in the same call that assigns the
  label, and the restore is verified rather than assumed — otherwise a
  production release that patches only the image inherits a pull request's
  database. Consuming repositories are expected to carry an independent check
  before moving the live label; this one is not load-bearing alone.

## 0.3.0

### Features Added

- Builds reuse a layer cache kept in the registry, at `<repository>:buildcache`.
  A CI runner is thrown away after every job, so its local cache is always
  empty; without this every preview rebuilt from nothing. `mode=max`, because
  the expensive layer lives in a stage the final image only copies from.
- Pushes straight from the builder rather than exporting to the local image
  store and pushing separately. The export was the slowest step of a build and
  a container-driver builder keeps nothing locally anyway.

### Other Changes

- Builds now run on a `docker-container` buildx builder, created once and
  reused. The default driver cannot export a cache at all.

## 0.2.0

### Breaking Changes

- Builds now use the local Docker daemon instead of ACR Tasks, so the machine
  running `azd preview up` needs Docker. A build in Azure kept Docker off the
  machine but hid the log, so a broken Dockerfile arrived as a status word.

### Features Added

- The build log streams to stdout.

### Other Changes

- `<PREFIX>_ACR_NAME` is no longer a required deployment output. Building,
  pushing, listing and deleting tags all go through the registry login server.

## 0.1.0

### Features Added

- `azd preview up|down|url <pr>`.
- A JSON schema for `preview.yaml`, and strict decoding so an unknown key is an
  error rather than a silence.
