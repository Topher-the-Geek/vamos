# Proposal

## Why

Releasing several images today takes one `vamos push` per image, and in CI each is usually its own workflow step
in its own container: each loads the project again, connects to the executor again, checks the cache again and
pushes alone, with nothing tying the pushes to the checks that should gate them.
A release is one decision, that these tasks passed and these images go out under this tag,
and vamos should let a project declare it once and carry it out in one command,
publishing nothing unless everything it depends on succeeded,
and leaving no tag half-published when an upload fails.

## What Changes

- A new BUILD rule, `publication()`, declares a release:
  `checks`, the tasks that must succeed, and `images`, a dictionary from repository, without a tag,
  to the pull, layer or index target published there.
  It takes `params`, and its repositories, like any other attribute, can be functions of `ctx`,
  so that a parameter can choose between, for example, a staging and a production registry.
  Keying by repository lets one target go to several repositories,
  as a release mirrored to more than one registry needs, such as `ghcr.io` and Docker Hub.
  A publication is a root only: no target can depend on it, and it is never cached.
- A new command, `vamos publish <publication> --tag <tag>...`, builds every check and image in one graph run,
  and only if all of them succeed publishes every image to its repository under every tag given.
  `--tag` is required and repeatable; `--push-from` and `--set` work as they do for `push`.
  Repeating it serves the common way of tagging a release under several names at once:
  an exact version that never moves, such as `1.4.2`, beside tags that follow each release, such as `1.4`, `1`
  and `latest`, and perhaps the commit.
  Writing them all in one command keeps them inside the same guarantee:
  a separate step per tag, or a registry tool run afterwards, would add back the CI steps this change removes,
  and could leave `latest` naming the previous release after `1.4.2` was published.
- Publishing has two phases.
  The first uploads every image's blobs and manifests to its repository by digest, with no tag,
  so that a failure leaves nothing visible under any tag.
  Only when every upload succeeded does the second write the tags, then read each back from the registry.
  A failure in the second phase still attempts every tag, reports which were written, and fails the command,
  so that running it again finishes the job cheaply, with every blob already uploaded.
- Where the pushes run follows `--push-from` and the executor.
  The local executor and the client upload one repository at a time, to spare the host's network interface;
  a coordinator runs the uploads as attempts on its workers at once, and the tags as one attempt after them,
  so that the coordinator itself still never contacts a destination registry.
  A coordinator admits a publication as one job.
- `vamos build <publication>` builds its checks and images and publishes nothing, as a dry run.
- `vamos push <target> <image>` is unchanged.

## Capabilities

### New Capabilities

- `publication`: declaring a release in a BUILD file and carrying it out with `vamos publish`:
  what it gates on, what it publishes where and under which tags, how it uploads before it tags,
  where the pushes run, and what it reports when part of it fails.

### Modified Capabilities

None: there are no specifications under `openspec/specs/` yet, and no existing behavior changes.

## Impact

- BUILD language, in `internal/load`: a `publication` declaration kind and its template,
  configured as a target with params; resolving it for `publish`, and as a set of roots for `build`.
  The graph format is unchanged: a publication becomes ordinary roots, and its repositories stay with the client.
- Executor API, in `internal/api`: a publish request and its outcome, beside push,
  implemented by `run.Local`, served through `internal/remote`, and run by `internal/coord` as worker attempts
  that `internal/worker` carries out, with the coordinator's journal recording it.
  `apitest.Contract` covers it for every executor.
- Image transfer, in `internal/transfer`: uploading by digest and tagging what was uploaded,
  reusing the existing copy, retry and read-back.
- Client delivery, in `internal/finish` and `internal/cli`: the `publish` command, its flags and its report,
  and publishing from the client with `--push-from=client`.
- Documentation, kept in sync as part of done.
  The user guide: the BUILD and CLI references; the cache reference, where it lists the commands that share the
  cache, take `--trace` and hold off a prune; the configuration reference's index delivery table;
  the tutorial's next steps; and the examples index and the Strawberry React README.
  The knowledge base: `architecture.md`; `container-builds.md`, for the two-phase delivery;
  `remote-environment.md`, for what `publish --push-from=client` must reach; `smoke-checks.md`,
  for the manual publish check; and `testing.md`.
  And the `doc.go` of each package touched.
- Behavior specifications: a new `publication` feature in `internal/features`, with the steps it needs.
