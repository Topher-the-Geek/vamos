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
- `internal/buildfile.go` — runs `BUILD` with starlark-go and validates `build()` arguments.
- `internal/image.go` — builds and commits the image with buildah.
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
go test ./...                 # everything, including image builds (~20s, needs root + network)
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
- Failing builds must leave nothing behind; check with `assertNoFiles`.
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
- Catch mistakes before building when possible (bad names, missing sources, unwritable output),
  so users don't wait for a build to fail.
- `main` prints `vamos: <error>`; Cobra's own error printing is silenced, so the root command
  reports unknown commands itself.
- Known noise: buildah's chroot helper writes lines such as `subprocess exited with status 1`
  directly to stderr when a build step fails. vamos can't suppress them.

## Code style

- gofmt (tabs), and lines of at most 120 characters, counting a tab as 4 columns. Break up dense lines
  even when they fit.
- Match the surrounding comment density: a doc comment on each function, and short "why"
  comments.
- Build-step output goes to the command's stdout and stderr (`cmd.OutOrStdout()` and
  `cmd.ErrOrStderr()`), never directly to `os.Stdout`.
- Read `BUILD` files and sources through the `afero.Fs`, never `os`. Output tarballs and buildah's
  storage use the real filesystem.

## Gotchas

- `docker-archive` refuses to overwrite a non-empty file. `write` builds to a temp file beside the
  target, created only just before committing, and renames it into place.
- `docker-archive` rejects paths containing `:`, so `archiveReference` reaches such a file through
  `/proc/self/fd`. Build references with the transport packages, not `alltransports.ParseImageName`.
- `load` doesn't use the `docker-daemon` transport, which always uses TLS for `tcp://` hosts and
  ignores `DOCKER_CERT_PATH`. It writes a tarball and sends it with the moby client,
  using the Docker connection settings from the environment.
- containers/image caches `registries.conf` regardless of `CONTAINERS_REGISTRIES_CONF`; tests that
  set it must call `sysregistriesv2.InvalidateCache`.
- buildah reads sources from disk, so `buildImage` copies them from the `afero.Fs` into a
  temporary build folder first.
- `--tls-verify=false` lets a registry use plain HTTP, both for pushing and for pulling the base image.
  Without the flag, `registries.conf` decides, so leave `DockerInsecureSkipTLSVerify` unset.

## Dependencies

- Pin new or upgraded modules to a release at least two weeks old, unless a fix is needed sooner.
- Many useful modules are already indirect dependencies of buildah (cobra, pflag, afero,
  go-containerregistry, logrus, moby client), so check `go.mod` before adding one.
- Run `go mod tidy` after changing imports.
