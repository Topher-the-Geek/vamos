# CLI reference

Run `vamos --help` or `vamos <command> --help` for command-line help.
Targets are names in the current directory's `BUILD` file, or [workspace labels](configuration.md#workspaces).
Any other name fails before anything runs,
suggesting the name that was meant where one is close, and otherwise naming the targets there are.
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
| `vamos coordinator` | No targets | Coordinate builds across workers for clients on the network |
| `vamos worker` | No targets | Run builds that a coordinator assigns |

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

`--push-from=executor` is the default: the executor pushes with its credentials and registry settings,
from its own environment: `registries.conf` and `REGISTRY_AUTH_FILE`.
`--push-from=client` pushes with the client's credentials and settings, including `--tls-verify`.
The executor currently runs inside vamos, so both read your local environment,
but only the client honours the command line's `--tls-verify`.
A push retries transient registry failures as `containers.conf` configures, then reads the tag back
from the registry it wrote to, never a mirror, and fails if it names another manifest.

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

Writes an archive that `docker load` can import, at any path, colons and all,
even inside the project, whose globs never read the archive being written.
`--tag <image>`, or `-t <image>`, records an image name; without it, the archive has no image name.
An existing output file is replaced only after the new image is complete.
The output file's parent directory must already exist, and the output path must not be a directory.
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
They receive the task's timestamp, and keep the modes the task selected them with.
Symbolic links are written as links, never followed.

### `prune`

```sh
vamos prune
```

Removes all cached layer images and other build results.
It keeps pulled images and the recorded resolutions of their tags.
To refresh a pull's tag, use `vamos build <pull-name> --rebuild`.


### `coordinator`

```sh
vamos coordinator --listen :8080 --cache-repository registry:5000/vamos-cache --pull-through docker.io=hub-proxy:5000
```

Coordinates builds for clients on the network, across the workers that register with it, until interrupted.
A client names it with `--coordinator http://host:8080`, or `VAMOS_COORDINATOR`, on any command that builds or prunes:
its sources are uploaded to the coordinator's build cache, which `--cache-dir` places, `/var/lib/vamos` by default;
the coordinator schedules the targets across the workers, with those of one build running at once on different
workers, up to `--bound`, and the client reads the results back for delivery.
Simultaneous builds that need the same target run it once and share its result.
A client whose connection drops reconnects and resumes watching its build, and gives up after `--grace`,
with a message saying that finished work is cached.
The coordinator keeps the same grace period: a build that no client watches for that long, as when the client was
killed, starts nothing more, while the targets already running finish and are cached, and the build fails saying
it expired; a client holds its build through delivery and releases it when done, so a prune waits for it.
A client and a coordinator must run the same version of vamos's build graph; the client refuses one that doesn't.
`prune --coordinator URL` empties the coordinator's build cache and the cache repository's tags, keeping tag records
and the pull-through cache; the cache registry must have deletion enabled, as the compose file's does.
It is refused while any build or push is open, a finished build that its client still holds included, while attempts
are still ending on the workers, and for a lease after the coordinator started, and a build whose sources were
captured before a prune is refused at submit with a message to run it again.
`--window` says how long a finished build stays readable after it ends or was last watched before the coordinator
forgets it; `--grace` if shorter.

`load`, `save` and `push --push-from=client` read a built image from the cache repository, and a `pull()` root from
the pull-through cache, with the client's credentials and `--tls-verify`, so the client must reach both;
`push` from the executor runs on a worker, copying from there with the worker's credentials.
`export` reads a task's files from the coordinator's build cache.

Workers push every layer they build to `--cache-repository`, an OCI registry repository, tagged by image ID,
before reporting it, so that any worker can fetch it and a result survives the worker that made it;
they pull base images through the pull-through caches that `--pull-through registry=cache` maps,
and a `pull()` from an unmapped registry fails with a message naming the mapping to add.
The coordinator checks cached layers against the cache repository and deletes its tags when it prunes, so its
credentials need read and delete access to it, and the workers' need push access.
A target's commands run on a worker of its image's platform when the deployment offers that platform,
so a pool of amd64 and arm64 workers builds both platforms of an index natively.
A build runs natively on the platforms offered when it is submitted, by `--offer` or by a registered worker,
and on any offered later that it reaches; it keeps them, across a restart too,
so if the only worker of one leaves, its targets for that platform wait for another rather than being emulated.
Commands for a platform the deployment doesn't offer, as `--platform linux/arm64` asks of an amd64 pool, run
under emulation on the execution platform's workers only when the build gives `--emulate`,
a flag kept out of `--help` until local builds take it too;
without it the target fails before anything runs, naming the platforms and the ways out.
Emulated work goes to workers that report an emulator for the platform, which they find in the kernel's binfmt_misc
handlers; `--offer-emulated os/arch` says the deployment has such workers, so a build waits for one,
where a platform nobody emulates fails the target at once.
`--offer os/arch` names a platform the deployment has workers for, besides those registered now:
a build for an offered platform without a worker present waits, printing that it waits for a worker of that
platform after a heartbeat interval, so offer only platforms the deployment serves; one for a platform never
offered is refused.
Pulls and targets that only add files prefer a worker of their image's platform,
but run on any worker while none of that platform is registered.
`--default-exec-platform os/arch`, one that `--offer` names, is the execution platform clients use
unless they give `--exec-platform`, so that clients of every architecture configure `EXEC_PLATFORM` alike
and reuse each other's results; without it each client uses its own platform where the coordinator offers it.
A worker that stops renewing an attempt's lease (`--lease`) is lost, and its work tried again elsewhere,
up to `--retries` times; a push is never retried, since its write may have happened.
Beyond `--max-builds` open builds, submissions are refused as busy.

There is no authentication: run a coordinator on a trusted network alone, where anyone who can reach it
can build on it and read its cache, and anyone who can reach a pull-through cache reads what its upstream
credentials can.
The coordinator keeps its jobs in `coordinator/` under `--cache-dir`, so a restart that keeps that directory resumes
them: clients reconnect within `--grace`, warn that the coordinator restarted, and see their builds finish.
A resumed build runs again from its start: what finished before the restart is cached, and what was running starts
again on a worker, since workers stop what they ran for a coordinator they lost and register again.
A push or prune that was running fails, since whether it wrote is unknown, and a cancelled build stays cancelled.
Two coordinators can't share that directory: the second refuses to start.
A coordinator started without that directory has none of its jobs: clients report that their builds were lost,
and rerunning reuses what finished.

### `worker`

```sh
vamos worker --coordinator http://coordinator:8080 --capacity 2
```

Runs the attempts the coordinator assigns, up to `--capacity` at once, each in a subprocess of its own in a cgroup,
so that cancelling a build kills every process the attempt started, however it daemonized itself.
A worker needs root, cgroup v2 and buildah's chroot isolation, as building does, and a container store of its own,
which is a disposable cache of base images and built layers.
It makes its cgroups under `/sys/fs/cgroup/vamos`, so it needs a cgroup namespace of its own, as Docker gives a
container and Kubernetes doesn't give a privileged one; the Helm chart in `remote/helm/vamos` starts it in one.
It renews each attempt's lease by heartbeat, and kills an attempt itself when it can't,
so that the coordinator can count an expired lease as a stopped attempt;
after a kill, or on starting after a crash, it removes the attempt's container.
Workers need egress to the pull-through caches, the cache repository, and whatever `run()` steps fetch.
Their registry credentials are their own; a client's never reach them.

## Flags

These flags are shared by commands that build targets:

| Flag | Meaning | Default |
| --- | --- | --- |
| `-C`, `--dir <dir>` | Directory for unqualified target names and workspace discovery | `.` |
| `--set <name=value>` | Set a named target's parameter; repeat for several parameters | Parameter defaults |
| `--secret <id=ID,env=VARIABLE>`, `--secret <id=ID,src=FILE>` | Give `run()` steps' secret `ID` the value of an environment variable or a file; repeat for several secrets | None |
| `--platform <os/arch[/variant]>` | Target platform for a pull, layer, or task | Host platform |
| `--exec-platform <os/arch[/variant]>` | Platform that `EXEC_PLATFORM` names, and that runs `run()` steps for platforms the executor doesn't offer; the executor must offer it | The coordinator's `--default-exec-platform`, or else this machine's, or the only one offered |
| `--coordinator <URL>` | Build on the coordinator at this URL rather than in this process | `$VAMOS_COORDINATOR`, or none |
| `--tls-verify <bool>` | Require HTTPS and verify certificates when vamos itself contacts a registry: pushing from the client, and reading a coordinator's images to deliver | `registries.conf` |
| `--timestamp <time>` | Minimum timestamp for newly executed targets | Derived from bases and added tasks |

`--platform` cannot be given when naming an index.
With multiple build targets, each `--set` applies to named targets declaring that parameter,
and must match at least one named target.
See [parameters](configuration.md#parameters) for value syntax.

`--timestamp` accepts Unix seconds or RFC 3339, such as `2026-01-01T00:00:00Z`.
It does not invalidate existing cached results.

### Secrets

`--secret` gives the [secrets](build.md#secrets) that `run()` steps name:

```sh
vamos build //react:build --set docker_tag=1.2.3 --secret id=sentry,env=SENTRY_AUTH_TOKEN
vamos build //app:deps --secret id=npmrc,src=$HOME/.npmrc
```

`env=` reads an environment variable of the vamos process, which must be set, though it may be empty;
`src=` reads a file.
They are read only when a build runs, and a secret that a step needs but no `--secret` gives fails the build
before anything runs.
Values stay in the vamos process: they never enter the build cache, images, traces or the graph.
`--secret` can't be combined with `--coordinator`, since a coordinator has no authentication or TLS;
build secrets in your own process.

`--trace <file.json>` writes a [Perfetto trace](cache.md#traces)
and is available on `build`, `push`, `load`, `save` (including its deprecated `write` alias), and `export`.

The TLS flag displays `true` in help, but when omitted vamos defers to `registries.conf`,
including entries with `insecure = true`.
It applies to what the client itself does: a push with `--push-from=client`, and reading a coordinator's images to
load, save or push;
the executor, which pulls and pushes otherwise, follows `registries.conf` alone,
so mark a plain-HTTP registry `insecure = true` there for it.

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
A target configured alike for several platforms of an index, such as a task on an `EXEC_PLATFORM` base,
is built once: the others report `building, shared with` the one whose work they share,
or with `another build` when another build started it.
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
| `can't reach the coordinator at ...` | Check the URL in `--coordinator` or `VAMOS_COORDINATOR`, and that this machine is on the coordinator's network |
| `the coordinator ... runs graph versions ..., not this vamos's` | Use a vamos of the coordinator's version, or upgrade the coordinator |
| `waiting for a worker that runs ...; none is registered` | Start a worker of that platform, or interrupt the build and run it again; a platform that `--offer` names is waited for again, so stop offering it first |
| `waiting for a worker; none is registered` | Start a worker |
| `its commands would run under emulation` | Add a worker of the image's platform, give `--platform` for one that runs them, or `--emulate` to allow emulation on a worker that has an emulator |
| `the connection to the coordinator was lost for ...` | Run the command again once the coordinator is reachable; finished targets are cached |
| `the build expired` | The build went unwatched for the coordinator's grace period; run the command again |
| `the build cache was pruned while the build's sources were being captured` | Run the command again |
| A build hangs with no output over a coordinator | Check the coordinator's and workers' logs with `docker compose logs`, and that a worker of the platform is registered |
