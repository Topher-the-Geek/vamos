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
Changes to `install.sh` or the [release workflow](../../.github/workflows/release.yml) need
[smoke check 11](smoke-checks.md), which runs `shellcheck`, and `actionlint` for the workflow,
`go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7`, whose two SC2046 warnings on the merge job's
deliberate word splitting are expected; the workflow itself runs only in a release.
Changes to the Helm chart need `helm lint --strict remote/helm/vamos`, its rendering checked with
`helm template vamos remote/helm/vamos | kubeconform -strict -summary`, for the defaults and for any values the change
adds, and the chart's checks in [smoke-checks.md](smoke-checks.md#kubernetes) on kind.
Documentation-only changes need formatting, link checks,
Go doc inspection and the short suite rather than container smoke runs.

After changing an example, run its own checks using its README's setup.
For TypeScript UI run `pnpm check` and `pnpm build` in its directory,
and `./examples/typescript-ui/check.sh` from the repository root;
for TypeScript MCP this is `npm run check`;
for FastMCP activate `.venv`, then run `ruff check .`, `ruff format --check .`, `pyright` and `pytest`.
The other projects' guides are linked from [development.md](development.md).

Everything a user does or sees is specified as [behavior specifications](#behavior-specifications):
what they write and run, what vamos reports, sends and keeps, and how it reports their mistakes.
A test outside them needs a strong case, and there are three.
The ways a remote cache, coordinator or executor, or the connection to one, can fail
are explored in Go tests, while a few scenarios show how a user sees such a failure.
Performance is measured in Go tests.
Internal contracts, which every implementation of an interface runs, such as `cachetest.Content` and `apitest.Contract`,
are Go tests, as are the checks of the test harness itself, such as the features package's matchers.
Needing a registry, a Docker daemon, emulation, root or a long table of examples is not a case:
a step provides what a scenario needs, or skips it where the host can't, as scenarios skip without buildah.
Mistakes that loading a BUILD file catches are scenarios too, from a background that needs no buildah.

Behavior tested before this rule is moving into scenarios, and [BDD-MIGRATION.md](../../BDD-MIGRATION.md)
lists by category what is left.
Until it moves, it is tested in Go tests that drive the real CLI using `vamos`
in [helpers_test.go](../../internal/cli/helpers_test.go).
Use `.succeeds(t)` and `.failsWith(t, usage, wants...)`; `usage` is true only for command-line mistakes.
Inspect resulting commands, files and tags.
Do not mock the core: push uses an in-process registry,
load uses a Unix-socket fake daemon and a real daemon when available.
Go tests of a package, such as the cache's, the graph's and selection's,
run on real implementations of the filesystem interface.
Every implementation of the stores runs `cachetest.Content` and `cachetest.Records`, and every executor
`apitest.Contract`: the directory and memory stores from the cache package's external tests, `run.Local` directly,
through JSON and through `remote.Client` and `remote.Server` in `TestContractHTTP`, which passes `Fixture.Buffered`
since a transport buffers events.
The remote package's own tests script an executor and inject dropped connections through the server's middleware.

Use `project(t, map[string]string{...})` for projects under `/project` in memory.
Use `diskProject` for symlinks or tests that write the project.
Do not add `testdata/` directories.
Workspace cases need `WORKSPACE` plus each package's BUILD file.
BUILD snippets start with `pullBusybox`; image commands usually name `app`.
Tests that build call `requireBuildah(t)`.
Every `TestMain` that uses buildah is `testenv.Main(m)`, which calls `buildah.InitReexec()` first
and `testenv.RemoveSuiteStores` after the tests; packages whose tests run a worker pass it `worker.ChildFromEnv`.
Table cases use keyed struct literals, one field per line.

On failure, check output absence with `assertNoFiles`.
A scenario checks it with `ThenNothingIsSaved` or `ThenNothingIsExported`,
or with `ThenTheDestinationPathsAre` in a table whose other rows leave something there,
and image cleanup with `ThenNoImageIsLeftBehind`; other tests compare `testenv.Stores.UnnamedImages` before and after.
Successful dependencies remain cached even if a later target fails.
For path and metadata changes inspect [export_test.go](../../internal/finish/export_test.go)
and the end-to-end cases routed by [container-builds.md](container-builds.md).

Tests never touch the developer's container store or build cache.
`testenv.SuiteStores` makes a store and cache for the test binary, beside the default store, removed after the run;
`testenv.PrivateStores` makes a test its own, for one that destroys what they hold, as `TestPrune` does.
In `internal/cli`, `vamos` runs against the suite's stores unless the test called `usePrivateStores`.
Each run of the suite pulls `busybox:1.36` once into its fresh store, which Docker Hub counts against its pull limit,
so tests that need only a base to add files to push one to an in-process registry instead.
Use `newNonce`, `testenv.NewNonce`, for cases that need fresh actions within a run;
`cached` versus `building` is observable.
Tests that need buildah call `requireBuildah(t)`; those that change owners call `testenv.RequireOwnership`.
Both skip where the capability is missing, by probing it, unless `VAMOS_REQUIRE_INTEGRATION` is set,
which makes them fail instead, for a host that must run the whole suite.
The buildah probe needs root and makes and mounts a container from scratch in the suite's store;
the ownership probe needs root, as exports do, and a filesystem that takes `chown`.
Test stores drop the default store's additional image and layer stores,
and are removed only once nothing under them is mounted.
`-short` still skips image builds.
The executor takes its registry settings from its environment, so a test whose executor reaches a registry
over plain HTTP marks it insecure with `testenv.InsecureRegistry`, or `insecureRegistry` in `internal/cli`,
which write a `registries.conf` and call `sysregistriesv2.InvalidateCache`,
because containers/image caches registry configuration.
Trace importer coverage skips without `trace_processor_shell`.

## Behavior specifications

[features](../../internal/features/) specifies the behavior of BUILD files and their targets, pulls, globs, layers,
tasks, their steps, variables and secrets, workspaces, modules, parameters, platforms, the cache,
and the save and export commands as features, rules and scenarios, in Go:
a feature is a file, such as `task_test.go`, and each of its rules is a test named `TestFeature_Rule`,
such as `TestTask_WithoutOutputIsACheck`,
whose doc comment states the rule's sentence, in the words of the [BUILD reference](../../docs/reference/build.md),
the [configuration reference](../../docs/reference/configuration.md),
the [cache reference](../../docs/reference/cache.md) or the [CLI reference](../../docs/reference/cli.md).
A rule's scenarios are its subtests, named by their sentences,
and each starts from its feature's background, such as `newTaskWorld`,
which skips it where buildah can't build, through `newBuildingWorld`.
Scenarios that fail before anything builds, such as the mistakes found as a glob is read,
start from a background that needs no buildah, such as `newGlobMistakeWorld`, so that `-short` runs them too;
a mistake that loading a BUILD file catches is checked with `ThenItFailsWith`, its position and all,
`ThenNoTargetRan`, and `ThenNothingIsSaved` or `ThenNothingIsExported` for the command that sends.
Scenarios run the command line in process, through `cli.NewRootCommand`,
on the suite's stores, or, for one that prunes them with `WhenIPrune`, stores of its own that
`GivenStoresOfTheScenariosOwn` gives it, and on a project held in memory,
or on disk once `GivenTheProjectLinks` gives it symbolic links, `GivenTheProjectIsReachedThroughALink` makes its
directory one, or `WhenISave` saves into it with `intoTheProject`.
They observe only what a user can:
the command's standard output, where its statuses and steps' output go, its error,
the image it saved, the folder it exported and the images left in the store.
A workspace's packages are declared with `GivenThePackageDeclares`, and commands run in one with `GivenIWorkIn`.
Observe a layer's image with `WhenISave` and a task's files with `WhenIExport`;
a push, with `WhenIPush` to a registry of the scenario's own that `GivenARegistry` starts,
which `<registry>` stands for in image names, errors and BUILD text, and Given steps fill with indexes and images
(`<outside>` stands likewise for the folder of files outside the project, once `GivenAFileOutsideTheProjectHolds`
has written one),
reads back what it pushed, for the steps about the image, or `ThenTheIndexManifestsAre` for an index;
a load, with `WhenILoad` into a fake Docker daemon of the scenario's own that `GivenADockerDaemon` starts,
reads back what the daemon loaded, and `GivenTheHostsDockerDaemon` loads into the host's, where there is one;
see a glob's files in a layer that adds them, which the scenario declares, such as
`layer(name="app", base="busybox", steps=[add("code", "/app")])`.
A scenario shows its input, its BUILD file and project, in its own text or in a fixture of its feature:
a constant, such as `theApp`, or a function of the values that vary, such as `depsSizeAndApp(nonce)`,
that returns BUILD text, or the files of a project, such as `locales`,
declared at the top of the feature's file, after its background,
which the scenario or the background declares with `GivenTheBuildFileDeclares` or `GivenTheProjectHolds`,
so that the reader sees the fixture's name and finds its text in the same file.
A background declares only what every scenario of its feature uses, and its doc comment says what that is.
Steps don't write BUILD text, but for the pull of busybox that `GivenAProjectThatPullsBusybox` starts a BUILD file with:
`GivenTheBuildFileDeclares`, `GivenThePackageDeclares` and `GivenTheBuildFileChanges` write the scenario's own,
and `GivenTheBuildFileChanges` changes only text that the scenario or a fixture in the same file declares.
Each command sends to a folder of its own, which `GivenTheDestinationHolds` can fill first,
unless `GivenTheNextCommandSendsWhereTheLastDid` follows one folder from command to command,
and `<destination>` stands for that folder in an expected error or output.
Add a behavior as a scenario of the rule it illustrates.
When cases differ only in short values, loop over a table of examples with a subtest for each, named by the example.
A table's rows differ in values, never in what the scenario does:
steps take the expectation that varies as data, such as `statuses` or the options of a command,
and Given steps that take a collection add nothing for an empty one, so that an optional setup is data too.
A scenario is then one straight sequence of steps, and rows that need different steps are different scenarios,
whose names say so.
Computing what to set up or expect from a row is fine, as the glob's mistakes do with and without `allow_no_files`;
choosing which steps run is not, and a table holds no functions that do,
though it may hold the options that a row passes to a command.

The specifications are written in Go rather than in Gherkin run by godog, which was tried and dropped.
Scenarios are full of multiline inputs, mostly BUILD files, and Gherkin handles them poorly:
a step takes at most one doc string, which must be its last argument,
and BUILD files' own `"""` strings collide with Gherkin's `"""` doc strings.
In Go, a BUILD file is a raw string, indented with the code around it, and a step can take as many as it needs.
Go also catches a misspelled step when it compiles, go to definition finds a step's code,
and a failure names the scenario's line.
`-run`, `-short` and skipping work as they do for any test;
godog runs scenarios without a `*testing.T`, so `testenv`'s helpers would need versions that work without one.

Four principles shape the steps that scenarios are written in.
A scenario shows its input and its expectations, and reading it needs no file but its feature's.
A step does what its name says, no less and no more, and its name says whether a check is exact.
Variation is data: options to steps and values in tables, never Go choosing which steps run.
And anything a scenario checks, Go checks, so that a failure names the property that was wrong.

Steps are the methods of `world`, the state of one scenario,
named as the sentence they stand for after `Given`, `When` or `Then`; its other methods are unexported helpers.
They are split by subject in files that sort together:
[steps_test.go](../../internal/features/steps_test.go) holds the world, how it runs commands,
and the comparisons that its steps share;
[steps_given_test.go](../../internal/features/steps_given_test.go) the project's Given steps;
[steps_commands_test.go](../../internal/features/steps_commands_test.go) the When steps and their options;
[steps_output_test.go](../../internal/features/steps_output_test.go) what a command reported, its targets' statuses
and its steps' observations;
[steps_image_test.go](../../internal/features/steps_image_test.go) the image saved;
[steps_destination_test.go](../../internal/features/steps_destination_test.go) the folder a command sends to;
and [steps_cache_test.go](../../internal/features/steps_cache_test.go) the Given steps that damage the build cache
or the container store, as something outside vamos might, on stores of the scenario's own.
Reuse a step before adding one, and name a new one as what a user does or sees, not as how it is checked.
Steps call `t.Helper`, so a failure names the scenario's line;
checks of whether the command succeeded stop the scenario, and other checks let it go on.
`GivenTheBuildFileDeclares` appends to the pull of the feature's background and dedents,
so a BUILD file is indented with the Go around it.
`files`, `modes`, `links`, `owners`, `environment` and `observations` are maps by path or name;
`testenv.NewNonce` gives a value new to each scenario, for actions that must run rather than come from the cache.

There is one step for each thing a user does or sees, and its variations are options, not steps of their own:
`WhenISave`, `WhenIExport` and `WhenIBuild` take a target and options,
such as `to`, `intoTheProject`, `tagged`, `stampedAt`, `setting`, `withSecrets`, `forPlatform`, `replacing`,
`rebuilding` and `alongWith`,
each of whose doc comments says which commands take it, and a step given one its command doesn't take fails at once.
A flag that a scenario needs gets a named option rather than a new step;
`withFlags` passes flags as they are, for a table whose rows are themselves flags,
or a flag that one feature alone passes, until a second needs it too, but never a flag that an option gives.
Likewise `ThenTheTargetsWere` takes the status expected of each target, by `layer` or `task` and its configuration:
`ran`, `reused`, `rebuilt`, `ranOrReused` or `notBuilt`.

A step's doc comment says what it checks, exactly or at least, and what stops the scenario.
A listing is exact unless its name ends in `AtLeast`:
`ThenTheImageHolds`, `ThenTheExportHolds` and `ThenTheDestinationHolds` list every regular file in their scope,
and fail on anything else there, a link or an empty directory among them;
`ThenTheImagePathsAre`, `ThenTheExportPathsAre` and `ThenTheDestinationPathsAre` list every path,
`ThenTheExportLinks` every link, and `ThenTheImageHoldsNothing`, `ThenNothingIsSaved` and `ThenNothingIsExported`
list nothing.
An image's listings take a scope first, `under` a directory, or `everywhere` for an image on scratch,
since an image on busybox can't be listed whole but a directory in it can.
Use the exact form wherever a scenario can say all that a directory holds,
and `ThenTheImageHoldsAtLeast` only where it can't, such as a file added to a base's `/etc`,
or where a listing of paths is exact, as `ThenTheExportHoldsAtLeast` and `ThenTheDestinationHoldsAtLeast` are used;
an exact check that fails where a subset would pass is a finding, to fix in vamos or the reference, not to loosen.
Steps about named paths, variables or targets, such as `ThenTheImageModesAre`, `ThenTheImageLinks`,
`ThenTheImageEnvironmentSets` and `ThenTheTargetsWere`, check those alone.
`ThenItSucceeds` and `ThenItFailsWith` carry the checks that success and failure mean for every command:
success reports sending what the command sends, nothing if it sends nothing, and a save the saved image's ID;
failure prints no usage and reports nothing sent.
`ThenItFailsShowingUsage` is the failure of a mistake on the command line, which prints usage.
`WhenIRun` runs a command line that the other When steps don't give, such as a mistaken one,
`WhenIRunVamosAlone` runs vamos without arguments,
and `GivenTheCommandRunsIn` gives `-C` as it is, even outside the project, rather than `GivenIWorkIn`.
No other step checks more than its name says, beyond what it needs to check that,
such as a real time to compare with or a path in both images it compares;
steps also stop the scenario on a mistake in the scenario itself, such as a path outside the scope it lists.
When steps fail the scenario only on such a mistake or when the harness can't do what they ask,
such as read the image a save wrote.

What only a running step can see, such as its variables, its secret files, its hostname and hosts,
a scenario checks with probes: shell in its visible BUILD text that reports what the step sees on lines of its output,
`observed <key>=<value>`, which `ThenTheStepsObserved` checks exactly, so that a failure names the property that was
wrong rather than a step's exit status or a missing marker.
Shell observes and reports; Go decides.
A comparison may run in shell only where Go can't see the value, such as a secret or the step's hostname,
and reports its outcome, such as `observed token during=matches`, never as an exit status.
Secrets are compared, never printed, since a step's output is cached and replayed:
the expected value enters the step encoded,
and a scenario about the image checks with `ThenTheImageDoesNotContain` that the value reached neither the image
nor its configuration.
Since observations of a cached step are replayed with its output, a scenario that probes makes the step run,
with a nonce, and checks that it ran with `ThenTheTargetsWere`, unless replay is what it specifies.
Keys that more than one step reports name the step, such as `token during` and `token after`.

Select with `-run`: `go test ./internal/features -run TestTask_` runs the task scenarios,
`-run TestTask_WithoutOutputIsACheck` one rule, and `-v` lists the scenarios as they run.
Scenarios share the suite's container store, so they don't call `t.Parallel`:
another scenario's build would leave images that `ThenNoImageIsLeftBehind` can't tell apart.
