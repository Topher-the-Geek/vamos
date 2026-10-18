# Storage, identity and reuse

Three stores participate in a build.
The content store holds graph blobs, source/output trees and logs, and the record store holds action results
and tag records; both are interfaces in [cache](../../internal/cache/doc.go), with `cache.Directory` on disk
and `cache.Memory` for tests.
The executor's container store holds images.
`run.CacheDir` locates the directory beside the executor's container store (normally `/var/lib/containers/vamos`),
so persisting `/var/lib/containers` keeps both; nothing outside run and cache names that directory,
and a client gets its content store from the executor it builds with.
Record metadata alone cannot prove an image or tree is still available.
Content is verified as it is read: a task whose cached files are found corrupt runs again,
a glob whose files are corrupt fails the build, since only the client can capture them again,
and a result whose log is corrupt is a miss; `TestCacheCorruption` covers each.

Do not conflate these identities:

| Identity | Owner | Used for |
| --- | --- | --- |
| Configured target | `load.confLabel` | Deduplicating dependency configurations |
| Graph blob | `graph.Encode`, `api.GraphRequest.Graph` | Transferring one build plan |
| Action key | `sched.Action`, `Scheduler.inputs` | Reusing equivalent target work |
| File tree | `cache.Tree` | Reusing file content and metadata independently of times |
| Image ID / manifest digest | `api.Result`, `cache.TagRecord` | Stored image content / registry resolution |

An image's ID is the digest of its config, which a push and a pull preserve; `TestImageIdentityRoundTrip` holds
it portable.
Run locates every image, built or pulled, through its name in the cache repository, `CacheRepository:<image ID>`,
so the container store's own IDs stay inside run; see `imageIdentity`, `locateImage`, `cacheName` and `nameImage`
in [pull.go](../../internal/run/pull.go).
Anything that changes the store can move a name, so `locateImage` checks that the image a name names has that ID,
and that its layers are those its config lists, then uses it by the store's ID it found;
a cached result whose name doesn't check out is a miss.
The layer check refuses images that storage.conf's `insecure_allow_unpredictable_image_contents` lets a partial
pull keep unverified, whose config wouldn't identify their files; `TestUnverifiedLayers` and `TestMovedCacheName`
hold both checks, and `TestImageStoreIDs` holds store IDs that differ from image IDs.
A resolve validates what a tag resolved to before anything uses it: the store's image is the manifest chosen,
and it is for the platform asked.
Each layer's and task's result records a `cache.Provenance`, the executor's platform, isolation, capabilities,
version and whether its commands ran emulated, which is no part of the action key;
a resolve's result isn't published, so it records none.
A result names its image by ID and lists its `cache.Location`s separately: the store's name locally,
for a pulled base the registry reference by digest too, at the pull-through cache when the coordinator maps its
registry to one, and for a layer built on a worker its reference in the cache repository, tagged by image ID,
which the worker pushes before reporting; `localRunner.materialize` fetches a missing image from a registry location,
and `TestMaterializeFromRegistry` holds that. The client delivers from registry locations only over a coordinator.
Only `sched.Sharing` writes the record store, once an attempt has published its artifact;
the runner returns a `sched.Report` and the puller the tag record to write, so that no record names an artifact that
isn't there, and the latest write wins.
Action identity is deliberately independent of graph position and target naming.
Changes to execution semantics must review [action.go](../../internal/sched/action.go),
`Scheduler.inputs` and `Scheduler.lookup` in [scheduler.go](../../internal/sched/scheduler.go),
and `localRunner.Check` in [runner.go](../../internal/run/runner.go).
`sched.ActionVersion` invalidates results built under old semantics;
`graph.Version` addresses a different compatibility boundary.

Reproducible layer metadata spans both stores: trees omit times, but action results carry an epoch.
Review `Scheduler.epoch`, `commitLayer` and `sched.Step.Epoch` together.
`--timestamp` controls newly executed work; it does not invalidate a cached target.
`TestTaskEpochInKey` and the timestamp cases
in [build_cache_test.go](../../internal/cli/build_cache_test.go) guard this distinction.

Tag freshness is separate from action reuse.
`cache.TagRecord` and `puller` in [pull.go](../../internal/run/pull.go) own resolution across builds,
and `Scheduler.resolve` pins each tag's index within a job, so every platform's image comes from one index;
`--rebuild` is not a blanket rebuild of every dependency.
Each platform's `cache.TagInstance` records the image's ID, platform, epoch and locations once pulled,
so `Scheduler.resolve` serves a cached pull from the record and the checker alone, asking the runner nothing;
only a missing or incomplete instance, an image the checker can't find, or `--rebuild` reaches the puller,
which is given the record in its request and reads none itself.
`TestSchedulerCachedResolve` and `TestTagRecordLegacyInstances` hold this, and `TestPullTag` still requires that
nothing reaches a registry once the record and images are cached.
Pruning removes action results, blobs and cached layer images while retaining tag records and pulled bases,
which lose their cache-repository name until the next resolve gives it back.
A coordinator's prune has no lock over its clients, so it fences by generation: `RecordStore.Generation` counts
the clears, `api.Capabilities.Generation` reports one more than that, so that zero still means unchecked,
the client reads it before capturing sources and sends it in `api.GraphRequest.Generation`,
and a build of another generation fails with a message to run the command again;
the build checks it once admitted, when no prune can start, so that none slips between the check and the start.
A report from an attempt that started in an earlier generation is refused too (`pool.Report`).
The coordinator's prune also deletes every tag of the cache repository, through `coord.Registry.ClearRepository`,
after the blobs and the records, so that a failure there leaves only images nothing names; tag records and the
pull-through cache are kept, as locally.
The blobs go before the records, since clearing the records advances the generation: a client that reads the new
generation then uploads to a content store that nothing clears any more.
`TestGenerationFencing`, `TestPruneOrder` and `TestPruneClearsRepository` hold this.
Consult `Local.prune` in run and the stores' `ClearContent` and `ClearResults` before changing this policy,
and `CacheRepository` before changing image naming.
Checking local images by manifest digest before fetching prevents unnecessary registry requests.

Pruning excludes every other use of the directory through its lock; see [concurrency-model.md](concurrency-model.md).
Shared-cache test isolation rules are in [testing.md](testing.md).
