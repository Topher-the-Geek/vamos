# CLI reference

Run `vamos --help` or `vamos <command> --help` for command-line help.
Targets are names in the current directory's `BUILD` file, or [workspace labels](configuration.md#workspaces).
`-C <dir>` chooses the directory whose targets can be named without a label.
It does not change the process's working directory: local output paths remain relative to where you run vamos.

## Commands

All commands that build targets reuse the [build cache](cache.md).
Delivery commands build the requested target and its dependencies before sending the result.

| Command | Accepted targets | Result |
| --- | --- | --- |
| `vamos build <target>...` | Pulls, layers, tasks, indexes | Keep results in the build cache |
| `vamos push <target> <image>` | Pulls, layers, indexes | Publish to an image registry |
| `vamos load <target> <image>` | Pulls, layers, indexes | Load an image into Docker |
| `vamos save <target> <file.tar>` | Pulls, layers, indexes | Write a Docker-loadable image archive |
| `vamos export <task> <folder>` | Tasks with output | Write a fileset into a local folder |
| `vamos prune` | No targets | Remove cached build results |

### `build`

```sh
vamos build lint test app
vamos build //backend:test //ui:lint -C path/to/workspace
vamos build test --rebuild
```

Accepts one or more target names.
An index builds all its images.
A successful check task can be cached even though it produces no files.

`--rebuild` runs the explicitly named targets again while allowing their dependencies to be reused.
For a named pull, it checks the registry again for the tag's current image.
See [refreshing results](cache.md#refreshing-results).

### `push`

```sh
vamos push app ghcr.io/me/app:1.0
vamos push release ghcr.io/me/app:1.0 --push-from=client
```

Publishes an image, or a whole index with its images, to the destination repository.
An image name without a tag gets `:latest`.
Destination names cannot include a digest.

`--push-from=executor` is the default: the executor pushes with its credentials and registry settings.
`--push-from=client` pushes with the client's credentials and settings.
The executor currently runs inside vamos, so both use your local environment.

### `load`

```sh
vamos load app app:dev
docker run --rm app:dev
```

Loads the image into Docker with the given name, defaulting its tag to `:latest`.
It uses `$DOCKER_HOST`, or `/var/run/docker.sock`, with TLS settings from `$DOCKER_CERT_PATH` and `$DOCKER_TLS_VERIFY`.
When the target is an index, it selects and builds only the image matching the Docker daemon's platform.
For a single-image target, the build platform defaults to the host unless `--platform` overrides it.

### `save`

```sh
vamos save app app.tar --tag app:dev
docker load -i app.tar
docker run --rm app:dev
```

Writes an archive that `docker load` can import.
`--tag <image>`, or `-t <image>`, records an image name; without it, the archive has no image name.
An existing output file is replaced only after the new image is complete.
The output file's parent directory must already exist.
For an index, only the image matching the host's platform is built and saved.
To save another platform, name a pull or layer directly and pass `--platform`.

`write` is a deprecated earlier name for `save` and still accepts the same arguments.

### `export`

```sh
vamos export codegen src/generated
vamos export codegen src/generated --replace
```

Requires a task declaring `source` and `include`.
Writes its files at paths relative to the task's `source`.
For example, a task selecting `/out/src/main.js` with `source = "/out"`
exports `src/main.js` beneath the destination folder.

The folder is replaced as a whole, rather than merged with existing contents.
A nonempty folder requires `--replace`.
Without that flag, if files appear in the folder during the build, export refuses to overwrite them.
Choose a dedicated generated-output directory.
Its parent directory must already exist.

When vamos runs as root, exported files receive the owner and group of the destination's parent directory.
Otherwise, files belong to the user running vamos, with the filesystem's usual group assignment.
They receive the task's timestamp.
Symbolic links are written as links, never followed.

### `prune`

```sh
vamos prune
```

Removes all cached layer images and other build results.
It keeps pulled images and the recorded resolutions of their tags.
To refresh a pull's tag, use `vamos build <pull-name> --rebuild`.

## Flags

These flags are shared by commands that build targets:

| Flag | Meaning | Default |
| --- | --- | --- |
| `-C`, `--dir <dir>` | Directory for unqualified target names and workspace discovery | `.` |
| `--set <name=value>` | Set a named target's parameter; repeat for several parameters | Parameter defaults |
| `--platform <os/arch[/variant]>` | Target platform for a pull, layer, or task | Host platform |
| `--tls-verify <bool>` | Require HTTPS and verify registry certificates when explicitly set | Registry settings |
| `--timestamp <time>` | Minimum timestamp for newly executed targets | Derived from bases and added tasks |

`--platform` cannot be given when naming an index.
With multiple build targets, each `--set` applies to named targets declaring that parameter,
and must match at least one named target.
See [parameters](configuration.md#parameters) for value syntax.

`--timestamp` accepts Unix seconds or RFC 3339, such as `2026-01-01T00:00:00Z`.
It does not invalidate existing cached results.

`--trace <file.json>` writes a [Perfetto trace](cache.md#traces)
and is available on `build`, `push`, `load`, `save` (including its deprecated `write` alias), and `export`.

The TLS flag displays `true` in help, but when omitted vamos defers to `registries.conf`,
including entries with `insecure = true`.
Use `--tls-verify=false` explicitly to contact a registry over plain HTTP.

Command-specific flags are `build --rebuild`, `push --push-from`, `save --tag`/`-t`, and `export --replace`.

## Registry credentials

Registry operations use your container registry authentication configuration,
including credentials from `docker login` in `~/.docker/config.json`.
If `$REGISTRY_AUTH_FILE` is set, only the credentials in that file are used.

When running vamos itself inside Docker, make the authentication file available inside that container.
For the tutorial's Docker function, add this mount to its `docker run` invocation after logging in:

```sh
-v "$HOME/.docker/config.json:/root/.docker/config.json:ro"
```

If the configuration uses a credential helper, that helper must also be available inside the vamos container.
Alternatively, mount an auth file and point `REGISTRY_AUTH_FILE` to its container path:

```sh
-v "$PWD/auth.json:/run/registry-auth.json:ro" -e REGISTRY_AUTH_FILE=/run/registry-auth.json
```

## Output and failures

Build progress reports target states such as `building` and `cached`, along with command output.
Cached targets replay output from their previous execution.
Successful delivery prints one of these lines:

```text
Image built! <destination> <image-id>
Index built! <destination> <digest>
Files exported! <folder> <digest>
```

Errors exit with status 1 and a `vamos:` prefix.
BUILD errors include the file, line, and column.
Command-line usage mistakes print usage; runtime failures do not.

## Common problems

| Symptom | What to check |
| --- | --- |
| `Operation not permitted` during namespace creation | Use the [tutorial setup](../tutorial.md#2-set-up-vamos) |
| `exec format error` in a build step | Use native tools or register emulation for the requested platform |
| An `add()` source is rejected | It must name a glob or a task with both `source` and `include` |
| A glob matches no files | Check its paths relative to the BUILD file and its exclusions |
| Export refuses a nonempty folder | Choose a dedicated output folder or pass `--replace` |
| A moved registry tag still uses an old image | [Rebuild the pull target](cache.md#refreshing-results) |
| Docker cannot be reached | Check the socket mount, `DOCKER_HOST`, and Docker's TLS environment variables |
