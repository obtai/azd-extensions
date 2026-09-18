# OBT Shell

An [azd extension](https://github.com/Azure/azure-dev/blob/main/cli/azd/docs/extensions/extension-framework.md)
that opens a shell in a running Azure Container Apps container — the idea of
[ecsgo](https://github.com/tedsmitt/ecsgo), for Container Apps.

`az containerapp exec` wants every name spelt out. `azd shell` asks instead:
app, revision, replica, container, each from a filterable picker, and any step
with only one choice is taken without asking. Inside an azd project it already
knows the subscription and resource group, and apps can be named by the azd
service they host.

```sh
azd ext install obtai.shell

azd shell                        # pick everything
azd shell -a api                 # the app hosting the azd service `api`
azd shell -a api -c bash         # something other than /bin/sh
azd shell -a api -c 'env'        # run one command and come back
azd shell -e prod -a web         # another azd environment
echo 'ls /app' | azd shell -a api
```

The command is `shell` because azd already has an `exec` of its own.

## Flags

| Flag | | Default |
|---|---|---|
| `--subscription` | `-s` | the azd environment's `AZURE_SUBSCRIPTION_ID`, then the shell's, then a picker |
| `--resource-group` | `-g` | the azd environment's `AZURE_RESOURCE_GROUP`, then the shell's; unset, apps are listed across the subscription and the group is picked first |
| `--app` | `-a` | a picker. Matches the app's name or its `azd-service-name` tag |
| `--revision` | `-r` | a picker over active revisions, the one taking traffic first |
| `--replica` | | a picker over the revision's replicas |
| `--container` | `-u` | a picker over the replica's containers (init containers excluded) |
| `--command` | `-c` | `/bin/sh` |
| `--quiet` | `-q` | print nothing but the container's output |

azd's own `-e`, `--no-prompt` and `--debug` work as usual. `--debug` also
shows the exec proxy's status messages.

Without a terminal, or with `--no-prompt`, a step with more than one choice is
an error that lists the choices and names the flag that settles it. The
pickers draw on stderr, so `azd shell -a api -c env > env.txt` still prompts.

A revision scaled to zero has no replica to connect to; hit the app's URL to
wake it, or set `minReplicas`.

## How it connects

The same websocket `az containerapp exec` uses: an app-scoped token from the
container app's `getAuthToken`, and the replica container's exec endpoint.
Everything else goes through the Azure SDK with the Azure CLI's login, or a
managed identity where `IDENTITY_ENDPOINT` is set. Port forwarding, which
ecsgo has, is missing because Container Apps has no API for it.

## Development

```sh
go test ./...
EXTENSION_ID=obtai.shell EXTENSION_VERSION=0.1.0 ./build.sh

# try it without publishing
azd x build --skip-install && azd x pack --bundle -o /tmp/obtai-shell
azd ext install /tmp/obtai-shell/obtai-shell_0.1.0.zip --force
```
