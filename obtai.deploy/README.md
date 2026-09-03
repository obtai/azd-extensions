# OBT Previews

An azd extension

## Installation

Run `azd ext install obtai.preview`

## Usage

`azd preview <command> [options]`

## Commands

### `context`

Displays the current `azd` project, environment and deployment context.

### `prompt`

Demonstrates the prompting capabilities available to extensions, including selecting an Azure subscription, resource group and resource.

## Development

The hidden `listen` command registers project and service lifecycle event handlers that `azd` invokes automatically. Customize the handlers in `internal/cmd/listen.go`.

| Command | Description |
| ------- | ----------- |
| `azd x build` | Build the extension and install it locally. |
| `azd x watch` | Watch for changes and automatically rebuild and reinstall. |
| `azd x pack` | Package the extension into a distributable artifact. |
| `azd x publish` | Publish the extension to an extension source. |
| `azd x release` | Create a GitHub release for the extension. |

Learn more about building extensions in the [azd extension framework docs](https://github.com/Azure/azure-dev/blob/main/cli/azd/docs/extensions/extension-framework.md).
