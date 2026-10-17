# Container build and delivery lifecycle

The load/run/finish boundary is defined in [architecture.md](architecture.md).
A build first configures all reachable targets and stores their source trees,
then resolves pulls before running targets in dependency order.
It does not validate unrelated workspace packages.
Source bytes used by execution must be the cached bytes used to identify its inputs:
never optimize an `add()` by reading the live project again.
Regression coverage is `TestCacheSourceEditedDuringBuild`
in [build_cache_test.go](../../internal/cli/build_cache_test.go).

The result kinds have different lifetimes: a layer produces an image in the executor's container store;
a task discards its container and may retain a tree of files; an index assembles images for several platforms.
See [run.doc.go](../../internal/run/doc.go) for execution ownership and [storage.md](storage.md) for reuse.

Platform selection spans configuration, execution and delivery.
`EXEC_PLATFORM` describes the executor, whereas a target's platform describes its requested output.
`workspace.edge` and `workspace.imageRoot` in [load](../../internal/load/doc.go) own propagation
and destination selection.
Indexes are command-line roots;
`build` and `push` keep the index, while `load` and `save` choose a manifest before configuring its dependencies.
This avoids building images a single-image destination cannot consume.

Cross-compilation and emulated execution are separate choices.
See the native compiler configuration in [golang-cli/BUILD](../../examples/golang-cli/BUILD).
Foreign executable steps depend on the host kernel's binfmt_misc handlers;
`setPlatform`, `forPlatform` and `emulatorHint` in [run](../../internal/run/doc.go) own platform checks,
library contexts and diagnostics.
For Docker Desktop's bundled QEMU support,
see its [emulation guide](https://docs.docker.com/build/building/multi-platform/#qemu).
On Apple silicon, Rosetta for amd64 is an option tied to the selected VM backend;
see [Desktop settings](https://docs.docker.com/desktop/settings-and-maintenance/settings/#general).
The buildah error-reporting limitation is documented beside `emulatorHint` in [runner.go](../../internal/run/runner.go).

Delivery is a separate commit from caching a successful target.
A failed build must publish no requested file output, while previously successful dependency results remain reusable.
For file destinations, inspect `Save`, `Export`, `install`
and `swap` in [finish](../../internal/finish/doc.go) before changing preflight or installation.
Preflight checks improve diagnostics but cannot authorize deleting files that appear while a build runs.
The replacement fallback may briefly leave the destination absent; cleanup failure after installation is a warning.

Transport details belong beside their owners:
`archiveReference` and `LoadDocker` in [finish.go](../../internal/finish/finish.go), `exportIndex`
and `policyContext` in [executor.go](../../internal/run/executor.go),
and `newSystemContext` in [cli.go](../../internal/cli/cli.go).
Construct image references with transport packages;
preserve the system signature policy
and the distinction between an unset TLS override and explicit `--tls-verify=false`.

For host/container path isolation, start with `cache.Checkout`,
`fileset.Walk`, `targetState.addSource` and `collectOutput`.
Their symbol comments own link handling and directory metadata rules.
The end-to-end guards are `TestDestinationModes`, `TestTaskOutputLinks`
and `TestTaskFailures` in [build_test.go](../../internal/cli/build_test.go),
and [export_test.go](../../internal/finish/export_test.go).
