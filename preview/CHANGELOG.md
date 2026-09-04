# Release History

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
