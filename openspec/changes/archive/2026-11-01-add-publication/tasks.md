# Tasks

## 1. Uploading by digest and tagging, in `internal/transfer`

- [x] 1.1 Add `transfer.Upload`, which copies an image or index to a repository through
  `docker.NewReferenceUnknownDigest` with `Retries` and returns the manifest written,
  and `transfer.Tag`, which puts that manifest to `repository:tag` and reads the tag back as `Push` does;
  verify with Go tests against an in-process registry that an upload leaves no tag, that a tag names the uploaded
  manifest for an image and for an index, and that a tag whose read-back differs fails naming it
- [x] 1.2 Update `internal/transfer/doc.go` for the two new entry points and check `go doc ./internal/transfer`

## 2. The `publication()` rule, in `internal/load`

- [x] 2.1 Add the `publication` declaration kind through `declarer`, with `name`, `checks?`, `images` and `params`,
  and its `publicationSpec` of check references and (repository, image reference) pairs in dictionary order,
  accepting functions of `ctx` for `checks` and `images`; verify with `go test -short ./internal/load`
- [x] 2.2 Check at resolution: a check is a task, an image is a pull, layer or index, a repository is a lowercase
  name with no tag or digest, `images` is nonempty, and no other declaration may reference a publication,
  each reported with `file:line:col`; verify each message in the `publication` feature (group 6)
- [x] 2.3 Add `WantPublication` and `LoadPublication`, which return the graph, with the checks then the images as
  roots in the publication's configuration, and the plan of (root index, full repository reference);
  `--set` names the publication's parameters, and a target that isn't a publication is refused, naming its kind;
  verify roots, plan and the refusal with a Go test of `internal/load`
- [x] 2.4 Make `WantBuild` expand a named publication into the same roots, and refuse a publication for
  `WantImage` and `WantFiles` with a message naming the command that takes it; verify with `go test -short`
- [x] 2.5 Document `publication()` in `docs/reference/build.md`, including keying by repository, params and
  functions of `ctx`, and that it can't be depended on; update `internal/load/doc.go`

## 3. The publish job, in `internal/api` and `run.Local`

- [x] 3.1 Add `api.PublishRequest` to `api.Request`, and `api.Published` with per-destination entries to
  `api.Outcome`, with doc comments stating that `Published` accompanies a `Failure` from the tagging phase;
  verify `apitest.JSON` round-trips them
- [x] 3.2 Implement the publish job in `run.Local`: upload each (result, repository) in turn, then, only if all
  succeeded, tag each in turn, attempting every tag and reporting each; verify with new `apitest.Contract` cases
  for success, an upload failure that leaves no tag, and a tagging failure that reports what was written
- [x] 3.3 Update `internal/api/doc.go` and `internal/run/doc.go`; check `go doc` for both

## 4. Publishing on a coordinator, in `internal/remote` and `internal/coord`

- [x] 4.1 Give `remote.PushWork` a mode, upload or tag, carrying the manifests and tags a tag attempt needs,
  and run both in the worker; verify with `go test ./internal/worker` and `./internal/remote`
- [x] 4.2 Serve `PublishRequest` through `remote.Client` and `remote.Server`; verify `TestContractHTTP` passes the
  new contract cases
- [x] 4.3 Add `submitPublish` to the coordinator: one job, upload attempts fanned out across workers and retried on
  worker loss up to `--retries`, then one tag attempt that is never retried; verify with Go tests in `internal/coord`
  of concurrent uploads, a worker lost during an upload, and a worker lost while tagging
- [x] 4.4 Journal a publication as `record.Publish`, counted among the kinds, and fail an unfinished one on restart
  saying its tags may or may not have been written; verify with a coordinator restart test
- [x] 4.5 Update `internal/coord/doc.go`, `internal/remote/doc.go`, `internal/worker/doc.go`, the coordinator section
  of `docs/reference/cli.md` and `.agents/kb/architecture.md` for publish jobs and that only workers contact
  destination registries, and `.agents/kb/remote-environment.md` for the registries `publish --push-from=client`
  must reach, beside `push --push-from=client`

## 5. The `publish` command, in `internal/cli` and `internal/finish`

- [x] 5.1 Add `finish.Publish`, the client's two-phase publish from a result's locations or the content store,
  one upload then one tag at a time; verify with a Go test of `internal/finish` against an in-process registry
- [x] 5.2 Add `vamos publish <publication> --tag ...` with a required, repeatable `--tag` checked before loading,
  `--push-from` and the common build flags; run one graph job, hold it, then submit one `PublishRequest` or call
  `finish.Publish`, printing one `Image built!` or `Index built!` line per repository and tag, and a span per
  upload and tag in a trace; verify with `go build -o vamos .` and `./vamos publish --help`
- [x] 5.3 Document `vamos publish` in `docs/reference/cli.md`, its row in the command table, `--tag`, the
  two phases, what a failure while tagging reports, and the untagged-manifest garbage-collection window;
  update `internal/cli/doc.go` and `internal/finish/doc.go`
- [x] 5.4 Bring the rest of the user guide in line: in `docs/reference/cache.md`, that `publish` shares the cache
  and doesn't force cached targets to run again, that a prune is refused while a publication is open,
  and `publish` among the commands taking `--trace`; a row for publishing an index in the index delivery table of
  `docs/reference/configuration.md`; and releasing in the CLI bullet of the tutorial's "Where to go next"
- [x] 5.5 Describe the two-phase delivery in `.agents/kb/container-builds.md`, beside "Delivery is a separate commit",
  and add `finish.Publish`, `transfer.Upload` and `transfer.Tag` to its transport owners

## 6. The `publication` feature, in `internal/features`

- [x] 6.1 Add the steps the feature needs, reusing `GivenARegistry` and the image steps: `WhenIPublish` with
  `taggedAs` and the existing push options, a check that a repository has exactly the tags expected, and one that
  several destinations name the same manifest; document each in the step files by subject
- [x] 6.2 Write `internal/features/publication_test.go` with a background and a `TestPublication_<Rule>` per
  requirement of `specs/publication/spec.md`, except the coordinator scenarios (6.3) and the trace requirement (6.4),
  with its mistakes from a background that needs no buildah;
  verify with `go test ./internal/features -run TestPublication_ -v`
- [x] 6.3 Add the coordinator scenarios, publishing through workers and a busy coordinator refusing before
  pushing, using the steps of `steps_remote_test.go`; verify with `go test ./internal/features -run TestPublication_`
- [x] 6.4 Add `TestTrace_ShowsPublishing` to `internal/features/trace_test.go`, beside `TestTrace_ShowsDelivery`,
  for the requirement "The trace shows publishing": the publish job within the client's span and on its track,
  uploads then tags, the copying process for the executor, the client and workers, and a rerun skipping uploaded
  blobs, adding to `steps_trace_test.go` a step that every span matching one name ends before any matching another
  starts; verify with `go test ./internal/features -run TestTrace_ShowsPublishing -v`
- [x] 6.5 Mention the `publication` feature in the behavior-specifications list in `.agents/kb/testing.md`

## 7. Integration

- [x] 7.1 Run `go build -o vamos .`, `go vet ./...`, `gofmt -l .` (prints nothing), `go test -short ./...` and the
  full `go test ./...` in the devcontainer, which includes `TestImportRules` for the new imports
- [x] 7.2 Add a publication to the Strawberry React example, publishing `//ui:server` and `//backend:server`,
  update its README and its entry in `examples/README.md`, and publish it by hand to a local registry with and
  without `--push-from=client`, as `.agents/kb/smoke-checks.md` describes for destinations;
  record that manual check in `smoke-checks.md`, beside the note that `push` has none
- [x] 7.3 Check that the documentation is in sync, as design decision 7 requires: search `docs/`, `README.md`,
  `examples/README.md` and `.agents/kb/` for lists of delivery commands and mentions of `push`,
  and confirm each covers `publish` where it applies; check the wrapping rules of `.agents/kb/documentation.md`,
  every local link, and `go doc` for each package whose `doc.go` changed
- [x] 7.4 Run `openspec validate add-publication --strict` and fix what it reports
