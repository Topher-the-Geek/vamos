# Tasks

## 1. The lockfile, in `internal/load` and `internal/graph`

- [x] 1.1 Add the lockfile model: `version` 1 and `pulls` keyed by normalized `image:tag`, each with `tag` and
  `digest`; read it from the workspace root that load already finds, ignoring unknown fields, refusing a greater
  version, and turning a JSON syntax error's byte offset into a line and column; write it with `MarshalIndent`,
  two spaces and a trailing newline, to a temporary file renamed over it after re-reading it;
  verify with `go test -short ./internal/load` that writing is byte-stable and that two writers keep both pins
- [x] 1.2 Give `graph.Pull` a `Digest`, set by load from the pull's entry, and raise `graph.Version` to 4;
  verify with `go test -short ./internal/graph ./internal/load`
- [x] 1.3 Add a Given step that writes `vamos.lock`, and the scenarios *A malformed lockfile is refused* and
  *A lockfile of a newer version is refused* in a new `internal/features/update_test.go`, from `newWorld`;
  verify they pass with `go test -short ./internal/features`
- [x] 1.4 Update `internal/load/doc.go` and `internal/graph/doc.go` for the lockfile and `Pull.Digest`;
  check with `go doc ./internal/load` and `go doc ./internal/graph`

## 2. Pinned pulls, in `internal/sched` and `internal/run`

- [x] 2.1 Seed the resolve action's `Index` from `Pull.Digest`, so that a pinned pull selects its platform's image
  from that index, or checks a single image's platform, and neither reads nor writes a tag record;
  verify with `go test ./internal/sched ./internal/run`
- [x] 2.2 Fail a pinned digest the registry doesn't have, naming the image, tag and digest and suggesting
  `vamos update <pull>`; verify with the scenario *A pinned digest that is gone says to update*
- [x] 2.3 Add the scenarios *A moved tag keeps the pinned image* and *Another platform gets the pinned index's
  image* to `pull_test.go`, on `GivenARegistry`; verify with `go test ./internal/features -run TestPull`
- [x] 2.4 Document pinned pulls in the `pull()` section of `docs/reference/build.md`, and update
  `internal/sched/doc.go` and `internal/run/doc.go`; check `go doc` for both

## 3. The registry seam, in `internal/cli`

- [x] 3.1 Add `Registry`, with `Tags` and `Digest`, to `cli.Config`, defaulting to `docker.GetRepositoryTags` and
  `docker.GetDigest` with the client's `SystemContext`, which reach the image's own registry, never a mirror;
  verify with `go build ./... && go vet ./internal/cli`
- [x] 3.2 Give the features world a fake `Registry`, filled by `GivenTheRegistryListsTags(repo, tags...)`, with a
  digest per tag, and a way to refuse listing; document both steps in `.agents/kb/testing.md`'s list of step files;
  verify with `go test -short ./internal/features`
- [x] 3.3 Update `internal/cli/doc.go` for the seam; check with `go doc ./internal/cli`

## 4. `vamos update` and resolving to the newest covered tag

- [x] 4.1 Add the tag model: parse `[v]N[.N[.N]][-suffix]`, split a leading `alpha`, `beta`, `rc` or `pre` marker
  off the suffix as a prerelease, and choose the newest stable tag covered by the pulled one, by its leading numbers,
  within the same `v` prefix and suffix, or the tag itself; specified only through the scenarios of 4.3
- [x] 4.2 Add the `update [pull...]` command: load every pull, or those named, refusing a target that isn't a pull
  by its kind; list each repository's tags once, resolve each tag, ask its resolved tag's digest, rewrite the
  lockfile, remove entries no pull uses on a full update, and report each entry added, changed or removed with
  tags and digests before and after
- [x] 4.3 Add the rules *Tags resolve to the newest tag they cover* (the spec's table), and *A moved tag is updated
  and reported*, *Naming a pull updates only its pin*, *An entry no pull uses is removed*, *Naming a target that
  isn't a pull is refused*, *Updating writes one entry per pulled tag*, *Adding a pin changes only its lines* and
  *The lockfile lives at the workspace root*, over the fake registry from `newWorld`;
  verify with `go test -short ./internal/features -run TestUpdate`
- [x] 4.4 Document `vamos update` in `docs/reference/cli.md`'s command table and its own section, with the lockfile's
  format and that it is committed, and in `docs/reference/cache.md`'s refreshing results

## 5. Reporting newer releases, prereleases and incomparable tags

- [x] 5.1 Report, per pull, the newest stable newer minor or patch release within its major, and the newest stable
  newer major, each where one exists; note the newest prerelease newer than the resolved tag, and a tag that isn't
  a version
- [x] 5.2 Add the rules *Newer releases are reported* (the spec's table), *A newer prerelease is noted* and *A tag
  that isn't a version is noted*; verify with `go test -short ./internal/features -run TestUpdate`
- [x] 5.3 Document the report and its notes in `docs/reference/cli.md`'s `update` section, with an example output

## 6. `vamos update --dry-run`

- [x] 6.1 Add `--dry-run`: compute the lockfile `update` would write, report as `update` does, write nothing, and
  fail with `vamos.lock would change; run vamos update` when it differs from the file on disk, or there is none
- [x] 6.2 Add the rule's scenarios: *A dry run fails when a pin would change*, *A dry run of current pins succeeds*
  and *Newer releases alone don't fail a dry run*; verify with `go test -short ./internal/features -run TestUpdate`
- [x] 6.3 Document `--dry-run` and its exit status, with a CI example, in `docs/reference/cli.md`

## 7. Tags at their upstream registry

- [x] 7.1 Fall back, when a registry refuses to list tags, to the digest the tag itself names, with a note;
  fail an unreachable registry naming the image and the cause, leaving the lockfile unchanged
- [x] 7.2 Add a registry step that lists tags over several pages, and one that refuses listing, to the real
  scenario registry; add the scenarios *A pull-through cache is not asked about tags*, *An unreachable registry
  fails, saying why*, *A registry that won't list tags pins the tag itself* and *Every page of a long tag list is
  read*, over the default `Registry`; verify with `go test ./internal/features -run TestUpdate`
- [x] 7.3 Say in `docs/README.md` and `docs/reference/cli.md`'s coordinator section that `update` and recording pins
  reach upstream registries from the client, and add that to `.agents/kb/remote-environment.md`'s list of what a
  client must reach

## 8. The `--pins` policy

- [x] 8.1 Add `--pins=record|resolve|require` to `build`, `push`, `publish`, `load`, `save` and `export`, defaulting
  to `VAMOS_PINS` where set, else `record`, and refusing another value by naming the policies
- [x] 8.2 Under `record`, resolve each unpinned pull on the client as `update` does and add its entry before the
  graph is submitted; under `resolve`, leave `Digest` empty so the executor resolves the tag through its tag records;
  under `require`, fail naming every unpinned pull and suggesting `vamos update`, before anything runs
- [x] 8.3 Add the rule's scenarios: *Recording adds the missing entry*, *Resolving keeps the lockfile as it is*,
  *Requiring pins fails naming every unpinned pull*, *The flag outranks the environment* and *An unknown policy is
  refused*; verify with `go test ./internal/features -run TestUpdate`
- [x] 8.4 Move the existing pull scenarios that specify tag records, in `TestPull_ResolvedTagsAreReused`, to
  `--pins=resolve`, and confirm the rest pass under the default `record`; verify with `go test ./internal/features`
- [x] 8.5 Document `--pins` and `VAMOS_PINS` in `docs/reference/cli.md`'s flags table, and what the lockfile and the
  cache's tag records each hold in `docs/reference/cache.md` and `.agents/kb/storage.md`

## 9. `--rebuild` stops resolving pulls

- [x] 9.1 Remove `againTags` and `Request.Again`; make a rebuilt pull report `pulls are refreshed by vamos update`
  and complete from its pin or record; verify with `go test ./internal/sched ./internal/run`
- [x] 9.2 Replace the `--rebuild` scenarios of `TestPull_ResolvedTagsAreReused` with *Rebuilding a pinned pull
  points to update*, and make the prune scenario refresh through `vamos update`;
  verify with `go test ./internal/features -run TestPull`
- [x] 9.3 Update `docs/reference/cli.md`'s `--rebuild` text and its troubleshooting row for a moved tag, and
  `docs/reference/cache.md`'s refreshing results, to point to `vamos update`

## 10. Integration

- [x] 10.1 Run `go build -o vamos . && go vet ./... && gofmt -l . && go test ./...` with nothing printed by gofmt
  and every test passing
- [x] 10.2 Run `openspec validate pin-pulls --strict` and check every spec scenario has a matching scenario in
  `internal/features`
- [x] 10.3 Run the relevant checks of `.agents/kb/smoke-checks.md` with the built binary: update an example project,
  commit its `vamos.lock`, build it with `VAMOS_PINS=require`, and confirm `vamos update --dry-run` exits 0, then 1
  after editing a digest in the lockfile
