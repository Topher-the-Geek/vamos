# Validation and test conventions

Run from the repository root in the [development environment](development.md):

```sh
go build -o vamos .
go vet ./...
gofmt -l .                 # must print nothing
go test -short ./...       # skips image builds
go test ./...              # includes real buildah; needs root and network
```

For timeout planning, previous devcontainer runs took under one second for the short suite
and about 90 seconds for the full suite.
These are approximate timings, not deadlines: allow extra time for cold Go compilation, image pulls and emulation.

Run the full suite before finishing changes to building, committing or destinations.
Run [smoke-checks.md](smoke-checks.md) for changes to building, destinations, examples or the Dockerfile;
automated tests do not execute the built binary, example projects or release Dockerfile.
Documentation-only changes need formatting, link checks,
Go doc inspection and the short suite rather than container smoke runs.

After changing an example, run its own checks using its README's setup.
For TypeScript UI run `pnpm check` and `pnpm build` in its directory,
and `./examples/typescript-ui/check.sh` from the repository root;
for TypeScript MCP this is `npm run check`;
for FastMCP activate `.venv`, then run `ruff check .`, `ruff format --check .`, `pyright` and `pytest`.
The other projects' guides are linked from [development.md](development.md).

Most behavior tests drive the real CLI using `vamos` in [helpers_test.go](../../internal/cli/helpers_test.go).
Use `.succeeds(t)` and `.failsWith(t, usage, wants...)`; `usage` is true only for command-line mistakes.
Inspect resulting commands, files and tags.
Do not mock the core: push uses an in-process registry,
load uses a Unix-socket fake daemon and a real daemon when available.
Complex cache, graph and selection cases can also exercise their packages directly
on real implementations of the filesystem interface.

Use `project(t, map[string]string{...})` for projects under `/project` in memory.
Use `diskProject` for symlinks or tests that write the project.
Do not add `testdata/` directories.
Workspace cases need `WORKSPACE` plus each package's BUILD file.
BUILD snippets start with `pullBusybox`; image commands usually name `app`.
Tests that build call `requireBuildah(t)` so `-short` and non-root runs skip them.
Every `TestMain` that uses buildah must call `buildah.InitReexec()` first.
Table cases use keyed struct literals, one field per line.

On failure, check output absence with `assertNoFiles` and image cleanup with `assertNoNewImages`.
Successful dependencies remain cached even if a later target fails.
For path and metadata changes inspect [export_test.go](../../internal/finish/export_test.go)
and the end-to-end cases routed by [container-builds.md](container-builds.md).

Build tests share the machine's cache and container store.
Use `newNonce` for cases that need fresh actions; `cached` versus `building` is observable.
`TestPrune` and `TestPullTag` empty the cache, so the next build runs targets again.
The initial run pulls `busybox:1.36`; avoid redundant registry requests and concurrent destructive cache tests.
Tests overriding `CONTAINERS_REGISTRIES_CONF` must call `sysregistriesv2.InvalidateCache`
because containers/image caches registry configuration.
Trace importer coverage skips without `trace_processor_shell`.
