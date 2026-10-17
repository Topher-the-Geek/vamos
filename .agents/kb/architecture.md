# Architecture and phase boundaries

vamos evaluates Starlark BUILD projects and builds OCI images through the buildah library.
It does not generate Dockerfiles or invoke Docker/buildah executables to build targets.
Docker is a destination, not the build engine.

| Phase | Placement | Owner | Boundary |
| --- | --- | --- | --- |
| Load | Client | [load](../../internal/load/doc.go) | Project and Starlark become a graph and cached sources |
| Run | Executor | [run](../../internal/run/doc.go) | Cached graph and sources become images or task files |
| Finish | Client or executor | [finish](../../internal/finish/doc.go) | Results reach the requested destination |

[cli](../../internal/cli/doc.go) coordinates the phases.
The executor owns buildah, container storage and base image pulls.
Push normally runs there; `--push-from=client` selects client credentials and registry settings.
Finish may contact a registry for that client push, so registry access is not exclusively an executor concern.

The graph and build cache are the load/run data boundary.
No Starlark values, open project files or in-memory loader state may cross it.
This keeps execution independent of the project and leaves room for a remote executor;
the current implementation is `run.Local`, not a distributed scheduler or cache service.
Dependency constraints are executable in [TestImportRules](../../internal/deps_test.go);
consult that test before adding imports.

The two notions of package differ: a BUILD package is a workspace directory addressed by `//dir:name`;
a Go package implements one of these subsystems.
Target identity before execution includes configuration.
See `confLabel` in [config.go](../../internal/load/config.go) for the loader's identity model
and [graph.doc.go](../../internal/graph/doc.go) for the serialized execution model.

Keep [main.go](../../main.go) limited to re-exec initialization, command execution and final error reporting.
Implementation belongs under `internal/`; the independent example modules are BUILD clients.

For a new build step, change Starlark construction/lowering in [load/step.go](../../internal/load/step.go),
the encoded form and validation in [graph](../../internal/graph/doc.go),
and execution plus action-key coverage in [run](../../internal/run/doc.go).
Graph compatibility and cache compatibility have separate version owners: `graph.Version` and `run.actionVersion`.
