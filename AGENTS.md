# AGENTS.md

Guidance for coding agents working on vamos. [README.md](README.md) covers what the tool does and how to use it.

## What this is

`vamos` is a Go CLI that builds container images from Starlark `BUILD` files using the buildah library
(no Dockerfile, no buildah or docker binary at build time). Subcommands send a target's image to a registry
(`push`), the Docker daemon (`load`), or a tarball (`save`, formerly `write`), or write a task's files to a
folder (`export`). A workspace (a directory with a `WORKSPACE`
file) holds a `BUILD` file per package, which name each other's targets by label, `//dir:name`.

## Layout

- `main.go` — only `main()`: buildah re-exec setup, then `cli.NewRootCommand(afero.NewOsFs())`.
  Keep it that way; all other code goes in `internal/`.

A build has three phases, each in its own packages, which pass nothing between them but the build graph and the
build cache. This is with remote building in mind: load runs on the client, and is the only phase that reads the
project; run runs on an executor that could be remote, and is the only one that reaches buildah, the container
store and registries; finish runs on the client, except for `push`, which runs on the executor unless
`--push-from=client`. `internal/deps_test.go` checks which packages each may import.

- `internal/cli/` — Cobra commands, argument checks, and the client's part of a build: loading the graph, storing
  it in the build cache, having the executor run it, printing its `Events`, and finishing.
- `internal/load/` — the load phase. `load.go` is its API, `Load`. `buildfile.go` finds the workspace, runs
  each package's `BUILD` with starlark-go as labels name it, and `.star` files as `load()` names them, and
  validates its `glob()`, `pull()`, `layer()`, `task()` and `index()` targets, and the `add()` steps that name
  other targets. `config.go` has targets' parameters (`params`, `param()`), `dep()` and `arg()`, each target's
  configurations (its platform and arguments) and `ctx`, and configuring: each target the named targets need, in
  each configuration it's needed in, lowered to a graph. `step.go` makes the other steps (`run()`, `env()`,
  `build_env()`, `workdir()`), each a Starlark value that lowers to a `graph.Step`.
- `internal/graph/` — the build graph: the nodes a build runs (globs' files, pulls, layers, tasks, indexes) and
  their steps, as plain data with no Starlark, its encoding, and `Validate`. `vars.go` has what both phases know
  of steps' `$NAME` references.
- `internal/run/` — the run phase: the `Executor` interface, and `Local`, which reads a graph from the build cache,
  pulls (`pull.go`, which resolves each tag once and records what it named), runs its layers and tasks with
  buildah or reuses their cached results (`runner.go`), and pushes or exports a result. `step.go` applies each
  step's op with buildah, and `action.go` says what of a target is in its cache key. Add a new kind of step to
  `load/step.go`, `graph`, and `run/step.go`.
- `internal/finish/` — sends a result to a registry, the Docker daemon, or a tarball, or, for `export`, writes a
  task's files to a folder (`export.go`).
- `internal/cache/` — the build cache: a content-addressed store of files, trees and logs, and an action
  cache from each target's key to its latest result. Layers' images stay in the container store.
- `internal/fileset/` — the selection of a glob's or task's files: include and exclude patterns, mode mask and
  limits, and the walk that reads only the folders they could match into a tree for the cache.
- `internal/tracing/` — traces, for `--trace`: spans, plain data so that an executor can send them to the client,
  the recorder that a context carries, estimating an executor's clock offset, and writing Chrome's JSON trace
  format for Perfetto. It uses no other package of vamos.
- `internal/testenv/` — what several packages' tests share: projects to build, and `RequireBuildah`.
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
  `internal/cli/helpers_test.go` and use real buildah. Don't mock the core. Most tests are in `internal/cli`.
- Complex units with many cases, such as pattern matching and the walk in `fileset`, the cache's trees, or the
  graph's validation, may also be tested on their own, in their packages, on an `afero.MemMapFs`, or
  `testenv.DiskProject` for symbolic links. That isn't
  mocking: it is the real code on a filesystem. A build test still checks that their results reach the image.
- Projects live in memory: `project(t, map[string]string{...})`, which is `testenv.Project`, returns an
  `afero.MemMapFs` rooted at `/project`. Tests that need symbolic links (`MemMapFs` has none) or write into the
  project use `diskProject`, which keeps them under a temporary directory. Don't add `testdata/` directories.
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

# 3. save a tarball, then load and run it (use a temp dir, not the repo)
./vamos save server "$TMP/example.tar" -t vamos-example:tar -C examples/typescript-mcp \
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
  && ./vamos save //:layer "$TMP/echo.tar" -t vamos-echo:other --platform linux/amd64 -C examples/golang-cli \
  && docker load -i "$TMP/echo.tar" && docker run --rm --platform linux/amd64 vamos-echo:other
```

```
# 8. export: the strawberry-react UI's generated GraphQL types, written to a temp dir, then again over them.
#    Expect src/gql's files, owned by you, and "it isn't empty" from the second, until --replace.
S=examples/strawberry-react
./vamos export //ui:graphql-codegen "$TMP/gql" -C $S && ls -ln "$TMP/gql" \
  && ./vamos export //ui:graphql-codegen "$TMP/gql" -C $S; ./vamos export //ui:graphql-codegen "$TMP/gql" \
  --replace -C $S
```

```
# 9. A trace, queried with Perfetto's trace processor, which the devcontainer has. Expect the slowest spans, then
#    arrows from what each glob, pull or target made to the steps that use it: none if every target was reused.
./vamos load server vamos-example -C examples/typescript-mcp --trace "$TMP/trace.json" \
  && echo "select name, dur/1000000 as ms from slice order by dur desc limit 10;
           select s1.name, s2.name from flow join slice s1 on slice_out = s1.id join slice s2 on slice_in = s2.id;" \
  | trace_processor_shell -q /dev/stdin "$TMP/trace.json"
```

`push` has no manual check here: the tests already push to a real (in-process) registry, from the executor
and from the client.

## Error messages

Errors are a feature here, and every user mistake should have a test for its message.

- Say what was wrong and where: `BUILD` errors are one line, `file:line:col: build: ...`
  (see `describeEvalError`); destinations are named, e.g. `pushing to app:latest: ...`.
- Catch mistakes before building when it's cheap to (bad names, missing sources, unwritable output).
  Don't add extra passes over the project just to report rarer problems sooner. Every glob a build uses is read
  once, before anything runs, so its problems, such as unreadable files or a pattern matching nothing, are reported
  then, as `BUILD` errors at the glob: `file:line:col: glob: ...`.
- `main` prints `vamos: <error>`; Cobra's own error printing is silenced, so the root command
  reports unknown commands itself.
- Known noise: buildah's chroot helper writes lines such as `subprocess exited with status 1`
  directly to stderr when a `run()` step fails. vamos can't suppress them.

## Code style

- gofmt (tabs), and lines of at most 120 characters, counting a tab as 4 columns. Break up dense lines
  even when they fit.
- Match the surrounding comment density: a doc comment on each function, and short "why"
  comments.
- Output of `run()` steps, and targets' statuses, go to the executor's `Events`, which the command prints to its
  stdout and stderr (`cmd.OutOrStdout()` and `cmd.ErrOrStderr()`), never directly to `os.Stdout`.
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
  `fileSet`), one per target and configuration (`confLabel`), which `lowering` turns into the graph's nodes, in
  the order the build runs them. A configuration belongs to one target: its platform and a value for each of its
  parameters, and `workspace.edge` works out each used target's from the using one's. Labels outside functions
  name earlier targets, but those in functions needn't, so `configurer` reports cycles.
- `arg()` runs before the call declaring the target that uses it, so it records its position, and the declarer
  (or `template`, for what functions give) checks its name (`checkArgNames`), reporting it there with `posError`.
  Its value's type is only checked by `edge`, as the using target is configured.
- Builtins are made once per workspace and read what's calling them from the thread's `evalContext`: a `BUILD`
  file running, a function of `ctx` (which can't declare targets), or a `.star` file loading. A function from a
  `.star` file thus resolves labels against the `BUILD` file or target it runs for.
- Checks that touch the filesystem, such as that a glob's patterns' leading folders exist, run only for the
  targets a command needs, as `readGlobs` reads their globs. There are no workspace-wide checks. Whether patterns
  match any file, and the limits, are checked by the walk, as the files are read.
- `addSource` adds only a tree's outermost entries, grouped by folder, so folders above them keep the image's
  own mode and owner, and with `FollowSymlink` off, since buildah would otherwise copy what a top-level link
  points to.
- `docker-archive` refuses to overwrite a non-empty file. `save` builds to a temp file beside the
  target, created only just before copying, and renames it into place.
- `export` stages a task's files beside the folder, then installs them in one step: `rename(2)`, which the kernel
  refuses if the folder holds anything, or, with `--replace`, `renameat2(RENAME_EXCHANGE)`. On a filesystem that
  can't exchange names (such as NFS), `swap` moves the folder aside first, so it is briefly missing. Don't check the folder
  and then remove it: files can appear in it while the task builds. Installing is the commit point; failing to
  remove what was replaced after it is only a warning. Times and owners are set with `UtimesNanoAt` and
  `AT_SYMLINK_NOFOLLOW`, and `Lchown`, since a task's links can point anywhere on the host
  (`internal/finish/export_test.go`).
- `docker-archive` rejects paths containing `:`, so `archiveReference` reaches such a file through
  `/proc/self/fd`. Build references with the transport packages, not `alltransports.ParseImageName`.
- `load` doesn't use the `docker-daemon` transport, which always uses TLS for `tcp://` hosts and
  ignores `DOCKER_CERT_PATH`. It writes a tarball and sends it with the moby client,
  using the Docker connection settings from the environment.
- containers/image caches `registries.conf` regardless of `CONTAINERS_REGISTRIES_CONF`; tests that
  set it must call `sysregistriesv2.InvalidateCache`.
- The files of each glob a build uses are stored in the build cache as the graph is loaded (`readGlobs`), and
  targets are built from that copy, never from the project, so a file edited mid-build can't be cached under the
  wrong key (`TestCacheSourceEditedDuringBuild`). Keep it that way: it is also what a remote builder will need.
- buildah reads sources from disk, so `addSource` checks an `add()`'s files out of the build cache into a
  temporary build folder first, at their paths relative to the `BUILD` file, then adds them to its `dest`.
  `TestDestinationModes` checks the folder's own mode doesn't leak.
- `collectOutput` reads a task's files from its mounted container on the host, where an absolute link
  would point at the host's own files. So it resolves `source` with `securejoin` and refuses links
  along included paths; `TestTaskOutputLinks` and `TestTaskFailures` cover this.
- Only `pull()` reaches registries: layers use `PullNever` on image IDs (or `scratch`). A pull resolves its tag
  once and records the manifest it names, and each platform's image in it, in the build cache (`tagRecord`, kept by
  `prune`), so later builds don't reach the registry until `--rebuild`. Pulled images are named by digest in the
  store, since a tag can name only one image there, and every platform's would need it. A pull copies the image
  with `copy.Image` (`puller.fetch`), not `buildah.Pull`, so that a trace shows its blobs, and retries as buildah
  would. Find a pulled image with `ImagesByDigest` first, since copying asks the registry even when the store has
  the image:
  Docker Hub allows 100 anonymous pulls an hour, and then stalls requests, so a test run would hang
  (`TestPullTag` counts requests). Every layer is
  cached: committed to local storage and named `localhost/vamos-cache:<image ID>`, so that tools removing
  unnamed images leave it alone. The named target's is then sent with `copy.Image`, under the system's signature
  policy, not `buildah.Push`, which re-parses the destination as a string. A target that fails leaves no
  unnamed image; tests check this with `assertNoNewImages`.
- The build cache (`internal/cache`) sits beside the container store, in `/var/lib/containers/vamos`. Tests
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
- An `index()` is only ever named on the command line. For `build` and `push` it is an `Index` node, which runs
  once its manifests are built; for `load` and `save`, which take one image, `Load` names the manifest for the
  destination's platform instead, before configuring, so that the others aren't built. `exportIndex` sends an
  index through a temporary OCI layout, since containers/image copies an index, with `CopyAllImages`, only from a
  source that holds one.
- The graph is the only thing that passes from configuring to building: the client stores it in the build cache,
  and `Local.Run` reads it back by digest and validates it, as a remote executor would, so nothing else can leak
  across. `EXEC_PLATFORM` is the executor's platform (`Executor.ExecPlatform`). Every mistake a `BUILD` file can
  make must be reported while configuring, with where; `Graph.Validate` failing means a bug.
- `--tls-verify=false` lets a registry use plain HTTP, both for pushing and for pulling the base image.
  Without the flag, `registries.conf` decides, so leave `DockerInsecureSkipTLSVerify` unset.
- Traces: spans come from `tracing.Start` and its kin, through the context, and cost nothing without `--trace`.
  - **Crossing the boundary.** The executor gets a `tracing.CallContext` in the request of each call that does work
    (`Run`, `Push`, `Export`), never a Go context, since a remote one couldn't get that. It sends its spans back
    through `Events.Spans` in batches as they end (`spanBatch`), the last as the call returns, never to `os.Stdout`.
    Work done only for a span's arguments goes behind `tracing.Enabled`.
  - **Clocks.** The client places the executor's spans by its own clock, from `Executor.Clock`
    (`tracing.EstimateOffset`).
  - **Nesting and tracks.** Spans on a track must nest, so each pull and target starts its own
    (`tracing.StartTrack`). Blobs are async, since containers/image copies several at once.
  - **Blob events.** containers/image's events don't pair up: a skipped blob has no start, a failed copy leaves
    starts with no end, and a config is reported as copied even when the registry has it. `blobTracker` handles
    each.
  - **Arrows.** A span either produces or uses one thing (`graph.FlowKey`), since Perfetto binds a slice to one
    arrow's end.
  - **Times.** They are written as microseconds since the trace's start, with a fraction, which Perfetto reads to
    the nanosecond. Whole microseconds would let a short last step escape its target.
  - **Testing.** `TestTracePerfetto` checks that Perfetto's importer agrees with the tests' reading of a trace. It
    is skipped without `trace_processor_shell`, which the devcontainer has.

## Dependencies

- Pin new or upgraded modules to a release at least two weeks old, unless a fix is needed sooner.
- Many useful modules are already indirect dependencies of buildah (cobra, pflag, afero,
  go-containerregistry, logrus, moby client), so check `go.mod` before adding one.
- Run `go mod tidy` after changing imports.
