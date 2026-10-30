# Configuration reference

Use labels to compose packages, parameters to vary a target's behavior,
and platforms to choose what its artifacts run on.

## Workspaces

A package is a directory containing a `BUILD` file, or one named `BUILD.vms`.
A workspace groups packages beneath a directory containing a `WORKSPACE` file, which may be empty.

Name a package's file `BUILD.vms` when the directory also holds a `build` directory, such as a project's build output.
Where file names are compared without case, as on macOS's default filesystem or with git's `core.ignorecase`,
`BUILD` and `build` are the same name: the two can't sit side by side,
and a `.gitignore` entry for `build` silently ignores `BUILD` as well.
A directory named `BUILD` or `build` is never taken for a BUILD file.
A package has one BUILD file: a directory with both `BUILD` and `BUILD.vms` is an error,
rather than one of them being silently ignored.
Everything said here of a `BUILD` file applies to a `BUILD.vms` file, and labels don't change:
`//ui:app` names the target `app` of `ui/BUILD` or `ui/BUILD.vms`.

```text
project/
├── WORKSPACE
├── BUILD
├── backend/
│   └── BUILD
└── ui/
    └── BUILD
```

vamos searches for the nearest `WORKSPACE` in `-C`'s directory and its ancestors.
Without one, `-C`'s directory is the workspace root.
A `WORKSPACE` that can't be checked, such as one in a directory vamos can't search, stops the search with an error,
rather than letting another directory become the root.

| Reference | Meaning |
| --- | --- |
| `app` or `:app` | Target in the current BUILD file's package |
| `//:tools` | Target in the workspace root's BUILD file |
| `//backend:schema` | Target in `backend/BUILD`, or `backend/BUILD.vms`, beneath the workspace root |

Use labels wherever a target reference is accepted, including `base`, `add()`, `dep()`, and index entries:

```python
task(
    name = "generate",
    base = "tools",
    steps = [
        add("//backend:schema", "/workspace"),
        run(["pnpm", "graphql-codegen"]),
    ],
    source = "/workspace/src/generated",
    include = ["**/*.ts"],
)
```

A label's path is a clean path to a directory within the workspace, reached through no symbolic link.
A referenced glob keeps paths relative to its own BUILD directory.
A mistake in a BUILD file that loads because another names one of its targets is reported where it is.
vamos loads only the packages needed by the requested targets and reports dependency cycles.

Packages may name each other's targets, as where their files sit is often for other tools to say.
A package's `BUILD` file runs when another first names one of its targets, while that other file is still running;
it can then name the other file's targets declared above that point, as a file names its own earlier targets:

```python
# BUILD
layer(name = "jre", base = "//:java")         # declared before scala/BUILD runs
layer(name = "app", base = "//scala:server")  # runs scala/BUILD, which names //:jre
```

```python
# scala/BUILD
layer(name = "server", base = "//:jre", steps = [add("jar", "/app")])
```

A target declared below that point isn't declared yet when the other file names it,
which is an error saying where the first file stopped; declare the target above that line.
A reference made by a function of `ctx`, such as `base = lambda ctx: "//:later"`, is resolved
once every BUILD file has run, so it can name any target.
Targets that name each other in a cycle, such as `//:app -> //scala:server -> //:app`, are an error.
The workspace root does not need a `BUILD` file unless a reference or command needs targets from it.

On the command line, unqualified names refer to `-C`'s package:

```sh
vamos build //backend:test //ui:lint -C project
vamos load app ui:dev -C project/ui
```

## Parameters

Targets other than pulls accept a `params` dictionary.
A plain value declares both a default and a type:

```python
params = {
    "mode": "release",
    "verbose": False,
    "workers": 4,
    "languages": ["en"],
}
```

Supported types are strings, booleans, integers, and lists of strings.
Use `param()` to constrain values or require an argument:

```python
params = {
    "mode": param(default = "release", values = ["debug", "release"]),
    "languages": param(default = ["en"], values = ["en", "es", "fr"]),
    "version": param(type = "string"),
}
```

`param(default = ..., values = ..., type = ...)` accepts these optional arguments:

| Argument | Meaning |
| --- | --- |
| `default` | Value used when no argument is given; determines the type |
| `values` | Nonempty list of allowed strings, for a string or list parameter |
| `type` | `"string"`, `"bool"`, `"int"`, or `"list"`; required when there is no default |

Provide a default or a type.
If both are given, they must agree.
A parameter without a default must be supplied on every use.
For list parameters, `values` constrains each item.

### Functions of `ctx`

An attribute can be a function that receives the target's configuration:

```python
glob(
    name = "translations",
    params = {"languages": ["en"]},
    include = lambda ctx: ["locales/%s" % language for language in ctx.languages],
)
```

Read user parameters as `ctx.<name>`, so a parameter's name is an identifier.
The function returns the whole attribute, such as a complete `steps` list or `include` list.
Individual list entries cannot be functions of `ctx`.
Such functions configure existing targets; they cannot declare new targets.
The target's `name` and `params` cannot be functions of `ctx`.
A mistake that a function makes names the configuration it was given,
such as `(configuring //:app with mode="debug")`.

### `dep()` and `arg()`

`dep(label, platform = ..., **arguments)` names a target with explicit parameter arguments.
It is accepted wherever a plain target label is accepted.
Omit `platform` to inherit the using target's platform.

```python
add(dep("translations", languages = ["en", "es"]), "/app")
base = dep("tools", platform = EXEC_PLATFORM)
```

`arg("name")` forwards the using target's parameter value as an argument of `dep()`:

```python
layer(
    name = "app",
    params = {"mode": "release"},
    base = "scratch",
    steps = [add(dep("compile", mode = arg("mode")), "/")],
    command = ["/app"],
)
```

The referenced target must declare the argument's parameter.
Arguments are not automatically propagated beyond that target.
A plain label uses all the referenced target's defaults.

### CLI values

Repeat `--set name=value` for each parameter to override:

```sh
vamos build compile --set mode=debug
vamos build bundle --set languages=en,es --set verbose=true --set workers=8
```

Lists use comma-separated strings and preserve order.
An empty value, such as `--set languages=`, gives an empty list.
Boolean values must be lowercase `true` or `false`.
An integer parameter's value must be an integer, and that of a parameter with `values` must be one of them,
or for a list, each of its items.
Give each parameter once.
With several targets, `build` applies each flag to every named target declaring that parameter.
A flag that matches none of the named targets is an error.

Parameters affect reuse through the configured inputs and operations.
Changing an unused parameter does not by itself force a target to run again.

## Platforms

A platform is `os/arch` or `os/arch/variant`, for example `linux/arm64` or `linux/arm/v7`.
Targets normally inherit the requested platform through their dependencies.
The command defaults to the host's platform unless `--platform` is given.

```sh
vamos build app --platform linux/arm64
```

Functions of `ctx` can read these built-in fields:

| Field | Meaning | Example |
| --- | --- | --- |
| `ctx.platform` | Full target platform | `"linux/arm64"` |
| `ctx.os` | Target operating system | `"linux"` |
| `ctx.arch` | Target architecture | `"arm64"` |
| `ctx.variant` | Architecture variant, or an empty string | `"v7"` |
| `ctx.exec_platform` | Platform executing build steps natively | `"linux/amd64"` |

These field names are reserved and cannot be user parameters.
Globs are shared across platforms and cannot read `ctx.platform`, `ctx.os`, `ctx.arch`, or `ctx.variant`.

### Native tools and cross-compilation

`EXEC_PLATFORM` asks `dep()` for the executor's platform, rather than the target's platform:

```python
task(
    name = "compile",
    base = dep("golang", platform = EXEC_PLATFORM),
    steps = lambda ctx: [
        add("source", "/src"),
        workdir("/src"),
        build_env(CGO_ENABLED = "0", GOOS = ctx.os, GOARCH = ctx.arch),
        run(["go", "build", "-o", "/out/app", "."]),
    ],
    source = "/out",
    include = ["app"],
)
```

This runs native compiler tools while producing a binary for the requested platform.
A task using native tools can share one result across platforms when its configured work is identical,
as for a platform-independent JavaScript bundle.
Reading the target platform here changes the compiler environment, producing a separate result for each platform.
A layer's base must match the layer's own platform; use a task to separate native tools from target output.

The execution platform is chosen among those the executor offers, before the BUILD files are read:
the one `--exec-platform` names if it is given, otherwise the one a coordinator's `--default-exec-platform` names,
otherwise this machine's platform if the executor offers it, and otherwise the only one it offers.
Since it configures `EXEC_PLATFORM`, it is part of those targets' cache keys:
a coordinator with `--default-exec-platform` gives clients on laptops and CI runners of other architectures
the same keys, so they reuse each other's results.
The executor built into vamos offers this machine's platform alone; a [coordinator](cli.md#coordinator) offers the
platforms of its workers, so a client on an Apple-silicon laptop building against an amd64 pool runs its commands
on `linux/amd64`, while `--platform` still says what to build and defaults to the client's own platform.
A coordinator runs each layer's and task's commands on a worker of its image's platform
when the deployment offers that platform, and on the execution platform otherwise,
so an index for `linux/amd64` and `linux/arm64` builds natively on a pool with workers of both.
The execution platform runs what `EXEC_PLATFORM` names, and tasks on `scratch`.
Where a target runs is no part of its cache key, so a result built on either platform's worker is reused.

If a `run()` executes another architecture's programs, the kernel needs a registered emulator for that architecture.
Otherwise the command can fail with `exec format error`.
Against a coordinator, such a target is refused before anything runs, with a message naming both platforms:
give `--platform` to build for the platform that runs the commands, or `--emulate` to let them run under emulation
on a worker that has an emulator for the image's platform.
With `--emulate`, a platform that no worker of the deployment emulates fails the target at once, with a message that
says so; one the deployment offers emulation of waits for such a worker.

### Index delivery

An [index](build.md#index) supplies its own platform list, so it cannot be named with `--platform`.

| Command naming an index | Images built and delivered |
| --- | --- |
| `build` | Build all images into the cache |
| `push` | Build and publish all images, preserving index order |
| `load` | Build and load only the Docker daemon's platform |
| `save` | Build and save only the host's platform |

Selecting a platform missing from the index is an error.
To select an arbitrary platform for delivery, name its pull or layer directly and use `--platform`.

## Shared `.star` files

Use Starlark `load()` to share functions and constants:

```python
load("//:defs.star", "go_checks")

go_checks(name = "test", base = "tools", source = "source")
```

For example, `defs.star` can contain:

```python
def go_checks(name, base, source):
    task(
        name = name,
        base = base,
        steps = [
            add(source, "/workspace"),
            workdir("/workspace"),
            run(["go", "test", "./..."]),
        ],
    )
```

Module labels start with `//` and name a `.star` file in the workspace.
A mistake in a module is reported where it is in the `.star` file, and a module that loads itself,
through the modules it loads, is an error.
A module cannot declare targets at its top level while loading, nor name a target by a label that doesn't start
with `//`; its functions can declare targets when called by a BUILD file.
The arguments that a module's `dep()` gives are checked where a BUILD file uses it.
References inside those functions resolve relative to the calling BUILD file's package.
References in functions of `ctx` resolve relative to the target being configured.
