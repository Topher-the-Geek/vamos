# Moving behavior tests into scenarios

The rule in [testing.md](.agents/kb/testing.md#validation-and-test-conventions):
everything a user does or sees is a scenario in [features](internal/features/).
A Go test needs one of three reasons to stay one:
the failure modes of a remote cache, coordinator or executor, or of the connection to one;
performance;
or an internal contract, or a check of the test harness itself.

This list holds what is left, by category.
Tick a category once its behavior is in scenarios and its old tests are gone, or once each that stays has its reason.
Delete this file when every box is ticked.

## Move into scenarios

- [x] **Mistakes in a BUILD file's targets and steps, and in the targets a command names.**
  `TestBuildFileMistakes` and `TestBuildTargetMistakes` are scenarios of the BUILD file, targets, pull, glob, task,
  add, run, env and secrets features.
- [x] **Mistakes in parameters, `--set`, `dep()` and `arg()`.**
  `TestParamMistakes`, `TestSetMistakes`, `TestDepMistakes`: scenarios of the parameters, targets, platforms
  and pull features.
- [x] **Mistakes in `load()` and `.star` files.**
  `TestLoadMistakes`: scenarios of the modules feature.
- [x] **Mistakes in workspaces.**
  `TestWorkspaceMistakes`, `TestWorkspaceErrorInOtherPackage`, `TestWorkspaceTargetMistakes`,
  and `TestPackagesNamingEachOther`'s cycles and late names: scenarios of the workspaces feature,
  from a background that needs no buildah, so `-short` runs them;
  each states the error, with its `file:line:col` and operation.
- [x] **Mistakes on the command line.**
  `TestCommandLineMistakes`, `TestUnknownCommands`, `TestNoCommandShowsHelp`, `TestInvalidImageNames`,
  `TestProjectDirectoryMistakes` and `TestTLSVerifyFlagForms` are scenarios of the command line feature;
  the flag mistakes of the platform, index, trace, remote and coordinator tests move with their categories,
  and `TestMissingAuthFile` with push.
- [x] **Workspace loading.**
  `TestWorkspaceRoot`, `TestLabelsInOwnPackage`, `TestWorkspaceSearchError`, `TestPackageLinks`, `TestBuildVms`,
  `TestPackagesNamingEachOther`.
  The workspaces feature, observed through what builds rather than the graph.
- [x] **Push and index push.**
  `TestPush`, `TestPushWithAuthFile`, `TestTLSVerifyIsTheClients`, `TestPushFailures`, `TestMissingAuthFile`,
  `TestIndexMistakes`, `TestIndexByPlatform`, and the push and save parts of `TestIndex`, `TestIndexChoosesManifest`
  and `TestPlatformVariants` are scenarios of the push, index and platforms features.
- [x] **Load into Docker.**
  `TestLoad`, `TestLoadRejected`, `TestLoadWithoutDocker`, `TestLoadIntoDocker`, the load parts of `TestIndex`,
  `TestIndexChoosesManifest` and `TestPlatformVariants`, and load refusing a task are scenarios of the load feature.
- [x] **Pulls by tag.**
  `TestPullTag` and `TestPullTagMistakes` are scenarios of the pull feature, from a registry of the scenario's own.
- [x] **Platforms and emulation.**
  `TestPlatformMistakes`, `TestPlatformFailures` and what `TestPlatforms` builds are scenarios of the platforms
  feature. `TestPlatforms` itself stays, for the provenance it reads from the cache's files, as decided below;
  `TestExecPlatformChoice`, a choice among a coordinator's platforms, moves with remote work.
- [x] **Steps and task outputs.**
  `TestIncludeProject`, `TestSteps`, `TestWorkdirUser` and `TestTaskOutputLinks` are scenarios of the glob, env,
  layer and task features.
- [x] **Export details.**
  `TestExportLinks`, `TestExportOwners`, `TestExportSetuid`, `TestExportReplace` and `TestExportIntoEmptyFolder`
  are scenarios of the export feature; the rest of `internal/finish`'s export tests, which inject faults or races,
  are decided below.
- [x] **The cache as a user sees it.**
  `TestCacheCorruption`, `TestCacheEntryRemoved`, `TestMovedCacheName`, `TestUnverifiedLayers` and
  `TestImageStoreIDs` are scenarios of the cache feature. What no user sees is decided below:
  the provenance that `TestResultProvenance` and `TestPlatforms` read from results,
  the names a pull gives in the store that `TestImageIdentityRoundTrip` reads, and `TestExportRepairsCorruptBlob`.
- [x] **Prune and store roots.**
  `TestPrune` and `TestPruneRefusedWhileInUse` are scenarios of the prune feature. `TestExplicitStoreRoots`, of the
  store a program embedding vamos gives it, and `TestDefaultStore`, which needs a process of its own since
  containers/storage reads its defaults once, are decided below; `TestPruneHandlerWithoutRoot` moves with write
  failures.
- [x] **Traces.**
  `TestTraceBuild`, `TestTracePush`, `TestTracePull`, `TestTraceFailure`, `TestTraceDestinations`,
  `TestTraceMistakes` and `TestTracePerfetto` are scenarios of the trace feature, which reads the file `--trace`
  writes; Perfetto's import still skips without `trace_processor_shell`.
  `TestSecretsNotInTrace` is a scenario of the secrets feature.
  `TestFinishTrace`, of a trace whose folder goes while the command runs, is decided below.
- [x] **Interruption and failure logs.**
  `TestInterrupted`, `TestCancelLoad` and `TestCancelConfigure` are scenarios of the interrupt feature;
  `TestDeadlineInBuildFile` keeps their deadline, decided below.
  `TestFailedStepLog` and `TestFailedOutputLog`, of the log that an executor's `api.Failure` names, which the command
  line never prints, and which a coordinator replays to jobs that share a failed attempt, are internal contracts,
  below.
- [x] **Failures to write output.**
  What `TestOutputWriteFailure`, `TestBuildHandlerWithoutRoot` and `TestPruneHandlerWithoutRoot` checked of a
  standard output that can't be written is the output feature's, and the prune feature's; the graph that
  `vamos graph` prints is read by `WhenIShowTheGraph`, which the trace feature uses.
  The handlers' running without the root command, which the latter two and `TestHandlerWithoutRoot` keep, and
  `TestWatchJobWriteFailure`, of how a cancelled job's events are drained into the trace, are internal contracts,
  below.
- [ ] **How a user sees remote work and its failures.**
  `TestRemoteBuild` and `TestRemoteMistakes` are scenarios of the remote feature, and `TestCheckRegistryHost` and
  `TestCoordinatorFlagMistakes` of the coordinator feature.
  Still to move: `TestCoordinatorCommand`, `TestCoordinatorWithWorker`, `TestExecPlatformChoice`.
  `TestUsesContainerStore`, of which command lines main re-executes in a user namespace, is an internal contract.
  The failure modes themselves stay below.

## Keep as Go tests, confirming each one's reason

- [ ] **Remote failure modes.**
  `internal/coord`, `internal/remote`, `internal/worker` and `internal/transfer`'s retries and read-backs:
  restarts, lost workers, fencing, expiry, retries, outages and resumed watches.
- [ ] **Internal contracts.**
  `cachetest.Content` and `cachetest.Records` (`TestContentStore`, `TestRecordStore`, `TestContentStoreContract`),
  `apitest.Contract` (`TestContract`, `TestContractJSON`, `TestContractHTTP`), `TestErrorsCross`,
  `TestFailureCrosses`, `TestWorkerFailureCrosses`, `TestReleaseCrosses`, `TestGraphValidate`, `TestGraphRoundTrip`,
  `TestFailedStepLog`, `TestFailedOutputLog`,
  `TestHandlerWithoutRoot`, `TestBuildHandlerWithoutRoot`, `TestPruneHandlerWithoutRoot`, `TestWatchJobWriteFailure`,
  `TestUsesContainerStore`,
  `TestGraphExamples` and `internal/api`.
- [ ] **The test harness.**
  `internal/testenv`, and the features package's own tests of its matchers and image reader.
- [ ] **Performance.**
  `TestWalkTreeReadsOnlyWhatItNeeds`, `TestStoreBatchesMissing`, `TestReadManyBatches`, `TestSpanBatches`.

## Decide

- [ ] **Mechanisms beneath a feature.**
  `internal/sched` (action keys, sharing, job limits, the scheduler's reuse and regeneration),
  `internal/fileset` (patterns, tree walks, edits during capture), `internal/cache` (logs, locks, generations),
  `internal/cli`'s `TestGraphConfigurations`, of the graph an index of two platforms configures,
  `TestFinishTrace`, of which error a command reports when its trace can't be written after all,
  `TestExplicitStoreRoots` and `TestDefaultStore`, of the container store vamos opens,
  `TestDeadlineInBuildFile`, of a deadline that a program embedding vamos sets, which a user's interrupt never is,
  `TestResultProvenance` and `TestPlatforms`, of the provenance a result records,
  and `TestImageIdentityRoundTrip`, of the names a pull gives an image in the store,
  `internal/run`'s `TestExportRepairsCorruptBlob`,
  `internal/finish`'s delivery from separate stores, and its export tests that inject a fault or race:
  `TestExportFolderFilledDuringBuild`, `TestExportReplacesOnlyFolders`, `TestExportCleanupFails`,
  `TestExportPutBackFails` and `TestExportReplaceWithoutExchange`,
  `internal/run`'s runner and its puller (`TestPullerPinnedIndex`, `TestMaterializeFromRegistry`,
  whose refusals of a mismatched manifest, a wrong image ID or a location that can't be parsed
  the pull feature doesn't reach), and `internal/tracing`.
  Each either shows in what a user sees, and moves into the feature it serves,
  or is an internal contract, and says so; anything that is neither needs a reason added to the rule.
