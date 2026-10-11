# vamos

A Go CLI that uses the [buildah](https://github.com/containers/buildah) library to build
a container image described by a `BUILD` file written in [Starlark](https://github.com/bazelbuild/starlark).

```
vamos push <image>                # push to a registry, e.g. ghcr.io/me/app:1.0
vamos load <image>                # load into the local Docker daemon, e.g. app:dev
vamos write <file.tar> [-t app]   # write a tarball that `docker load` can import
vamos build <step>... [--rebuild] # build steps into the build cache, e.g. packages
vamos prune                       # empty the build cache
```

Each command builds from the `BUILD` file in the current directory, or in the directory
given with `-C <dir>`. Image names without a tag get `:latest`.

- `push` uses credentials from `docker login` (`~/.docker/config.json`) or, if set, only those
  in `$REGISTRY_AUTH_FILE`. Use `--tls-verify=false` for a registry served over plain HTTP, or mark
  it `insecure = true` in `registries.conf`.
- `load` talks to the Docker daemon as the `docker` command does: at `$DOCKER_HOST`, or
  `/var/run/docker.sock`, with TLS settings from `$DOCKER_CERT_PATH` and `$DOCKER_TLS_VERIFY`.
- `write` records no image name in the tarball unless `--tag` is given. An existing file is
  replaced only once the new image is complete.

Run `vamos <command> --help` for details.

## Build cache

Each step's result is cached, and a later build reuses it instead of running the step again, as long as
the step's inputs are the same: its base image, its arguments, and the content, modes and links of its
sources. Modification times and step names aren't inputs, so touching a file or renaming a step doesn't
make it run again. A build says which steps it reused, and replays what they printed when they ran:

```
layer "packages": cached
layer "dev-packages": cached
task "compile": building
layer "server": cached
```

A cached result is reused even if running the step again would make something different, as when it
downloads the latest version of a dependency. `push`, `load` and `write` never run a cached step again.
To run one again, pass its name to `vamos build --rebuild`, which runs the named steps again, reusing the
steps they use, and replaces their results; steps that use them then run again if their files changed.
Naming a `pull()` pulls it again, in case its tag names a newer image.

```
vamos build packages --rebuild
```

Layers are kept as images named `localhost/vamos-cache:<id>` in the container store, and the rest of the
cache beside it, in `/var/lib/containers/vamos`, so a volume on `/var/lib/containers` keeps both. Nothing is
removed until `vamos prune`, which empties the cache but keeps pulled images.

So that the same inputs make the same image, every file a step adds is stamped with the creation time of
the pulled images it is built on, which is also the image's creation time. `--timestamp` gives a later time
to stamp steps that run with, in Unix seconds or as `2026-01-01T00:00:00Z`; it isn't an input, so steps
that are reused keep the time they were built with.

## BUILD files

A `BUILD` file declares named steps. `pull()` fetches an image from a registry, `glob()` names a set of
files beside the `BUILD` file, and `layer()` adds a layer to the image of an earlier step, which may itself be
a `layer()`. `task()` runs a command on an image and collects the files it makes, as Docker's multi-stage
builds do, for later steps' sources. Only `pull()` contacts a registry.

```python
pull(
    name="node",                                   # required: names the step for later layers
    image="docker.io/library/node",                # required: image to fetch, without a tag
    tag="24-alpine",                               # defaults to "latest"
)

glob(
    name="package-manifest",                       # required: names the files for layers' sources
    include=["package.json", "package-lock.json"], # required: files or directories relative to the BUILD file
)

layer(
    name="packages",                               # required: names the step
    base="node",                                   # required: an earlier pull() or layer(), or "scratch"
    sources=["package-manifest"],                  # earlier glob()s and task()s to copy in
    destination="/app",                            # directory to copy sources into
    run=["npm", "ci", "--prefix", "/app"],         # command run at build time
)

layer(
    name="dev-packages",                           # adds the compiler, for the task below
    base="node",
    sources=["package-manifest"],
    destination="/build",
    run=["npm", "ci", "--prefix", "/build"],
)

glob(
    name="server-source",
    include=["server.ts", "tsconfig.json", "tsconfig.build.json"],
)

task(
    name="compile",                                # required: names the files for sources
    base="dev-packages",                           # required: an earlier pull() or layer(), or "scratch"
    sources=["server-source"],                     # earlier glob()s and task()s to copy in
    destination="/build",                          # directory to copy sources into
    command=["npm", "run", "build", "--prefix", "/build"],  # command to run
    source="/build/dist",                          # required: directory the files are made in
    include=["server.js"],                         # required: files or directories relative to source
)

layer(
    name="server",
    base="packages",
    sources=["compile"],
    destination="/app",
    command=["node", "/app/server.js"],            # the image's default command
)
```

The last `layer()` makes the image. Every other step must be used by a later `layer()` or `task()`, as its base
or in its sources, so nothing is declared but left out. A layer keeps its base's files and command, as Docker's `FROM`
does; only the last one's image is sent, and the others are kept in the build cache. A pulled image is kept
in local storage and reused. `base="scratch"` starts from an empty image, so no step may be named `scratch`.
A task's image is discarded once its files are collected and cached, so its base can hold build tools the
image doesn't need. A task can't be a base.

Sources keep their paths relative to the `BUILD` file, or to a task's `source`, beneath `destination`: with
`include=["src/main.ts"]` and `destination="/app"`, the file lands at `/app/src/main.ts`. A directory brings
everything in it, and `"."` the whole project. Patterns such as `*.ts` aren't supported yet.
Folders above an included path that the image already has, such as `/tmp` for `include=["tmp/x"]` and
`destination="/"`, keep their own mode and owner. A step's sources can't include the same path from two places,
such as the project and a task, since one would replace the other.

Included paths and their parent directories, within the project or a task's `source`, must not be symbolic
links. Links in `source` itself are resolved within the task's container.
Links inside an included directory are copied as links, as Docker's `COPY` does.

See [examples/nodejs/](examples/nodejs/) and [examples/fastmcp/](examples/fastmcp/).

## Build

```
docker build . -t vamos
```

## Test

```
go test ./...          # everything, including image builds; needs root
go test -short ./...   # command line and BUILD file checks only
```

Image builds use `busybox` from Docker Hub. `push` is tested against an in-process registry
and `load` against a fake Docker daemon; if a real Docker daemon is running, `load` is also
checked against it. The tests use the machine's build cache, and one of them empties it, so the
next build of your own project runs every step again.

## Run

Buildah needs to create namespaces and mounts, which Docker's defaults don't allow.
Without extra permissions the run fails with
`Error during unshare(CLONE_NEWUSER): Operation not permitted`.

Run with full privileges, loading the image straight into the host's Docker:

```
docker run --rm -it --privileged \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD/examples/nodejs:/example" \
  vamos load vamos-example -C /example
```

Or grant only what buildah needs:

```
docker run --rm -it \
  --cap-add SYS_ADMIN \
  --security-opt seccomp=unconfined \
  --security-opt apparmor=unconfined \
  --security-opt label=disable \
  -v "$PWD/examples/nodejs:/example" -v "$PWD/output:/output" \
  vamos write /output/example.tar -t vamos-example -C /example
```

This variant writes a tarball instead. To push, use `vamos push` and mount your credentials,
e.g. `-v ~/.docker/config.json:/root/.docker/config.json:ro`.

On success `push`, `load` and `write` print `Image built! <destination> <image-id>`.

## Run the built image

```
docker run -i --rm vamos-example
```

If you wrote a tarball, `docker load -i output/example.tar` first.

The example is an [MCP](https://modelcontextprotocol.io) server with an `echo` tool, speaking MCP over stdin and
stdout. Send it a request with `docker run -i --rm vamos-example < examples/nodejs/echo-request.jsonl`.
