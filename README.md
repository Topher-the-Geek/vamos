# vamos

A Go CLI that uses the [buildah](https://github.com/containers/buildah) library to build
container images described by `BUILD` files written in [Starlark](https://github.com/bazelbuild/starlark).

```
vamos push <target> <image>              # push a target's image to a registry, e.g. server ghcr.io/me/app:1.0
vamos load <target> <image>              # load it into the local Docker daemon, e.g. server app:dev
vamos write <target> <file.tar> [-t app] # write a tarball that `docker load` can import
vamos build <target>... [--rebuild]      # build targets into the build cache, e.g. packages
vamos prune                              # empty the build cache
```

Targets are named as the `BUILD` file in the current directory, or in the directory given with `-C <dir>`,
declares them, or by a label such as `//backend:server` for a target of another `BUILD` file in the
[workspace](#workspaces). Each command builds only the targets it names and those they use. `push`, `load`
and `write` send the image of a `pull()` or `layer()`, or an `index()` of images for several
[platforms](#platforms). Image names without a tag get `:latest`.

- `push` uses credentials from `docker login` (`~/.docker/config.json`) or, if set, only those
  in `$REGISTRY_AUTH_FILE`. Use `--tls-verify=false` for a registry served over plain HTTP, or mark
  it `insecure = true` in `registries.conf`.
- `load` talks to the Docker daemon as the `docker` command does: at `$DOCKER_HOST`, or
  `/var/run/docker.sock`, with TLS settings from `$DOCKER_CERT_PATH` and `$DOCKER_TLS_VERIFY`.
- `write` records no image name in the tarball unless `--tag` is given. An existing file is
  replaced only once the new image is complete.

Run `vamos <command> --help` for details.

## Build cache

Each target's result is cached, and a later build reuses it instead of running the target again, as long
as the target's inputs are the same: its base image, its arguments and steps, and the content, modes and
links of the files it adds. Modification times and target names aren't inputs, so touching a file or renaming
a target doesn't make it run again. A build says which targets it reused, and replays what they printed when
they ran:

```
layer "packages": cached
layer "dev-packages": cached
task "compile": building
layer "server": cached
```

A cached result is reused even if running the target again would make something different, as when it
downloads the latest version of a dependency. `push`, `load` and `write` never run a cached target again.
To run one again, pass its name to `vamos build --rebuild`, which runs the named targets again, reusing the
targets they use, and replaces their results; targets that use them then run again if their files changed.
Naming a `pull()` asks the registry what its tag names again, in case it names a newer image: until then, builds
use what it named when it was first pulled, even if it has moved since, without reaching the registry.

```
vamos build packages --rebuild
```

Layers are kept as images named `localhost/vamos-cache:<id>` in the container store, and the rest of the
cache beside it, in `/var/lib/containers/vamos`, so a volume on `/var/lib/containers` keeps both. Nothing is
removed until `vamos prune`, which empties the cache but keeps pulled images, and what their tags named.

So that the same inputs make the same image, every file a target adds is stamped with the creation time of
the pulled images it is built on, which is also the image's creation time. `--timestamp` gives a later time
to stamp targets that run with, in Unix seconds or as `2026-01-01T00:00:00Z`; it isn't an input, so targets
that are reused keep the time they were built with.

## BUILD files

A `BUILD` file declares named targets. `pull()` fetches an image from a registry, `glob()` names a set of
files beside the `BUILD` file, and `layer()` adds a layer to the image of an earlier target, which may itself be
a `layer()`. `task()` runs commands on an image and collects the files they make, as Docker's multi-stage
builds do, for later targets to add. Only `pull()` contacts a registry.

```python
pull(
    name="node",                                   # required: names the target for later layers
    image="docker.io/library/node",                # required: image to fetch, without a tag
    tag="24-alpine",                               # defaults to "latest"
)

glob(
    name="package-manifest",                       # required: names the files for add()
    include=["package.json", "package-lock.json"], # required: files or directories relative to the BUILD file
)

layer(
    name="packages",                               # required: names the target
    base="node",                                   # required: an earlier pull() or layer(), or "scratch"
    steps=[                                        # applied in order, as a Dockerfile's are
        add("package-manifest", "/app"),           # copies in an earlier glob() or task()
        workdir("/app"),                           # the directory later commands, and the image's, run in
        build_env(npm_config_cache="/tmp/npm"),    # variables for this target's later commands only
        run(["npm", "ci", "--omit=dev"]),          # runs a command at build time
        run(["rm", "-rf", "/tmp/npm"]),
    ],
)

layer(
    name="dev-packages",                           # adds the compiler, for the task below
    base="node",
    steps=[
        add("package-manifest", "/build"),
        workdir("/build"),
        run(["npm", "ci"]),
    ],
)

glob(
    name="server-source",
    include=["*.ts", "tsconfig*.json"],            # patterns: see "Selecting files" below
    exclude=["*.test.ts"],
)

task(
    name="compile",                                # required: names the files for add()
    base="dev-packages",                           # required: an earlier pull() or layer(), or "scratch"
    steps=[                                        # as for layer(); runs in /build, its base's workdir
        add("server-source", "/build"),
        run(["npm", "run", "build"]),
    ],
    source="/build/dist",                          # directory the files are made in
    include=["*.js"],                              # patterns relative to source, as for glob()
)

layer(
    name="server",
    base="packages",
    steps=[
        add("compile", "/app"),
        env(NODE_ENV="production"),                # variables for later commands, and the image's
    ],
    command=["node", "server.js"],                 # the image's default command
)
```

A layer keeps its base's files, command, variables and working directory, as Docker's `FROM` does. The image of
the layer named on the command line is sent, and the others are kept in the build cache. A target nothing uses
is fine, to build with `vamos build`. A pulled image is kept in local storage, named by its digest, and reused.
If its tag names an index of images for several platforms, the one for the platform being built is pulled
(see [Platforms](#platforms)). `base="scratch"` starts
from an empty image, so no target may be named `scratch`. A task's image is discarded once its files are collected
and cached, so its base can hold build tools the image doesn't need. A task can't be a base.

A task without `source` and `include` makes no files, so no target can add it. It runs only for its steps to
succeed, as a task that runs tests or a linter does, and is cached like any other:

```python
task(
    name="test",
    base="dev-packages",
    steps=[add("server-source", "/build"), run(["npm", "test"])],
)
```

A target applies its steps in order, and each of them sees what those before it did:

- `add(source, dest)` copies the files of an earlier `glob()` or `task()`, or one of another package, into the
  absolute directory `dest`.
  Files added later replace those added before. They are owned by root, unless `owner` or `group` say otherwise:
  `add("build", "/app", owner="node")` gives them to the image's user `node` and that user's group, and
  `group="staff"` alone keeps root as the owner. Each is a name in the image, or a numeric ID. Folders that
  `dest` and the files' paths need, but the image lacks, are made and given to the files' owner too, as
  Docker's `COPY --chown` does: add to a `dest` the image already has, such as one made by `workdir()`, to keep
  folders such as `/opt` owned by root.
- `run(argv)` runs a command, which must succeed, in the target's container. It isn't run through a shell, so
  use `["sh", "-c", "..."]` for one.
- `env(NAME="value", ...)` sets variables for later commands and in the image, as a Dockerfile's `ENV` does.
  `build_env(...)` sets them for the target's later commands only, leaving them out of the image. Either takes a
  dict of variables as `**vars`, which also allows names that aren't Starlark identifiers. Values refer to
  variables as `$NAME` or `${NAME}`, such as `env(PATH="/app/bin:$PATH")`, which see the variables as they
  were before the call; an unset one is empty. Write `$$` for a `$`. Forms such as `${NAME:-default}` aren't
  supported.
- `workdir(path)` makes the absolute directory `path` and sets it as the working directory of later commands
  and of the image, as a Dockerfile's `WORKDIR` does.

`add()`'s `dest` and `workdir()`'s `path` refer to variables as values do, such as `workdir("$HOME/app")`,
and must be absolute once they are expanded. As in a Dockerfile, variables are expanded while the image is
built, so the image holds their values.

### Selecting files

`glob()` selects files beside its `BUILD` file, and a task's output selects files within its `source`, with the
same arguments:

```python
glob(
    name="code",
    include=["src/**/*.ts", "package.json"],  # required: patterns of the files, relative to the BUILD file
    exclude=["**/*.test.ts"],                 # patterns of files and folders to leave out
    mode_mask=0o755,                          # the mode bits kept of each file and folder; the default
    allow_no_files=False,                     # the default: it is an error if no file matches
    max_file_count=10000,                     # defaults: a build stops at a glob over a limit
    max_file_size=100 * 1024 * 1024,          # bytes, of any one file
    max_total_size=1024 * 1024 * 1024,        # bytes, of all the files together
)
```

- A pattern's `*` matches any part of a name, but never `/`, and `?` and `[a-z]` match one character, as in Go's
  `path.Match`. `*` matches names that start with `.`, too. `**` matches any number of folders, even none, and
  must be a whole part of the path: `src/**/*.ts`, not `src/**.ts`.
- Patterns are relative to the `BUILD` file, or to `source`, so `*.ts` matches only files beside it, and
  `**/*.ts` matches them at any depth.
- A path without wildcards also brings everything beneath it, so `include=["src"]` is `src/**`, and `"."` is the
  whole directory.
- Patterns select files and links. Folders between a pattern's leading folders and the files it selects come
  along, with their modes, but empty folders are left out.
- An exclude pattern that matches a folder leaves out everything in it.
- The limits catch a pattern that reaches further than meant, before all of its files are read.

Only the folders a pattern could match within are read. Its leading folders, `src` for `src/**/*.ts`, say where
to start, so other folders, such as a large `node_modules`, are never read. An exclude pattern that matches a
folder, such as `**/node_modules`, keeps a pattern such as `**/*.ts` out of it.

Files keep their paths relative to the `BUILD` file, or to a task's `source`, beneath `dest`: with
`include=["src/main.ts"]` and `add("code", "/app")`, the file lands at `/app/src/main.ts`. A pattern's leading
folders aren't part of what it selects, so ones the image already has, such as `/tmp` for `include=["tmp/x"]`
and `dest="/"`, keep their own mode and owner.

A pattern's leading folders, within the project or a task's `source`, must exist, and must not be symbolic
links. Links in `source` itself are resolved within the task's container. Links that patterns select are copied
as links, as Docker's `COPY` does, and are never followed.

Whether a glob's leading folders exist is checked before anything is built, but only for globs that the targets
being built add, so a missing file doesn't stop building targets that don't need it. Whether its patterns match
any file, and its limits, are checked as its files are read.

See [examples/typescript-mcp/](examples/typescript-mcp/) and [examples/fastmcp/](examples/fastmcp/).

## Workspaces

A project can have a `BUILD` file in each of several directories, or packages, beneath a directory holding a
`WORKSPACE` file, which may be empty. A `BUILD` file names the targets of another by label: `//dir:name`, where
`dir` is the package's directory relative to the workspace, and `//:name` names one in the workspace's own
`BUILD` file. Any kind of target can be named this way, such as another package's `pull()` as a base, or its
`glob()` or `task()` in `add()`; a glob's files are relative to its own `BUILD` file. A target of the same file
is named as `name` or `:name`, and must be declared earlier, unless a function of `ctx` names it (see
[Parameters](#parameters)); any target of another package can be named.

```python
task(
    name="graphql-codegen",
    base="packages",
    steps=[
        add("//backend:schema", "/workspace"),     # a task in backend/BUILD
        run(["pnpm", "graphql-codegen"]),
    ],
    source="/workspace",
    include=["src/gql"],
)
```

vamos looks for the `WORKSPACE` file in the directory given with `-C` and the directories above it; without one,
labels are relative to `-C`. It loads only the `BUILD` files that the named targets need, and reports packages
whose targets name each other in a cycle. On the command line, targets of `-C`'s own `BUILD` file can be named
without a label:

```
vamos load layer strawberry-ui -C examples/strawberry-react/ui
vamos build //backend:test //ui:lint -C examples/strawberry-react
```

See [examples/strawberry-react/](examples/strawberry-react/).

## Parameters

A target can take parameters, such as a build mode or the languages to bundle, which make it behave like a
function. `params` declares them, each with its default, whose type (a string, a bool, an int, or a list of
strings) is the type of the values it takes. `param()` declares more: the values allowed, or, with `type` and
no default, a parameter every use must give.

```python
glob(
    name="messages",
    params={"languages": ["en"]},
    include=lambda ctx: ["locales/%s" % language for language in ctx.languages],
)

task(
    name="build",
    params={
        "languages": ["en"],
        "mode": param(default="release", values=["debug", "release"]),
    },
    base="packages",
    steps=lambda ctx: [
        add("build-files", "/workspace"),
        add(dep("messages", languages=arg("languages")), "/workspace"),
        run(["pnpm", "build", "--mode", "development" if ctx.mode == "debug" else "production"]),
    ],
    ...
)
```

Any attribute of a target but its name and `params` can be a function of the target's configuration, `ctx`,
which reads a parameter as `ctx.<name>`. vamos calls the function for each configuration the target is built
in, and checks what it returns as if it were written in its place. A function gives a whole attribute, not an
item of a list.

`dep()` names a target with arguments for its parameters, wherever a label is accepted; a plain label gives
every parameter its default. `arg("name")`, as an argument of `dep()`, passes on the value of the using target's
own parameter `name`. Arguments reach only the target they're given to, so a target that takes no parameters is
built once however many other targets take:

```python
add(dep("lib", mode="release"), "/lib")           # the release build of "lib", whatever this target's mode
base=dep("tools", verbose=arg("verbose"))          # this target's verbose, passed on
```

`--set` gives the named target's parameters values for one command: `--set mode=debug`, `--set languages=en,es`.
A list's strings are separated by commas, and keep their order. With several targets, `vamos build` gives each
the flags that name its own parameters, and each flag must name a parameter of at least one of them.

Targets that an argument doesn't change aren't built again for it: a build is cached by what it does, so `--set
mode=debug` rebuilds only the targets whose steps or files the mode changes, and those that use them. A target
given arguments other than its defaults is reported with them, as in `task "lib" (mode="debug"): cached`.

### Platforms

Every target is built for a platform, which is the host's unless `--platform linux/arm64` names another. A
platform is written `os/arch` or `os/arch/variant`; in a function, `ctx.platform` is the whole, as a string
such as `"linux/arm64"`, and `ctx.os`, `ctx.arch` and `ctx.variant` its parts, as Go's `GOOS` and `GOARCH` name
them. A target passes its
platform on to the targets it uses, unless `dep()` gives another, such as `EXEC_PLATFORM`, the platform build
steps run on, which is the host's (`ctx.exec_platform`). A target built for another platform than the command's
is reported with it, as in `layer "app" (platform=linux/arm64): building`.

A `pull()` fetches the image for its platform, a layer's image records it, even on `scratch`, and its `run()`
steps run for it. Another machine's programs run under emulation, through the emulators the kernel has
registered, such as Docker Desktop's: they are slower, and if none is registered for the platform, a `run()`
fails with "exec format error". A task whose files are the same for every platform, such as a JavaScript bundle,
or which cross-compiles, can run natively instead, on a base for the exec platform:

```python
task(
    name="build",
    base=dep("golang", platform=EXEC_PLATFORM),
    steps=lambda ctx: [
        add("build-files", "/src"),
        build_env(GOOS=ctx.os, GOARCH=ctx.arch, CGO_ENABLED="0"),
        run(["go", "build", "-o", "/out/app", "."]),
    ],
    ...
)
```

The task is then built once, and shared by every platform's image that adds it, unless, as here, it reads its
platform: then it is built once for each. A layer's base must be for the layer's own platform.

An `index()` lists images for several platforms under one name, as a multi-platform tag does. Each of its
manifests is the pull or layer `layer`, with the same arguments, for one of `platforms`, in order. Its `dep()`
can't give a platform, since `platforms` does. For a different target on each platform, `layer` is a dict from
each platform to its target instead:

```python
index(
    name="server",
    params={"mode": "release"},
    layer=dep("layer", mode=arg("mode")),
    platforms=["linux/amd64", "linux/arm64"],
)

index(name="tools", layer={"linux/amd64": "linux-tools", "windows/amd64": "windows-tools"})
```

`push` sends the whole index, in the order listed. `load` and `write` send one image, so they choose the
manifest for the platform that will run it: the Docker daemon's for `load`, which may be another machine's, and
the host's for `write`. Only that manifest is built. `build` builds them all. `--set` gives the index's own
parameters, while `--platform` can't be given with an index, whose manifests give their own platforms; name one
of them instead.

### Loading .star files

`load()` reads functions and values from a `.star` file, named by its label within the workspace, so that
`BUILD` files can share them:

```python
load("//:defs.star", "node_task", "LANGUAGES")
```

A `.star` file can't declare targets as it is loaded, but its functions can, when a `BUILD` file calls them.
Labels in them are relative to the package of that `BUILD` file, or, in a function of `ctx`, of the target it
gives an attribute of. A function of `ctx` may name targets declared after the one it belongs to, so vamos
reports targets that use each other in a cycle, even in different configurations.

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
next build of your own project runs every target again.

## Run

Buildah needs to create namespaces and mounts, which Docker's defaults don't allow.
Without extra permissions the run fails with
`Error during unshare(CLONE_NEWUSER): Operation not permitted`.

Run with full privileges, loading the image straight into the host's Docker:

```
docker run --rm -it --privileged \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD/examples/typescript-mcp:/example" \
  vamos load server vamos-example -C /example
```

Or grant only what buildah needs:

```
docker run --rm -it \
  --cap-add SYS_ADMIN \
  --security-opt seccomp=unconfined \
  --security-opt apparmor=unconfined \
  --security-opt label=disable \
  -v "$PWD/examples/typescript-mcp:/example" -v "$PWD/output:/output" \
  vamos write server /output/example.tar -t vamos-example -C /example
```

This variant writes a tarball instead. To push, use `vamos push` and mount your credentials,
e.g. `-v ~/.docker/config.json:/root/.docker/config.json:ro`.

On success `push`, `load` and `write` print `Image built! <destination> <image-id>`, or, for an index that
`push` sends, `Index built! <destination> <digest>`, the digest of the index's manifest.

## Run the built image

```
docker run -i --rm vamos-example
```

If you wrote a tarball, `docker load -i output/example.tar` first.

The example is an [MCP](https://modelcontextprotocol.io) server with an `echo` tool, speaking MCP over stdin and
stdout. Send it a request with `docker run -i --rm vamos-example < examples/typescript-mcp/echo-request.jsonl`.
