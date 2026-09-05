# Release History

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
