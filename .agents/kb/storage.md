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
| Action key | `run.action`, `scheduler.inputs` | Reusing equivalent target work |
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
Each layer's and task's result records a `cache.Provenance`, the executor's platform, isolation, capabilities
and version, which is no part of the action key; a resolve's result isn't published, so it records none.
Action identity is deliberately independent of graph position and target naming.
Changes to execution semantics must review [action.go](../../internal/run/action.go),
`scheduler.inputs` and `scheduler.lookup` in [scheduler.go](../../internal/run/scheduler.go),
and `localRunner.check` in [runner.go](../../internal/run/runner.go).
`actionVersion` invalidates results built under old semantics;
`graph.Version` addresses a different compatibility boundary.

Reproducible layer metadata spans both stores: trees omit times, but action results carry an epoch.
Review `scheduler.epoch`, `commitLayer` and `actionStep.Epoch` together.
`--timestamp` controls newly executed work; it does not invalidate a cached target.
`TestTaskEpochInKey` and the timestamp cases
in [build_cache_test.go](../../internal/cli/build_cache_test.go) guard this distinction.

Tag freshness is separate from action reuse.
`cache.TagRecord` and `puller` in [pull.go](../../internal/run/pull.go) own resolution across builds,
and `scheduler.resolve` pins each tag's index within a job, so every platform's image comes from one index;
`--rebuild` is not a blanket rebuild of every dependency.
Pruning removes action results, blobs and cached layer images while retaining tag records and pulled bases,
which lose their cache-repository name until the next resolve gives it back.
Consult `Local.prune` in run and the stores' `ClearContent` and `ClearResults` before changing this policy,
and `CacheRepository` before changing image naming.
Checking local images by manifest digest before fetching prevents unnecessary registry requests.

Pruning excludes every other use of the directory through its lock; see [concurrency-model.md](concurrency-model.md).
Shared-cache test isolation rules are in [testing.md](testing.md).
