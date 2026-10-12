# AGENTS.md

Guidance for coding agents working on vamos. [README.md](README.md) covers what the tool does and how to use it.

## What this is

`vamos` is a Go CLI that builds container images from Starlark `BUILD` files using the buildah library
(no Dockerfile, no buildah or docker binary at build time). Subcommands send a target's image to a registry
(`push`), the Docker daemon (`load`), or a tarball (`write`). A workspace (a directory with a `WORKSPACE`
file) holds a `BUILD` file per package, which name each other's targets by label, `//dir:name`.

## Layout

- `main.go` — only `main()`: buildah re-exec setup, then `internal.NewRootCommand(afero.NewOsFs())`.
  Keep it that way; all other code goes in `internal/`.
- `internal/cli.go` — Cobra commands, argument checks, destinations (`docker://`, `docker-daemon:`,
  `docker-archive:`).
- `internal/buildfile.go` — finds the workspace, runs each package's `BUILD` with starlark-go as labels
  name it, and `.star` files as `load()` names them, and validates its `glob()`, `pull()`, `layer()` and
  `task()` targets, and the `add()` steps that name other targets.
- `internal/config.go` — targets' parameters (`params`, `param()`), `dep()` and `arg()`, each target's
  configurations (its platform and arguments) and `ctx`, and the configuration phase: the graph of each target the
  named targets need, in each configuration it's needed in.
- `internal/glob.go` — the selection of a glob's or task's files: include and exclude patterns, mode mask and
  limits, and the walk that reads only the folders they could match into a tree for the cache.
- `internal/step.go` — the other steps a layer or task applies in order (`run()`, `env()`,
  `build_env()`, `workdir()`): each is a Starlark value that knows its part of the cache key and how to apply
  itself with buildah. Add new kinds here.
- `internal/image.go` — pulls, then runs each layer and task the named targets need with buildah, or reuses
  its cached result, and copies the named target's image to the destination.
- `internal/pull.go` — resolves each `pull()`'s tag once, recording what it named in the build cache, and pulls
  the image for each platform a build needs by digest.
- `internal/cache.go` — the build cache: a content-addressed store of files, trees and logs, and an action
  cache from each target's key to its latest result. Layers' images stay in the container store.
- `examples/` — sample `BUILD` projects: `typescript-mcp/` (TypeScript) and `fastmcp/` (Python), each an MCP server
  with an echo tool; `strawberry-react/`, a workspace whose UI package uses the backend's GraphQL schema, built
  as indexes for two platforms, with parameters for the mode and languages; and `golang-cli/`, a Go command
  cross-compiled for two platforms. Each has its own lint and tests; see its README.

## Environment

Work inside the devcontainer (`.devcontainer/`). Building images needs root, a privileged container,
`BUILDAH_ISOLATION=chroot` (no OCI runtime is installed), and `/var/lib/containers` on a volume. The
Docker socket is mounted, so `docker` works too.

## Commands

```
go build -o vamos .           # binary is git-ignored
go vet ./...
gofmt -l .                    # must print nothing
go test -short ./...          # fast: command line and BUILD checks, no builds (<1s)
go test ./...                 # everything, including image builds (~90s, needs root + network)
```

After changing an example, run its own checks (setup is in its README):

```
(cd examples/typescript-mcp && npm run check)
(cd examples/fastmcp && . .venv/bin/activate && ruff check . && ruff format --check . && pyright && pytest)
```

Run the full suite before finishing a change that touches building, committing, or destinations.
The first run pulls `busybox:1.36` from Docker Hub; later runs use the local container store.

## Tests

- Tests are sociable: they drive the real command line through `vamos(t, fsys, args...)` in
  `internal/helpers_test.go` and use real buildah. Don't mock the core.
- Complex units with many cases, such as pattern matching and the walk in `glob_test.go`, or the cache's trees,
  may also be tested on their own, on an `afero.MemMapFs`, or `diskProject` for symbolic links. That isn't
  mocking: it is the real code on a filesystem. A build test still checks that their results reach the image.
- Projects live in memory: `project(t, map[string]string{...})` returns an `afero.MemMapFs` rooted
  at `/project`. Tests that need symbolic links (`MemMapFs` has none) or write into the project use
  `diskProject`, which keeps them under a temporary directory. Don't add `testdata/` directories.
- Boundaries are verified, not assumed: `push` goes to an in-process registry
  (go-containerregistry), `load` to a fake Docker daemon on a Unix socket (and a real one when
  available), and every result is inspected (command, files, tags).
- Use `.succeeds(t)` and `.failsWith(t, usage, wants...)`. `usage` is true only for command-line
  mistakes; runtime errors must not print usage.
- Failing builds must leave no output behind; check with `assertNoFiles`. The cached results of targets that
  succeeded before the failure are kept, so the next build resumes from there.
- Test BUILD files start with `pullBusybox`, which declares the target `busybox`. Commands that send an
  image name the target to send, usually `app`.
- Workspace tests put a `WORKSPACE` file in the project and a `BUILD` file in each package's directory.
- Tests that build call `requireBuildah(t)` so `-short` and non-root runs skip them.
- Write table cases as keyed struct literals, one field per line.
- `TestMain` must call `buildah.InitReexec()` first, because buildah re-executes the test binary to
  run build steps.


## Sanity checks

The tests never run the built binary, the examples, or the `Dockerfile`. Before finishing a change to
building, destinations, the examples, or the `Dockerfile`, run these by hand. Each sends
`examples/typescript-mcp/echo-request.jsonl` over stdin and should end with its reply:
`{"result":{"content":[{"type":"text","text":"hola"}]},"jsonrpc":"2.0","id":2}`.

```
# 1. Command line: help works; mistakes print `vamos: ...` and exit 1
go build -o vamos . && ./vamos --help && ./vamos nope; ./vamos load

# 2. load into Docker, then run
./vamos load server vamos-example -C examples/typescript-mcp \
  && docker run -i --rm vamos-example < examples/typescript-mcp/echo-request.jsonl

# 3. write a tarball, then load and run it (use a temp dir, not the repo)
./vamos write server "$TMP/example.tar" -t vamos-example:tar -C examples/typescript-mcp \
  && docker load -i "$TMP/example.tar" && docker run -i --rm vamos-example:tar \
  < examples/typescript-mcp/echo-request.jsonl

# 4. The release image (README's "Run"). Paths in the devcontainer aren't host paths, so pipe
#    the example in instead of bind-mounting it.
docker build -q . -t vamos \
  && tar -C examples/typescript-mcp --exclude=node_modules -c . | docker run -i --rm --privileged \
  -v /var/run/docker.sock:/var/run/docker.sock --entrypoint sh vamos \
  -c 'mkdir /example && tar -x -C /example && vamos load server vamos-example:image -C /example' \
  && docker run -i --rm vamos-example:image < examples/typescript-mcp/echo-request.jsonl

# 5. The fastmcp example. Its SDK cancels pending requests when stdin closes, so hold stdin open long enough
#    for it to start.
./vamos load server vamos-fastmcp -C examples/fastmcp \
  && (cat examples/typescript-mcp/echo-request.jsonl; sleep 6) | docker run -i --rm vamos-fastmcp

# 6. The strawberry-react workspace, behind its compose file's ingress. The devcontainer can't reach the
#    host's port 8080, so curl from the compose network. Expect the page's <title>, then the reply below.
S=examples/strawberry-react
./vamos load //backend:layer strawberry-backend -C $S && ./vamos load //ui:layer strawberry-ui -C $S \
  && docker compose -f $S/docker-compose.yaml up -d --wait \
  && curl="docker run --rm --network strawberry-graphql_default curlimages/curl -s" \
  && $curl http://ingress/ | grep '<title>' \
  && $curl http://ingress/graphql -H 'content-type: application/json' \
  -d '{"query":"mutation { increment { value } }"}'; \
  docker compose -f $S/docker-compose.yaml down
```

Check 6's reply is `{"data":{"increment":{"value":1}}}`.

```
# 7. The golang-cli example: an index, cross-compiled; load sends the daemon's platform's image. Expect "hola",
#    from each platform's image.
./vamos load echo vamos-echo -C examples/golang-cli && docker run --rm vamos-echo \
  && ./vamos write //:layer "$TMP/echo.tar" -t vamos-echo:other --platform linux/amd64 -C examples/golang-cli \
  && docker load -i "$TMP/echo.tar" && docker run --rm --platform linux/amd64 vamos-echo:other
```

`push` has no manual check here: the tests already push to a real (in-process) registry.

## Error messages

Errors are a feature here, and every user mistake should have a test for its message.

- Say what was wrong and where: `BUILD` errors are one line, `file:line:col: build: ...`
  (see `describeEvalError`); destinations are named, e.g. `pushing to app:latest: ...`.
- Catch mistakes before building when it's cheap to (bad names, missing sources, unwritable output).
  Don't add extra passes over the project just to report rarer problems, such as unreadable files, sooner:
  they are reported when the target that reads them runs, and the targets before it are cached by then.
- `main` prints `vamos: <error>`; Cobra's own error printing is silenced, so the root command
  reports unknown commands itself.
- Known noise: buildah's chroot helper writes lines such as `subprocess exited with status 1`
  directly to stderr when a `run()` step fails. vamos can't suppress them.

## Code style

- gofmt (tabs), and lines of at most 120 characters, counting a tab as 4 columns. Break up dense lines
  even when they fit.
- Match the surrounding comment density: a doc comment on each function, and short "why"
  comments.
- Output of `run()` steps goes to the command's stdout and stderr (`cmd.OutOrStdout()` and
  `cmd.ErrOrStderr()`), never directly to `os.Stdout`.
- Read `BUILD` files and sources through the `afero.Fs`, never `os`. Output tarballs and buildah's
  storage use the real filesystem.

## Gotchas

- A package's `BUILD` file is loaded when a label first names one of its targets, even partway through running
  another `BUILD` file, or while configuring. An error inside the loaded file is a `loadError`, reported as it is
  rather than as an error of the call that named its target.
- A `declaration` holds a *template* of its target, made and checked as the `BUILD` file runs, so its errors
  say where. Only if an attribute is a function of `ctx` is the template made later, once per configuration
  (`workspace.template`). `params` is never a function, so a target's parameters are known as it is declared, and
  `dep()` checks its arguments then. Configuring turns templates into *instances* (`targetSpec`, `pullSpec`,
  `fileSet`), one per target and configuration (`confLabel`), which the build runs in the order
  `workspace.configure` returns. A configuration belongs to one target: its platform and a value for each of its
  parameters, and `workspace.edge` works out each used target's from the using one's. Labels outside functions
  name earlier targets, but those in functions needn't, so `configurer` reports cycles.
- `arg()` runs before the call declaring the target that uses it, so it records its position, and the declarer
  (or `template`, for what functions give) checks its name (`checkArgNames`), reporting it there with `posError`.
  Its value's type is only checked by `edge`, as the using target is configured.
- Builtins are made once per workspace and read what's calling them from the thread's `evalContext`: a `BUILD`
  file running, a function of `ctx` (which can't declare targets), or a `.star` file loading. A function from a
  `.star` file thus resolves labels against the `BUILD` file or target it runs for.
- Checks that touch the filesystem, such as that a glob's patterns' leading folders exist, run only for the
  targets a command needs (`checkSources`). There are no workspace-wide checks. Whether patterns match any file,
  and the limits, are checked by the walk, as the files are read.
- `addSource` adds only a tree's outermost entries, grouped by folder, so folders above them keep the image's
  own mode and owner, and with `FollowSymlink` off, since buildah would otherwise copy what a top-level link
  points to.
- `docker-archive` refuses to overwrite a non-empty file. `write` builds to a temp file beside the
  target, created only just before copying, and renames it into place.
- `docker-archive` rejects paths containing `:`, so `archiveReference` reaches such a file through
  `/proc/self/fd`. Build references with the transport packages, not `alltransports.ParseImageName`.
- `load` doesn't use the `docker-daemon` transport, which always uses TLS for `tcp://` hosts and
  ignores `DOCKER_CERT_PATH`. It writes a tarball and sends it with the moby client,
  using the Docker connection settings from the environment.
- containers/image caches `registries.conf` regardless of `CONTAINERS_REGISTRIES_CONF`; tests that
  set it must call `sysregistriesv2.InvalidateCache`.
- The files each `add()` copies are stored in the build cache as its target's key is worked out, and the target
  is built from that copy, never from the project, so a file edited mid-build can't be cached under the wrong key
  (`TestCacheSourceEditedDuringBuild`). Keep it that way: it is also what a remote builder will need.
- buildah reads sources from disk, so `addSource` checks an `add()`'s files out of the build cache into a
  temporary build folder first, at their paths relative to the `BUILD` file, then adds them to its `dest`.
  `TestDestinationModes` checks the folder's own mode doesn't leak.
- `collectOutput` reads a task's files from its mounted container on the host, where an absolute link
  would point at the host's own files. So it resolves `source` with `securejoin` and refuses links
  along included paths; `TestTaskOutputLinks` and `TestTaskFailures` cover this.
- Only `pull()` reaches registries: layers use `PullNever` on image IDs (or `scratch`). A pull resolves its tag
  once and records the manifest it names, and each platform's image in it, in the build cache (`tagRecord`, kept by
  `prune`), so later builds don't reach the registry until `--rebuild`. Pulled images are named by digest in the
  store, since a tag can name only one image there, and every platform's would need it. Find a pulled image with
  `ImagesByDigest` before calling `buildah.Pull`, which asks the registry even when the store has the image:
  Docker Hub allows 100 anonymous pulls an hour, and then stalls requests, so a test run would hang
  (`TestPullTag` counts requests). Every layer is
  cached: committed to local storage and named `localhost/vamos-cache:<image ID>`, so that tools removing
  unnamed images leave it alone. The named target's is then sent with `copy.Image`, under the system's signature
  policy, not `buildah.Push`, which re-parses the destination as a string. A target that fails leaves no
  unnamed image; tests check this with `assertNoNewImages`.
- The build cache (`internal/cache.go`) sits beside the container store, in `/var/lib/containers/vamos`. Tests
  share it, and a reused target prints `cached` rather than `building`, so tests about caching put a value unique
  to the run (`newNonce`) in their targets. `TestPrune` and `TestPullTag` empty the cache, so the run after a full
  test run builds every target again.
- Layers are committed with `HistoryTimestamp`, which sets every file a layer adds to the target's epoch: the
  latest of its base's and its added tasks' epochs (a pulled image's is its creation time) and `--timestamp`.
  `RewriteTimestamp` only moves times earlier, which would leave build times and source files' own times in
  layers, so the same inputs could make different layers.
- A target's key covers each added task's epoch as well as its files (`actionStep.Epoch`), since trees hold no
  times and the same files can come with a later epoch (`TestTaskEpochInKey`). The base's epoch needs no field,
  as its image ID fixes it. `--timestamp` is deliberately not an input: targets reused keep their time.
- A configuration's `platform` is its own field, which every edge passes on unless `dep()` gives another. A layer's
  action includes it, since
  a layer on `scratch` has no base to tell platforms apart; a task's doesn't, since only its base and steps
  matter. A layer's base must be for its platform (`setPlatform`). Pass `forPlatform` contexts to buildah, or it
  warns about images for other platforms than the host's. Emulated steps run through the kernel's binfmt_misc
  handlers, which Docker Desktop registers for amd64 (Rosetta) and qemu's architectures; buildah prints "exec format
  error" itself, so vamos can only hint at it (`emulatorHint`).
- An `index()` is only ever named on the command line: `roots` in `cli.go` turns it into its manifests, or, for
  `load` and `write`, which take one image, the one for the destination's platform, before configuring, so that
  the others aren't built. `sendIndex` pushes it through a temporary OCI layout, since containers/image copies an
  index, with `CopyAllImages`, only from a source that holds one.
- `--tls-verify=false` lets a registry use plain HTTP, both for pushing and for pulling the base image.
  Without the flag, `registries.conf` decides, so leave `DockerInsecureSkipTLSVerify` unset.

## Dependencies

- Pin new or upgraded modules to a release at least two weeks old, unless a fix is needed sooner.
- Many useful modules are already indirect dependencies of buildah (cobra, pflag, afero,
  go-containerregistry, logrus, moby client), so check `go.mod` before adding one.
- Run `go mod tidy` after changing imports.
