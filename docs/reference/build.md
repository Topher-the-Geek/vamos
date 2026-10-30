# BUILD language reference

A `BUILD` file is a Starlark program that declares named targets.
It may be named `BUILD.vms` instead, where a `build` directory beside it would clash with `BUILD`;
see [workspaces](configuration.md#workspaces).
Use lists, dictionaries, functions, and comprehensions to compose declarations.
Commands run inside build containers through `run()` steps.

A mistake in how a `BUILD` file declares its targets, such as a misspelled argument or a value of the wrong type,
fails the command as the file loads, before anything runs,
with an error that names the file, line and column of the call that made it.
No string given to a target or step, such as a name, an argument, a path, an owner or a variable,
may hold a NUL byte, which can't reach a container.

## Target names and references

Every target has a unique, nonempty `name` within its `BUILD` file.
Names cannot contain `:` or `/`, and `scratch` is reserved for the empty image.
Declarations register targets; they do not return artifact objects.
Refer to a target by its name or a [label](configuration.md#workspaces), optionally wrapped in `dep()`.

Declare local dependencies before the targets that use them.
References produced by a function of `ctx` can name later targets; dependency cycles are errors.

| Target | Produces | Can be a `base` | Can be an `add()` source |
| --- | --- | --- | --- |
| `pull()` | Image | Yes | No |
| `glob()` | Fileset | No | Yes |
| `task()` with output | Fileset | No | Yes |
| `task()` without output | Successful check | No | No |
| `layer()` | Image | Yes | No |
| `index()` | Image index | No | No |

All target kinds except `pull()` accept `params`, a dictionary of parameter declarations.
Target attributes other than `name` and `params` can be functions of `ctx`.
See [configuration](configuration.md) for `param()`, `dep()`, `arg()`, and platform configuration.

## `pull()`

```python
pull(name = "node", image = "docker.io/library/node", tag = "24-alpine")
```

| Argument | Meaning | Default |
| --- | --- | --- |
| `name` | Target name | Required |
| `image` | Repository name without a tag or digest | Required |
| `tag` | Repository tag | `"latest"` |

A pull fetches the image for its configured platform.
If the tag names an index, it selects the matching image.
A single-platform image must match the configured platform; a mismatch is an error.
Use fully qualified repository names to make the source explicit.
An image's tag belongs in `tag`, rather than inside `image`.
Pull targets have no user parameters; only their platform configures them.
Previously resolved tags are reused until explicitly [rebuilt](cache.md#refreshing-results).
A pull whose image its registry doesn't have, or won't give, fails, suggesting to check the name,
and to log in if the image is private; one whose registry can't be reached fails, saying why.

## `glob()`

```python
glob(
    name = "source",
    include = ["src/**/*.ts", "package.json"],
    exclude = ["**/*.test.ts"],
)
```

A glob selects files relative to the directory containing its `BUILD` file.
`name` and a nonempty `include` list are required.
Other selection arguments and their defaults are listed under [file selection](#file-selection).
Globs provide input filesets to `add()`; they cannot be named directly by `vamos build` or `vamos export`.

## `task()`

```python
task(
    name = "compile",
    base = "tools",
    steps = [
        add("source", "/workspace"),
        workdir("/workspace"),
        run(["npm", "run", "build"]),
    ],
    source = "/workspace/dist",
    include = ["**/*.js"],
)
```

| Argument | Meaning | Default |
| --- | --- | --- |
| `name` | Target name | Required |
| `base` | A pull or layer reference, or `"scratch"` | Required |
| `steps` | Steps applied in order | `[]` |
| `source` | Absolute container directory to collect output from | Omitted: no output |
| `include` | Nonempty list of output patterns relative to `source` | Required with `source` |
| Selection options | Exclusions, modes, and limits | See [file selection](#file-selection) |

Provide `source` and `include` together, or omit both for a task that only runs checks.
Selection options require output to be declared.
Output files keep their paths relative to `source`.
For example, `source = "/workspace/dist"` and `include = ["js/main.js"]` produce the entry `js/main.js`.
Only selected output survives the task's temporary container.

A task inherits its base's working directory and environment.
Its base may use another platform, such as `dep("tools", platform = EXEC_PLATFORM)` for native cross-compilation.
See [platforms](configuration.md#platforms).

## `layer()`

```python
layer(
    name = "app",
    base = "runtime",
    steps = [add("compile", "/app"), workdir("/app")],
    command = ["node", "main.js"],
)
```

| Argument | Meaning | Default |
| --- | --- | --- |
| `name` | Target name | Required |
| `base` | A pull or layer reference, or `"scratch"` | Required |
| `steps` | Steps applied in order | `[]` |
| `command` | Image's default command as an argument list | Inherited from the base |
| `entrypoint` | Image's entrypoint as an argument list | Inherited from the base |

A layer keeps the base's files and runtime configuration and applies its own changes.
`command` sets the image's command; it does not run while building.
It follows the entrypoint, if one exists, as Docker's `CMD` does.
An omitted or empty `command` leaves the inherited command in place.

`entrypoint` sets the program the image runs, as Docker's `ENTRYPOINT` does:
`docker run` arguments are appended to it rather than replacing it.

```python
layer(
    name = "server",
    base = "jre",
    steps = [add("jar", "/app")],
    entrypoint = ["java", "-jar", "/app/server.jar"],
)
```

`docker run server --port 9` then runs `java -jar /app/server.jar --port 9`.
As in a Dockerfile, giving `entrypoint` drops the inherited command, which would otherwise become its arguments;
give `command` as well for default arguments.
`entrypoint = []` removes the inherited entrypoint, and an omitted `entrypoint` keeps it.
A layer's base must have the same platform as the layer.

## `index()`

```python
index(
    name = "release",
    layer = "app",
    platforms = ["linux/amd64", "linux/arm64"],
)
```

`name` and `layer` are required.
With a single pull or layer reference, `platforms` must be a nonempty list.
vamos configures that target once for each listed platform and preserves the list's order in the index.

To use a different target for each platform, give `layer` as a nonempty dictionary and omit `platforms`:

```python
index(
    name = "release",
    layer = {
        "linux/amd64": "app-amd64",
        "linux/arm64": "app-arm64",
    },
)
```

Each platform must be unique.
A `dep()` in an index may pass user parameters but cannot specify `platform`;
the list or dictionary already determines each image's platform.
An index cannot contain another index.

## Steps

Put steps in the `steps` list of a task or layer; a step made anywhere else is an error.
They execute in order, and later steps see earlier changes.

### `add(source, dest, owner = "", group = "")`

Copy a glob's or task's fileset beneath an absolute container directory.
`source` is a target reference; `dest` is a path, which can contain environment variable references.
Paths within the fileset are preserved:

```python
# If "source" contains src/main.ts, this writes /app/src/main.ts.
add("source", "/app")
add("compile", "/app", owner = "node", group = "staff")
```

Later additions replace earlier files at the same paths.
Files are owned by root unless `owner` or `group` overrides ownership.
Names refer to users and groups in the image.
Both arguments require strings, including numeric IDs such as `owner = "1000"`,
and neither holds `:`, as `owner = "user:group"` would; give the group as `group`.
With only `owner`, the group is that user's group; with only `group`, the owner remains root.
New parent directories receive the selected ownership too.
Directories the image already has keep their modes and owners,
unless the fileset selects them itself, as `include = ["."]` selects every directory beneath it:
a selected directory brings its own masked mode and the selected ownership,
so a project directory added over `/tmp` replaces its mode, sticky bit and all.
Create a destination first with `workdir()` when it should keep root ownership.

### `run(argv)`

Run a nonempty list of command arguments in the container's current working directory.
The command must exit successfully.
No shell is implicit:

```python
run(["npm", "ci"])
run(["sh", "-c", "mkdir -p /out && printf 'hello\\n' > /out/message.txt"])
```

Use a shell explicitly for redirection, pipes, or shell variable expansion.
The base image must contain every command you run, including the shell.

A command runs with chroot isolation, in the network of the process that runs vamos:
the same interfaces and addresses, and the same DNS servers, from its `/etc/resolv.conf`.
In a devcontainer, a step reaches what the devcontainer reaches,
such as the services of a Docker Compose network that the devcontainer is on, by their names;
on a [coordinator's](cli.md#coordinator) workers, it reaches what the worker's container reaches.
The step gets a hostname of its own and an `/etc/hosts` that names it and `host.containers.internal`,
neither of which stays in the image.
Unlike podman's, it gets none of the host's `mounts.conf` mounts, such as subscription secrets at `/run/secrets`,
though on a host in FIPS mode buildah still mounts the host's FIPS settings for bases that use them.
Network access is not an input of a target's [cache key](cache.md#what-changes-a-result):
a step that downloads, or talks to a service, is reused like any other until its inputs change.

### Secrets

A step that needs a credential, such as a token to upload source maps, is given it as a secret:

```python
run(
    ["sh", "-c", "sentry-cli sourcemaps upload dist"],
    secrets = [secret("sentry", env = "SENTRY_AUTH_TOKEN")],
)
run(["sh", "-c", "npm ci"], secrets = [secret("npmrc", file = "/root/.npmrc")])
```

`secret(id, env = ...)` sets the variable `env` for that step alone,
and `secret(id, file = ...)` mounts a read-only file, owned by root, at that absolute path for that step alone.
Each secret gives exactly one of them, and no two secrets of a step give the same variable or file.
An ID is letters, digits, `_`, `.` or `-`, starting with a letter or digit, and a file is a clean absolute path.
The value comes from the command line, `--secret id=sentry,env=SENTRY_AUTH_TOKEN` or `--secret id=npmrc,src=.npmrc`;
see [the CLI reference](cli.md#secrets).
A build that reaches a step whose secret the command line doesn't give fails when the BUILD file loads,
before anything runs.

A secret's value never enters an image, the build cache, a trace or the graph.
The step's cache key covers each secret's ID and the variable or file that the step names for it,
but not its value, nor where `--secret` reads it,
so a new value doesn't run the step again, and a cached step's replayed output is from the run that made it.
A step that must run again, such as an upload for each release, should depend on what identifies the release,
such as a parameter, `build_env(VERSION = ctx.version)`.
A step can still print its secret, and its output is kept with its result; don't echo secrets.

### `env(**variables)` and `build_env(**variables)`

```python
env(NODE_ENV = "production", PATH = "/app/bin:$PATH")
build_env(npm_config_cache = "/tmp/npm")
env(**{"NAME-WITH-DASHES": "value"})
```

Both set string-valued variables for later steps in the target.
`env()` also records them in the resulting image and makes them available to targets using that image.
`build_env()` applies only within the current target and does not persist its assignments in the image.
Each call needs at least one variable, given by a nonempty name that holds no `=`, once in a call.
Names such as `NAME-WITH-DASHES` can be set, but cannot be referenced by vamos's variable expansion.

Values expand `$NAME` and `${NAME}` using the environment before that call.
Variable references require an ASCII letter or underscore followed by ASCII letters, digits, or underscores.
Unset variables expand to an empty string; `$$` produces a literal dollar sign.
Any other `$` syntax, including `${NAME:-default}`, `$5`, or a trailing `$`, is an error when the BUILD file loads.
For runtime or shell expansion, put the expression inside a command passed to `run(["sh", "-c", ...])`.

### `workdir(path)`

Create an absolute directory and use it for later commands and the resulting image's working directory.
The path can refer to environment variables, for example `workdir("$HOME/app")`.
Both `workdir()` paths and `add()` destinations must be absolute after expansion.

## File selection

`glob()` and output-producing `task()` targets share these arguments:

| Argument | Meaning | Default |
| --- | --- | --- |
| `include` | Nonempty list of paths or patterns to select | Required |
| `exclude` | List of paths or patterns to omit | `[]` |
| `mode_mask` | Permission bits to retain for files and directories | `0o755` |
| `allow_no_files` | Allow a selection with no files or links | `False` |
| `max_file_count` | Maximum number of selected files and links | `10000` |
| `max_file_size` | Maximum bytes in one file | `100 * 1024 * 1024` |
| `max_total_size` | Maximum total file bytes | `1024 * 1024 * 1024` |

Limits must be positive; raise them explicitly when a larger selection is intentional.
`mode_mask` must be between `0` and `0o7777`.
It masks existing permission bits rather than assigning a fixed mode.

### Patterns and paths

Patterns are relative to the BUILD directory for a glob, or to `source` for task output.
Use `/` as the separator.
Patterns cannot be empty or resolve outside the selection root after normalizing `.` and `..` components.
Use `.` to select the whole directory.

| Pattern | Selects |
| --- | --- |
| `main.go` | That path |
| `*.go` | Go files directly beneath the selection root |
| `src/**/*.go` | Go files at any depth beneath `src`, including directly inside it |
| `src` | The directory's contents recursively |
| `.` | The selection root's contents recursively |
| `**/node_modules` in `exclude` | Entire matching directories and their contents |

`*` matches within a single path component, including names beginning with `.`.
`?` matches one character and `[a-z]` matches a character range.
`**` matches zero or more directories and must be a whole path component:
use `src/**/*.go`, rather than `src/**.go`.
An exclusion matching a directory prevents traversal into it.
Empty directories are omitted; selected files bring the needed directory entries and their modes.
Leading directories used to reach a pattern's matches are not themselves selected entries.

Paths stay relative to the selection root, even when the pattern has a leading directory.
Selecting `src/main.go` does not strip `src/` when adding or exporting it.

### Reading files and links

Only directories that a pattern could match within are read.
In each include, every path component before the first wildcard must exist and must not be a symbolic link,
even when `allow_no_files = True`.
For an include without wildcards, this applies to the entire path, including a literal file such as `main.go`.
For example, `include = ["link"]` rejects a symbolic link.
Links matched by wildcards or found within an included directory are copied as links and never followed.
A task's `source` may itself traverse links, resolved within its container.

vamos captures reachable globs once before execution, so their input files remain consistent throughout the build.
Globs that no requested target uses are not read.
Task outputs are selected after the task's steps finish.
