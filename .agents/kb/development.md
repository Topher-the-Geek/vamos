# Contributor workflow

Work inside the [devcontainer](../../.devcontainer/devcontainer.json).
Image builds need root, a privileged container, `BUILDAH_ISOLATION=chroot`
and a volume for `/var/lib/containers`; no OCI runtime is installed.
The Docker socket is mounted for destination checks.
See [testing.md](testing.md) for required validation.

Use gofmt and keep lines within 120 columns, counting a tab as four.
For prose, follow the semantic-break and technical-item exceptions in [documentation.md](documentation.md).
Match the existing comment density: document functions and add short explanations for non-obvious choices.
Documentation placement is defined in [documentation.md](documentation.md);
package behavior lives in `doc.go` and individual symbol contracts.

Read BUILD files and project sources through `afero.Fs`.
Real filesystem access is for output artifacts, cached content and container storage.
Send target statuses and step output through `run.Events`; the CLI renders them through Cobra's configured writers.
Do not print executor output directly to `os.Stdout`.

Errors are part of the interface.
Test the message for every new user mistake.
Report BUILD errors as one line with `file:line:col` and the offending operation;
destination errors must name the destination.
See `describeEvalError`, `loadError` and `posError` in [load](../../internal/load/doc.go)
and destination wrapping in [cli.go](../../internal/cli/cli.go).
Cheap validation belongs before execution, without an extra project scan.
Keep filesystem selection checks on reachable globs' actual reads.
Command-line mistakes print usage; runtime failures do not.
`main` owns the final `vamos: <error>` prefix and Cobra error printing stays silenced.
Buildah's chroot helper can write failure noise directly to stderr; vamos cannot suppress that output.

Before adding a dependency, inspect [go.mod](../../go.mod):
useful libraries may already be indirect buildah dependencies.
Pin new or upgraded modules to a release at least two weeks old unless an earlier fix is needed.
Run `go mod tidy` after changing imports.
Import constraints are in [TestImportRules](../../internal/deps_test.go), not inferred from directory placement.

Example-specific setup and checks remain in their project guides:
[TypeScript UI](../../examples/typescript-ui/README.md),
[TypeScript MCP](../../examples/typescript-mcp/README.md), [FastMCP](../../examples/fastmcp/README.md),
[Strawberry React](../../examples/strawberry-react/README.md) and [Go CLI](../../examples/golang-cli/README.md).
