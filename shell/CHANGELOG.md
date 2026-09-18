# Release History

## 0.1.0

### Features Added

- `azd shell`: a shell in a running container app, picked step by step —
  subscription, resource group, app, revision, replica, container — with
  Charm's huh. A step with one choice is skipped; every step has a flag.
- Inside an azd project the subscription and resource group come from the
  environment, and `--app` accepts the azd service name.
- `--command` runs something other than `/bin/sh`, including one-off commands.
  Piped stdin is sent to the container and ended with Ctrl-D.
- Terminal resizes follow the local terminal.
