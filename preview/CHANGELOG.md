# Release History

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
