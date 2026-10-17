# Storage, identity and reuse

Two stores participate in a build.
[cache](../../internal/cache/doc.go) holds graph blobs, source/output trees, logs, action results and tag records;
the executor's container store holds images.
`run.CacheDir` locates the build cache beside the configured container store (normally `/var/lib/containers/vamos`),
so persisting `/var/lib/containers` keeps both.
Cache metadata alone cannot prove an image or tree is still available.

Do not conflate these identities:

| Identity | Owner | Used for |
| --- | --- | --- |
| Configured target | `load.confLabel` | Deduplicating dependency configurations |
| Graph blob | `graph.Encode`, `run.Request.Graph` | Transferring one build plan |
| Action key | `run.action`, `targetRunner.inputs` | Reusing equivalent target work |
| File tree | `cache.Tree` | Reusing file content and metadata independently of times |
| Image ID / manifest digest | `run.Result`, `run.tagRecord` | Stored image content / registry resolution |

Action identity is deliberately independent of graph position and target naming.
Changes to execution semantics must review [action.go](../../internal/run/action.go),
`targetRunner.inputs` and `targetRunner.checkResult` in [runner.go](../../internal/run/runner.go).
`actionVersion` invalidates results built under old semantics;
`graph.Version` addresses a different compatibility boundary.

Reproducible layer metadata spans both stores: trees omit times, but action results carry an epoch.
Review `targetRunner.epoch`, `commitLayer` and `actionStep.Epoch` together.
`--timestamp` controls newly executed work; it does not invalidate a cached target.
`TestTaskEpochInKey` and the timestamp cases
in [build_cache_test.go](../../internal/cli/build_cache_test.go) guard this distinction.

Tag freshness is separate from action reuse.
`tagRecord` and `puller` in [pull.go](../../internal/run/pull.go) own resolution across platforms and builds;
`--rebuild` is not a blanket rebuild of every dependency.
Pruning removes action results, blobs and cached layer images while retaining tag records and pulled bases.
Consult `run.Prune` and `cache.Clear` before changing this policy, and `CacheRepository` before changing image naming.
Checking local images by manifest digest before fetching prevents unnecessary registry requests.

Concurrent publication and destructive pruning have different guarantees;
see [concurrency-model.md](concurrency-model.md).
Shared-cache test isolation rules are in [testing.md](testing.md).
