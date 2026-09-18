# OBT Previews

An [azd extension](https://github.com/Azure/azure-dev/blob/main/cli/azd/docs/extensions/extension-framework.md)
that gives every pull request a preview on Azure Container Apps.

A preview is a zero-traffic **revision** of a container app the repository
already has, carrying a **label** and served at the label's own address,
`https://<app>---<label>.<environment domain>`. It inherits everything the app
has — identity, registry, secret references, probes — and gets its own image,
its own database and a handful of environment overrides. `down` removes all of
it. The shape is the same in every repository, which is why it is an
extension; what a release has to do is not, which is why the release lifecycle
stays in the consuming repository's azd hooks.

```sh
azd ext install obtai.preview

azd preview up 42        # build, database, revision, label, health gate
azd preview url 42       # the preview's address
azd preview status 42    # what exists
azd preview promote 42   # the preview revision takes 100% of traffic
azd preview down 42      # label, revisions, database, image tags — gone
```

The machine running `up` needs Docker: the image is built locally with buildx
and pushed straight to the registry, with a layer cache kept beside it.

## Configuration

A `preview.yaml` beside `azure.yaml`. Every key is optional; the schema is
[`preview.schema.json`](preview.schema.json), and a modeline points an editor
at it:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/obtai/azd-extensions/main/preview/preview.schema.json
```

### One service

```yaml
previewEnvironment: prod                       # the azd environment previews live alongside
previewApp: ${output:SALES_PREVIEW_APP_NAME}   # a dedicated app; unset = the app azd deploys
provision: sh scripts/hooks/predeploy.sh       # migrations, run on the deploying machine
env:                                           # overrides on the inherited template
  APP_ENV: preview
  APP_URL: ${url}
  PG_DATABASE: ${database}
dockerfile: Dockerfile
```

That is the whole of a 0.5.0 file, and it still means what it did: one
service named after the azd project, pushing to the `<project>` repository in
the registry, labelled `pr-<n>`.

### Several services

```yaml
previewEnvironment: dev
prefix: APP
labels: [comet, ember, falcon, glacier, harbour]
database:
  create: true
  server: ${output:APP_DATABASE_SERVER_NAME}
services:
  api:
    app: ${output:APP_API_CONTAINER_APP_NAME}
    dockerfile: apps/api/Dockerfile
    healthPath: /api/health
    env:
      DB_NAME: ${database}
      APP_ORIGIN: ${url:ui}
  ui:
    app: ${output:APP_UI_CONTAINER_APP_NAME}
    dockerfile: apps/ui/Dockerfile
    healthPath: /healthz
    env:
      API_UPSTREAM: https://${fqdn:api}
```

Services are built and minted in declaration order, then all of them are
gated on health; one that never comes up fails the pull request and
deactivates what was minted. Every service's address is known before anything
is built, so any service's `env` can name another's.

### Every key

| Key | Meaning | Default |
| --- | --- | --- |
| `previewEnvironment` | The azd environment previews live alongside. Without it no azd environment is read at all, and every value below comes from `outputs` or the shell. | — |
| `prefix` | Deployment-output prefix: `APP` reads `APP_CONTAINER_APPS_ENV_DOMAIN` and so on. | The azd project name, uppercased, `-` and `.` as `_` |
| `outputs.subscriptionId` | | `AZURE_SUBSCRIPTION_ID` |
| `outputs.resourceGroup` | | `AZURE_RESOURCE_GROUP` |
| `outputs.registry` | The registry login server. | `AZURE_CONTAINER_REGISTRY_ENDPOINT` |
| `outputs.domain` | The Container Apps environment's default domain. | `<PREFIX>_CONTAINER_APPS_ENV_DOMAIN` |
| `outputs.postgresServer` | The flexible server. Optional: without it there is no database to create or drop. | `<PREFIX>_POSTGRES_SERVER_NAME` |
| `outputs.keyVault` | The vault `${secret:name}` reads. Optional. | `<PREFIX>_KEY_VAULT_NAME` |
| `provision` | A command run before the revision is minted, with the deployment outputs, `provisionEnv` and every service's `env` in its environment. | — |
| `provisionEnv` | Extra environment for `provision`. The only place `${secret:name}` is allowed. | — |
| `env` | Overrides on the inherited template. Shorthand for `services.<project>.env`. | — |
| `previewApp` | A dedicated app for previews. Shorthand for `services.<project>.app`; unset, previews go on the app azd deploys and its template is restored afterwards. | `<PREFIX>_CONTAINER_APP_NAME` |
| `liveLabel` | On a shared app, the label carrying production. | `live` |
| `dockerfile` | Shorthand for `services.<project>.dockerfile`. | `Dockerfile` |
| `labels` | A pool of revision labels, in preference order. | — (`pr-<n>`) |
| `database.create` | Create the database over ARM before `provision`. | `false` |
| `database.server` | Overrides the server name. | `outputs.postgresServer` |
| `database.name` | Overrides the database name; `${pr}` and `${label}` substitute. | `<project>_pr_<n>` |
| `services.<name>.app` | The container app this service previews on — the one azd deploys. | `<PREFIX>_<NAME>_CONTAINER_APP_NAME` |
| `services.<name>.dockerfile` | | `Dockerfile` |
| `services.<name>.context` | Build context. | `.` |
| `services.<name>.image` | Registry repository. | `<name>` |
| `services.<name>.container` | Which container in the template gets the image and the overrides. | the first |
| `services.<name>.env` | Overrides on this service's template. | — |
| `services.<name>.healthPath` | Probed on the preview URL, advisory. | `/api/health` |
| `services.<name>.scale.min` / `.max` | Replicas. | `0` / `1` |

The top-level `previewApp`, `dockerfile` and `env` may not be combined with
`services`.

**Where values come from.** Every deployment value is resolved in one order:
what `preview.yaml` says explicitly, then the azd environment by the
conventional name, then the **process environment** by the same name.
`${output:NAME}` follows the same path, so a repository with no azd
environment exports the names it would otherwise have read.

**Substitutions.** In `env`, `provisionEnv` and `database.name`:

| | |
| --- | --- |
| `${pr}` | the pull request number |
| `${label}` | the revision label |
| `${database}` | the database name |
| `${url}` | this service's preview URL |
| `${url:<service>}` | another service's preview URL |
| `${fqdn:<service>}` | the same without the scheme |
| `${output:NAME}` | a deployment output, or an exported `NAME`; `${output:NAME\|fallback}` |
| `${env:NAME}` | the caller's environment; `${env:NAME\|fallback}` |
| `${secret:name}` | a Key Vault secret — `provisionEnv` only; a revision template is plaintext |

## Which app

A revision is minted from its app's **template**, so a preview necessarily
writes its `APP_ENV`, its database name and its URL onto whichever app hosts
it.

- **A dedicated app** (`previewApp` naming something azd does not deploy to):
  nothing else writes there, the pollution is harmless, and nothing is
  restored. Prefer this when the repository can declare one.
- **The app azd deploys** (`previewApp` unset, or any entry under `services`):
  the template is derived from the revision carrying `liveLabel` — or, when no
  revision does, the one holding 100% of traffic — and put back in the same
  call that assigns the label. The restore mints an idle revision of its own
  every push, and it is verified rather than assumed. A consuming repository
  should still check the app's template before it releases; this guard is not
  load-bearing alone.

The app must be in **multiple-revision mode**. In single-revision mode a new
revision takes all the traffic, so the extension refuses rather than put a
pull request on the front door.

## Labels

Without `labels`, the label is `pr-<n>` and every name is derived from the
number — `url` answers without touching Azure.

With `labels`, a pull request borrows a name from the pool: the one it already
holds (its revision name starts with `<app>--pr<n>-`), else the first the app
has no traffic entry for. All services borrow the same name. A full pool fails
`up` with `all N preview labels are in use`; free one with `down`. The label
is then a fact about the app rather than the number, so `url`, `down` and
`status` read it off the traffic array, and `url` errors for a pull request
that has no preview. `up --label <name>` uses that name regardless, taking it
over if something else holds it.

The point of a pool is an app that signs people in through Entra: it has to
register every callback URL by hand, and a pool of ten names registered once
beats a `pr-<n>` per pull request registered never.

## Commands

All take `<pr>`, a positive number, and honour `-o json`, which puts one JSON
object on stdout and moves the progress to stderr.

**`up <pr> [--sha] [--ref] [--label]`** — chooses the label; creates the
database when `database.create` is set; builds and pushes every service's
image as `<registry>/<image>:pr-<n>-<sha7>` with `BUILD_SHA`, `BUILD_REF` and
`BUILD_PR` as build args; runs `provision`; mints a revision per service and
labels it; waits for every revision to be healthy, deactivating them all if
one is not; deactivates the pull request's earlier revisions and removes
their tags; probes each `healthPath` for up to three minutes, advisory. In
GitHub Actions it appends `label=`, `url=` (the first service) and, for more
than one service, `url_<service>=` to `$GITHUB_OUTPUT`. `--sha` and `--ref`
default to `$BUILD_SHA` and `$BUILD_REF`.

```json
{
  "pr": 42, "label": "comet", "database": "plot_pr_42",
  "services": [
    {"name": "api", "app": "ca-plot-api", "url": "https://ca-plot-api---comet.example.io",
     "fqdn": "ca-plot-api---comet.example.io", "revision": "ca-plot-api--pr42-abc1234",
     "image": "acr.io/api:pr-42-abc1234"}
  ]
}
```

**`url <pr> [--service]`** — the preview's address. One service: the bare
URL. Several: `<service>=<url>` per line, or the bare URL of the one named.
JSON: `{"pr", "label", "urls": {"<service>": "<url>"}}`.

**`status <pr>`** — per service: app, revision, URL and health; and whether
the database exists. Exit 1 when no app carries the pull request's label.
JSON: `{"pr", "exists", "label", "database": {"name", "exists"}, "services":
[{"name", "app", "label", "revision", "url", "healthy"}]}` — `exists` and
`healthy` are `null` when there was nothing to ask.

**`promote <pr> [--service]`** — moves the labelled revision to 100% of
traffic on every service's app, or the one named. Only on a shared app, where
the preview is a revision of the app azd deploys; a dedicated preview app is
refused, because promoting there changes nothing anybody uses. Every other
entry goes to zero and keeps its label. JSON: `{"pr", "label", "services":
[{"name", "app", "revision"}]}`.

**`down <pr>`** (alias `rm`) — removes the label from every app, deactivates
the pull request's active revisions, drops the database when there is a
server, and untags `pr-<n>-*` in every service's repository. Deactivating
does not free a revision slot: an app holds 100 and purges the oldest past
that, so `maxInactiveRevisions` on the app is what protects rollback
history. JSON: `{"pr", "label", "database", "databaseDropped", "services":
[{"name", "app", "labelRemoved", "deactivated": [], "untagged": []}]}`.

## Running without an azd environment

A repository whose infrastructure was not provisioned by azd — one Plot
provisioned over ARM, say — leaves `previewEnvironment` unset and exports
what a deployment would have recorded, under the conventional names or the
ones its `outputs` and `services.<name>.app` refer to:

```sh
export AZURE_SUBSCRIPTION_ID=… AZURE_RESOURCE_GROUP=… AZURE_CONTAINER_REGISTRY_ENDPOINT=…
export APP_CONTAINER_APPS_ENV_DOMAIN=…            # <PREFIX>_…
export APP_API_CONTAINER_APP_NAME=… APP_UI_CONTAINER_APP_NAME=…
export APP_DATABASE_SERVER_NAME=…                 # whatever database.server names
azd preview up 42
```

`provision` still receives every value the azd environment would have
supplied — with none, it inherits the shell.

## Development

```sh
go test ./...
go generate ./...                       # regenerates preview.schema.json from internal/config
EXTENSION_ID=obtai.preview EXTENSION_VERSION=0.6.0 ./build.sh
```

Publishing goes through `azd x pack` and `azd x publish` against the OBT
extension registry; see `CHANGELOG.md` for what each version changed.
