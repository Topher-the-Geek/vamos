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
- [ ] **Platforms and emulation.**
  `TestPlatformMistakes`, `TestExecPlatformChoice`, `TestPlatforms`, `TestPlatformFailures`, `TestGraphConfigurations`.
  The platforms feature, skipping where the host can't emulate.
- [ ] **Steps and task outputs.**
  `TestIncludeProject`, `TestSteps`, `TestWorkdirUser`, `TestTaskOutputLinks`.
  Tables of examples in the run, add and task features; rows that don't build run without buildah.
- [ ] **Export details.**
  `internal/finish`: `TestExportLinks`, `TestExportOwners`, `TestExportSetuid`, `TestExportReplace`,
  `TestExportReplacesOnlyFolders`, `TestExportIntoEmptyFolder`, `TestExportFolderFilledDuringBuild`.
  The export feature; owners skip where the host can't `chown`.
- [ ] **The cache as a user sees it.**
  `TestCacheCorruption`, `TestCacheEntryRemoved`, `TestMovedCacheName`, `TestUnverifiedLayers`,
  `TestResultProvenance`, `TestExportRepairsCorruptBlob`, and the store's names in `TestImageIdentityRoundTrip`,
  whose reported ID the push feature checks.
  The cache feature: a Given step damages the cache, and the scenario sees it rebuild or recover.
- [ ] **Prune and store roots.**
  `TestPrune`, `TestPruneRefusedWhileInUse`, `TestExplicitStoreRoots`, `TestDefaultStore`,
  `TestPruneHandlerWithoutRoot`.
  A prune feature, and the configuration feature for the roots; prune runs on private stores.
- [ ] **Traces.**
  `TestTraceBuild`, `TestTracePush`, `TestTracePull`, `TestTraceFailure`, `TestTraceDestinations`, `TestFinishTrace`,
  `TestTraceMistakes`, `TestTracePerfetto`, `TestSecretsNotInTrace`.
  A trace feature that reads the file `--trace` writes; Perfetto import still skips without `trace_processor_shell`.
- [ ] **Interruption and failure logs.**
  `TestInterrupted`, `TestCancelLoad`, `TestCancelConfigure`, `TestFailedStepLog`, `TestFailedOutputLog`.
  A feature for interrupting a build and for what a failure reports.
- [ ] **Failures to write output.**
  `TestOutputWriteFailure`, `TestBuildHandlerWithoutRoot`, `TestWatchJobWriteFailure`.
  Scenarios with a step that makes standard output fail.
- [ ] **How a user sees remote work and its failures.**
  `TestRemoteBuild`, `TestRemoteMistakes`, `TestCoordinatorCommand`, `TestCoordinatorWithWorker`,
  `TestUsesContainerStore`, `TestCheckRegistryHost`, `TestCoordinatorFlagMistakes`.
  A few scenarios with a coordinator and worker in process: a build, a mistake, and what an unreachable or failing
  coordinator reports. The failure modes themselves stay below.

## Keep as Go tests, confirming each one's reason

- [ ] **Remote failure modes.**
  `internal/coord`, `internal/remote`, `internal/worker` and `internal/transfer`'s retries and read-backs:
  restarts, lost workers, fencing, expiry, retries, outages and resumed watches.
- [ ] **Internal contracts.**
  `cachetest.Content` and `cachetest.Records` (`TestContentStore`, `TestRecordStore`, `TestContentStoreContract`),
  `apitest.Contract` (`TestContract`, `TestContractJSON`, `TestContractHTTP`), `TestErrorsCross`,
  `TestFailureCrosses`, `TestWorkerFailureCrosses`, `TestReleaseCrosses`, `TestGraphValidate`, `TestGraphRoundTrip`,
  `TestGraphExamples` and `internal/api`.
- [ ] **The test harness.**
  `internal/testenv`, and the features package's own tests of its matchers and image reader.
- [ ] **Performance.**
  `TestWalkTreeReadsOnlyWhatItNeeds`, `TestStoreBatchesMissing`, `TestReadManyBatches`, `TestSpanBatches`.

## Decide

- [ ] **Mechanisms beneath a feature.**
  `internal/sched` (action keys, sharing, job limits, the scheduler's reuse and regeneration),
  `internal/fileset` (patterns, tree walks, edits during capture), `internal/cache` (logs, locks, generations),
  `internal/finish`'s delivery from separate stores, `internal/run`'s runner and its puller (`TestPullerPinnedIndex`,
  `TestMaterializeFromRegistry`, whose refusals of a mismatched manifest, a wrong image ID or a location that can't
  be parsed the pull feature doesn't reach), and `internal/tracing`.
  Each either shows in what a user sees, and moves into the feature it serves,
  or is an internal contract, and says so; anything that is neither needs a reason added to the rule.
