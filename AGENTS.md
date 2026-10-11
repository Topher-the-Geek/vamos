# AGENTS.md

Guidance for coding agents working on vamos. [README.md](README.md) covers what the tool does and how to use it.

## What this is

`vamos` is a Go CLI that builds container images from a Starlark `BUILD` file using the buildah library
(no Dockerfile, no buildah or docker binary at build time). Subcommands send the image to a registry
(`push`), the Docker daemon (`load`), or a tarball (`write`).

## Layout

- `main.go` — only `main()`: buildah re-exec setup, then `internal.NewRootCommand(afero.NewOsFs())`.
  Keep it that way; all other code goes in `internal/`.
- `internal/cli.go` — Cobra commands, argument checks, destinations (`docker://`, `docker-daemon:`,
  `docker-archive:`).
- `internal/buildfile.go` — runs `BUILD` with starlark-go and validates its `glob()`, `pull()`,
  `layer()` and `task()` targets, and the `add()` steps that name earlier targets.
- `internal/step.go` — the other steps a layer or task applies in order (`run()`, `env()`,
  `build_env()`, `workdir()`): each is a Starlark value that knows its part of the cache key and how to apply
  itself with buildah. Add new kinds here.
- `internal/image.go` — pulls, then runs each layer and task with buildah, or reuses its cached result, and
  copies the last layer's image to the destination.
- `internal/cache.go` — the build cache: a content-addressed store of files, trees and logs, and an action
  cache from each target's key to its latest result. Layers' images stay in the container store.
- `examples/` — sample `BUILD` projects, each an MCP server with an echo tool: `nodejs/` (TypeScript) and
  `fastmcp/` (Python). Each has its own lint and tests; see its README.

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
(cd examples/nodejs && npm run check)
(cd examples/fastmcp && . .venv/bin/activate && ruff check . && ruff format --check . && pyright && pytest)
```

Run the full suite before finishing a change that touches building, committing, or destinations.
The first run pulls `busybox:1.36` from Docker Hub; later runs use the local container store.

## Tests

- Tests are sociable: they drive the real command line through `vamos(t, fsys, args...)` in
  `internal/helpers_test.go` and use real buildah. Don't mock the core.
- Projects live in memory: `project(t, map[string]string{...})` returns an `afero.MemMapFs` rooted
  at `/project`. Tests that need symbolic links or write into the project use `diskProject`, which
  keeps them under a temporary directory. Don't add `testdata/` directories.
- Boundaries are verified, not assumed: `push` goes to an in-process registry
  (go-containerregistry), `load` to a fake Docker daemon on a Unix socket (and a real one when
  available), and every result is inspected (command, files, tags).
- Use `.succeeds(t)` and `.failsWith(t, usage, wants...)`. `usage` is true only for command-line
  mistakes; runtime errors must not print usage.
- Failing builds must leave no output behind; check with `assertNoFiles`. The cached results of targets that
  succeeded before the failure are kept, so the next build resumes from there.
- Test BUILD files start with `pullBusybox`, which declares the target `busybox`.
- Tests that build call `requireBuildah(t)` so `-short` and non-root runs skip them.
- Write table cases as keyed struct literals, one field per line.
- `TestMain` must call `buildah.InitReexec()` first, because buildah re-executes the test binary to
  run build steps.


## Sanity checks

The tests never run the built binary, the examples, or the `Dockerfile`. Before finishing a change to
building, destinations, the examples, or the `Dockerfile`, run these by hand. Each sends
`examples/nodejs/echo-request.jsonl` over stdin and should end with its reply:
`{"result":{"content":[{"type":"text","text":"hola"}]},"jsonrpc":"2.0","id":2}`.

```
# 1. Command line: help works; mistakes print `vamos: ...` and exit 1
go build -o vamos . && ./vamos --help && ./vamos nope; ./vamos load

# 2. load into Docker, then run
./vamos load vamos-example -C examples/nodejs \
  && docker run -i --rm vamos-example < examples/nodejs/echo-request.jsonl

# 3. write a tarball, then load and run it (use a temp dir, not the repo)
./vamos write "$TMP/example.tar" -t vamos-example:tar -C examples/nodejs \
  && docker load -i "$TMP/example.tar" && docker run -i --rm vamos-example:tar \
  < examples/nodejs/echo-request.jsonl

# 4. The release image (README's "Run"). Paths in the devcontainer aren't host paths, so pipe
#    the example in instead of bind-mounting it.
docker build -q . -t vamos \
  && tar -C examples/nodejs --exclude=node_modules -c . | docker run -i --rm --privileged \
  -v /var/run/docker.sock:/var/run/docker.sock --entrypoint sh vamos \
  -c 'mkdir /example && tar -x -C /example && vamos load vamos-example:image -C /example' \
  && docker run -i --rm vamos-example:image < examples/nodejs/echo-request.jsonl

# 5. The fastmcp example. Its SDK cancels pending requests when stdin closes, so hold stdin open.
./vamos load vamos-fastmcp -C examples/fastmcp \
  && (cat examples/nodejs/echo-request.jsonl; sleep 2) | docker run -i --rm vamos-fastmcp
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
- buildah reads sources from disk, so `addSources` checks an `add()`'s files out of the build cache into a
  temporary build folder first, at their paths relative to the `BUILD` file, then adds them to its `dest`.
  `TestDestinationModes` checks the folder's own mode doesn't leak.
- `collectOutput` reads a task's files from its mounted container on the host, where an absolute link
  would point at the host's own files. So it resolves `source` with `securejoin` and refuses links
  along included paths; `TestTaskOutputLinks` and `TestTaskFailures` cover this.
- Only `pull()` reaches registries: layers use `PullNever` on image IDs (or `scratch`). Every layer is
  cached: committed to local storage and named `localhost/vamos-cache:<image ID>`, so that tools removing
  unnamed images leave it alone. The last one's is then sent with `copy.Image`, under the system's signature
  policy, not `buildah.Push`, which re-parses the destination as a string. A target that fails leaves no
  unnamed image; tests check this with `assertNoNewImages`.
- The build cache (`internal/cache.go`) sits beside the container store, in `/var/lib/containers/vamos`. Tests
  share it, and a reused target prints `cached` rather than `building`, so tests about caching put a value unique
  to the run (`newNonce`) in their targets. `TestPrune` empties the cache, so the run after a full test run
  builds every target again.
- Layers are committed with `HistoryTimestamp`, which sets every file a layer adds to the target's epoch: the
  latest of its base's and its added tasks' epochs (a pulled image's is its creation time) and `--timestamp`.
  `RewriteTimestamp` only moves times earlier, which would leave build times and source files' own times in
  layers, so the same inputs could make different layers.
- `--tls-verify=false` lets a registry use plain HTTP, both for pushing and for pulling the base image.
  Without the flag, `registries.conf` decides, so leave `DockerInsecureSkipTLSVerify` unset.

## Dependencies

- Pin new or upgraded modules to a release at least two weeks old, unless a fix is needed sooner.
- Many useful modules are already indirect dependencies of buildah (cobra, pflag, afero,
  go-containerregistry, logrus, moby client), so check `go.mod` before adding one.
- Run `go mod tidy` after changing imports.
