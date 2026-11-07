---
name: update-dependencies
description: Update every pinned dependency in the repository, in two phases around a devcontainer rebuild - first the devcontainer's own tools, then, in a fresh session in the rebuilt container, Go modules, Node and Python packages and lockfiles, base image tags and vamos.lock pins, GitHub Actions and everything else. Use when the user asks to update, bump, upgrade or refresh dependencies, versions, lockfiles or image tags, even if they don't name this skill.
---

# Update dependencies

The devcontainer supplies the toolchains that every other update is checked with:
Go (with `GOTOOLCHAIN=local`, so `go.mod` can't ask for a newer Go than it has), Node.js, pnpm, Python and uv,
openspec, kind and kubectl, actionlint and kubeconform.
So the devcontainer is updated first, the user rebuilds it, and everything else is updated in the rebuilt container.

A rebuild ends this session and keeps nothing but the repository, which is bind-mounted from the host.
Carry nothing across in the scratchpad or memory: whatever phase 2 needs is committed, or is re-derived.

## Which phase

Every run starts with phase 1, which is idempotent.
Read `git log -5` first: a commit of an earlier run records what it took and which majors the user declined.
In a container that is already current it changes nothing, and the run continues to phase 2 in the same session.
So the user invokes this skill the same way both times, and a session that was cut short can simply be restarted.

## Policy

- **Two weeks old.** Take only releases published at least 14 days ago, as the repo's pnpm, npm and uv settings do.
  A newer release is fine only for a security fix the user asks for.
- **Same major, freely; new major, ask.** Move within a major version without asking.
  Collect every available new major version first: base OS, language runtime, image tag, package or action.
  Then ask the user once per phase, in a single question listing them, which ones to take.
  Fedora's next release, Node's next LTS and a new Go minor (1.26 → 1.27) count as majors here.
- **Keep coherence groups together** (below): a version that appears in several files changes in all of them or none.
- **Commit at the end of each phase.** If the branch is `main`, first create `update-dependencies-<YYYY-MM-DD>`.
  End commit messages with the attribution lines the session asks for.
- Keep downloads in the session scratchpad, never in the repo.

## Coherence groups

| Version | Must agree across |
| --- | --- |
| Go toolchain | Fedora `golang` in the devcontainer ≥ the `go` line of [go.mod](../../../go.mod); [Dockerfile](../../../Dockerfile)'s `golang:1.NN-bookworm` has go.mod's minor |
| Go in the example | [examples/golang-cli/go.mod](../../../examples/golang-cli/go.mod)'s `go` line, its [BUILD](../../../examples/golang-cli/BUILD) tag `1.NN-alpine`, [vamos.lock](../../../examples/golang-cli/vamos.lock), its README's "`1.NN.x-alpine`", and the comment on line 62 of [smoke-checks.md](../../../.agents/kb/smoke-checks.md) |
| Node.js major | Fedora `nodejs24`/`nodejs24-npm`; `node:24-alpine` in the BUILD files of typescript-mcp, typescript-ui and strawberry-react/ui; [docs/user/tutorial.md](../../../docs/user/tutorial.md) (its BUILD walkthrough and mermaid diagram); the example READMEs' mermaid diagrams; `engines` and `@types/node` in typescript-mcp's package.json; `engines` in docs/site's package.json |
| pnpm | Fedora `pnpm`; `packageManager` in [docs/site/package.json](../../../docs/site/package.json) and [examples/typescript-ui/package.json](../../../examples/typescript-ui/package.json); `pnpm@X` in [examples/typescript-ui/BUILD](../../../examples/typescript-ui/BUILD), [examples/strawberry-react/ui/BUILD](../../../examples/strawberry-react/ui/BUILD) and tutorial.md |
| Python minor | Fedora `python3`; `python:3.NN` / `3.NN-slim` in the fastmcp and strawberry-react/backend BUILD files and vamos.lock files; `--python-version 3.NN` in each lockfile header and README command; `target-version`/`pythonVersion` in [examples/fastmcp/pyproject.toml](../../../examples/fastmcp/pyproject.toml); `uv venv --python 3.NN` in the strawberry-react README |
| kubectl | Fedora `kubernetes1.NN-client` matches the Kubernetes minor of the default node image of the `kind` that Fedora ships |
| Registry image | `registry:3` in [remote/compose.yaml](../../../remote/compose.yaml) (twice), [remote/helm/vamos/values.yaml](../../../remote/helm/vamos/values.yaml) (twice) and smoke-checks.md |
| glibc floor | Dockerfile's build stage stays on Debian **bookworm** (glibc 2.36), since `glibc_minimum=2.34` in [install.sh](../../../install.sh) and the tutorial promise Ubuntu 24.04; moving to trixie raises that floor, so it is a major and needs the user's yes |
| OpenSpec | the CLI version in the devcontainer and `generatedBy` in the generated skills and commands |

## Phase 1: the devcontainer

The only file is [.devcontainer/Dockerfile](../../../.devcontainer/Dockerfile).
[devcontainer.json](../../../.devcontainer/devcontainer.json) pins nothing: its VS Code extensions update themselves.

### 1. Find what is new

- **Fedora base**, `FROM fedora:NN`: the newest *released* Fedora, from
  `curl -fsSL https://fedoraproject.org/releases.json | jq -r '.[].version' | sort -un`, ignoring Beta entries.
  A new release is a major. Before proposing it, check that every package in the `dnf install` list exists there:
  `dnf -q --releasever=NN repoquery --latest-limit=1 --qf '%{name} %{version}\n' <packages>`.
  Watch the versioned names `nodejs24`, `nodejs24-npm` and `kubernetes1.NN-client`.
- **Fedora packages** are unpinned, so a rebuild takes their newest builds by itself.
  Still list what a rebuild would bring, since phase 2 depends on it:
  `dnf -q repoquery --latest-limit=1 --qf '%{name} %{version}\n' golang nodejs24 pnpm python3 uv helm kind`,
  against the installed `go version`, `node --version`, `pnpm --version`, `python3 --version`, `uv --version`,
  `helm version --short` and `kind version`.
  - A newer **Node.js LTS major** (`nodejs26` and so on) is a major. Taking it renames the package in the
    Dockerfile and moves the whole Node.js coherence group in phase 2.
  - **`kubernetes1.NN-client`**: find the default node image of the `kind` version that the repoquery reports, from
    `gh release view v<kind version> -R kubernetes-sigs/kind` (its notes name `kindest/node:v1.NN.x`).
    Then set the package to that minor. Check that the package exists with repoquery.
- **Perfetto** `trace_processor_shell`: newest release from `gh release list -R google/perfetto --exclude-pre-releases`.
- **OpenSpec**: newest from `gh release list -R Fission-AI/OpenSpec --exclude-pre-releases`.
- **kubeconform** (`yannh/kubeconform`) and **actionlint** (`rhysd/actionlint`): the `go install ...@vX` versions,
  newest from `gh release list -R <repo> --exclude-pre-releases`.
- **Coding agents** (Claude, Codex, herdr, Antigravity) are installed from their `install.sh` scripts unpinned,
  and they update themselves. Nothing to edit; the rebuild refreshes them.
  Check only that each installer URL still answers: `curl -fsSI <url>`.

Apply the two-week rule to each release (`gh release view <tag> -R <repo> --json publishedAt`).
Ask about the majors you collected, in one question,
leaving out those that a commit of an earlier run records as declined (see step 3).

### 2. Edit and check

Skip this step if nothing needs an edit: step 3 checks the running container directly.

- For Perfetto and OpenSpec, a new version also needs **both** architectures' SHA-256 sums.
  Download each asset named in the Dockerfile's `case` into the scratchpad and run `sha256sum` on it.
  Then replace `sum=` for both `x86_64` and `aarch64`. Never copy a sum from release notes alone.
- Keep the Dockerfile's comments true. For example, if kubectl moves, the comment still says it matches kind.
- Prove the Dockerfile builds before asking for a rebuild. The Docker socket is the host's.
  Build without the cache: the `dnf install` line doesn't change when Fedora's packages do,
  so a cached build reuses the old layer and reports the old versions.
  ```sh
  docker build --pull --no-cache -t vamos-devcontainer-candidate .devcontainer
  docker run --rm vamos-devcontainer-candidate sh -c \
    'cat /etc/fedora-release; go version; node --version; pnpm --version; python3 --version; uv --version;
     kind version; kubectl version --client; openspec --version; actionlint --version;
     go version -m /usr/local/bin/kubeconform | grep -m1 "^\smod"; trace_processor_shell --version'
  docker rmi vamos-devcontainer-candidate
  ```
  It checks only this machine's architecture. The other architecture's sums were checked when you downloaded them.

### 3. Hand over, or continue

Phase 1 is done, and phase 2 may start **in this session**, only when:
- the Dockerfile needed no edits, and
- the running container already has what it pins (compare the `docker run` line above, run directly here), and
- the repoquery showed no newer `golang`, `nodejs24`, `pnpm`, `python3` or `uv` than is installed.
  Newer builds of other packages don't matter.

Otherwise:
1. Commit the Dockerfile, if it changed, as "Update the devcontainer's dependencies".
   List in the body what the candidate image reported and any majors the user declined. That is the record phase 2 reads.
   If only Fedora's packages are newer, there is nothing to commit; phase 2 re-derives what it needs.
   But if the user declined a major, record it anyway, with `git commit --allow-empty`, so the next session reads it.
2. Tell the user, and stop. Ask for a rebuild **without cache**: a plain rebuild reuses the cached `dnf install`
   layer and gets none of Fedora's newer packages.
   > The devcontainer is ready to update (commit `<sha>`, or "no file changes, only newer Fedora packages"). Rebuild it with **Dev Containers: Rebuild Without Cache and Reopen in Container**. This session ends with it. Then start a new session and run `/update-dependencies` again: it will confirm the rebuild and update everything else.

Don't start phase 2 in a container that is out of date. Its toolchains would check the updates against the wrong versions.

## Phase 2: everything else

Start by reading `git log -5`, so that you know what phase 1 recorded and which majors were declined.
Ask once about the new majors found in this phase. Leave declined majors alone unless the user brings them up.

### Go modules: [go.mod](../../../go.mod), [go.sum](../../../go.sum)

1. List updates to direct dependencies, with their dates:
   `go list -m -u -f '{{if and .Update (not .Indirect)}}{{.Path}} {{.Version}} {{.Update.Version}} {{.Update.Time}}{{end}}' all`.
   If the newest is under two weeks old, take the newest version that isn't. Check it with `go list -m -versions <mod>`,
   then `go list -m -json <mod>@<v>` for its `Time`.
2. The `go.podman.io/*` modules (buildah, common, image, storage) move together.
   Take buildah's release, then the versions of the other three that buildah's own go.mod requires.
   `go.starlark.net` has no releases, so take its newest pseudo-version that is at least two weeks old.
3. `go get <mod>@<v> ...`, then `go mod tidy`.
   If this raises go.mod's `go` line, it must stay ≤ the devcontainer's `go version`.
   The line's minor must also match the Dockerfile's `golang:1.NN-bookworm`.
4. **[Dockerfile](../../../Dockerfile)**: `golang:1.NN-bookworm` follows go.mod's minor; keep bookworm (see glibc floor).
   `fedora:NN` in the runtime stage follows the devcontainer's Fedora.
   Fedora's `containers-common` and `netavark` are unpinned.

### Go example: [examples/golang-cli](../../../examples/golang-cli)

It has no module dependencies, only its Go version. Its `go` line follows the newest Go minor
whose `golang:1.NN-alpine` image is at least two weeks old; a new minor is a major here.
Update its whole coherence group, then re-pin it as described under vamos.lock pins.

### Node packages

All of them pin exact versions, and their settings refuse releases under two weeks old.

| Project | Manifest and lock | Settings |
| --- | --- | --- |
| [docs/site](../../../docs/site/package.json) | `package.json`, `pnpm-lock.yaml` | `pnpm-workspace.yaml` |
| [examples/typescript-ui](../../../examples/typescript-ui/package.json) | `package.json`, `pnpm-lock.yaml` | `pnpm-workspace.yaml` |
| [examples/strawberry-react/ui](../../../examples/strawberry-react/ui/package.json) | `package.json`, `pnpm-lock.yaml` | `pnpm-workspace.yaml` |
| [examples/typescript-mcp](../../../examples/typescript-mcp/package.json) | `package.json`, `package-lock.json` (npm) | `.npmrc` |

For each pnpm project, in its directory:
1. Run `pnpm outdated` and note the new majors.
2. Run `pnpm update --latest` to take everything, or `pnpm update --latest <pkg>...` to leave a declined major out.
   Then run `pnpm install`.
   Check that `package.json` still has exact versions, with no `^` or `~`.

For typescript-mcp:
1. Run `npm outdated`.
2. Run `npm install <pkg>@<version> ...` with versions at least two weeks old. Look them up with `npm view <pkg> time --json`.
3. Run `npm update` to move the transitive dependencies in the lockfile.
4. Keep `@types/node` on the Node.js major in use.

Three more things:
- **`engines.node`**: a package may raise its own Node.js minimum within a major, as astro 7.4.2 did.
  For each project with `engines`, compare its floor with `npm view <pkg>@<version> engines.node`
  for its direct dependencies, and raise the floor to the highest they need.
- **[docs/site/wrangler.jsonc](../../../docs/site/wrangler.jsonc)** `compatibility_date`: after Wrangler moves,
  set it to two weeks ago, or to the installed workerd's date if that is earlier.
  workerd's version `1.YYYYMMDD.N` names the newest date it supports; find it with
  `ls node_modules/.pnpm | grep '^workerd@'`. Neither `pnpm build` nor `wrangler dev` warns about a later date.
- **pnpm itself**: after the rebuild, set every `pnpm@X` and `packageManager` in its coherence group
  to the devcontainer's `pnpm --version`.

### Python packages

| Project | Inputs | Locks |
| --- | --- | --- |
| [examples/fastmcp](../../../examples/fastmcp/README.md#dependencies) | `requirements.txt`, `requirements-dev.txt` | `requirements_lock.txt`, `requirements-dev_lock.txt` |
| [examples/strawberry-react/backend](../../../examples/strawberry-react/README.md) | `requirements.txt`, `requirements_dev.txt` | `requirements_lock.txt`, `requirements_dev_lock.txt` |

Regenerate with each README's commands, adding `--upgrade`.
Without `--upgrade`, uv keeps the versions already in the output file.
Set `--exclude-newer` to the date 14 days ago (`date -d '14 days ago' +%F`).
Compile the runtime lock before the dev lock, because strawberry's `requirements_dev.txt` includes `requirements_lock.txt`.
Each lock's header records the command, so check that it shows the new date.
`fastmcp>=4.0,<5` is a major bound: if fastmcp 5 has been out two weeks, it's a major.

In [examples/strawberry-react/backend/BUILD](../../../examples/strawberry-react/backend/BUILD),
set `PYRIGHT_PYTHON_NODE_VERSION` to the newest **LTS** Node.js release at least two weeks old.
Find it with `curl -fsSL https://nodejs.org/dist/index.json | jq -r '[.[] | select(.lts)][0] | .version, .date'`
(the newest LTS release comes first). Write it without the `v`.

### Base images in BUILD files and their pins in `vamos.lock`

| Project | BUILD tags | Lock |
| --- | --- | --- |
| [examples/fastmcp](../../../examples/fastmcp/BUILD) | `python:3.14-slim` | `vamos.lock` |
| [examples/golang-cli](../../../examples/golang-cli/BUILD) | `golang:1.26-alpine` | `vamos.lock` |
| [examples/typescript-mcp](../../../examples/typescript-mcp/BUILD) | `node:24-alpine` | `vamos.lock` |
| [examples/typescript-ui](../../../examples/typescript-ui/BUILD) | `node:24-alpine`, `nginx:alpine` | `vamos.lock` |
| [examples/strawberry-react](../../../examples/strawberry-react/vamos.lock) | `backend/BUILD`: `python:3.14`; `ui/BUILD`: `node:24-alpine`, `nginx:alpine` | `vamos.lock` (one for the workspace) |

1. Build the CLI: `go build -o vamos .`.
2. For each project, run `./vamos update --dry-run -C <project>` and then `./vamos update -C <project>`.
   The dry run reports newer major and minor tags, which are majors to ask about.
   The update re-pins each tag to its newest release and digest.
   (`vamos update` asks Docker Hub directly. It doesn't apply the two-week rule, and doesn't need to:
   the tags it follows are the maintainers' own rolling tags.)
3. If a tag changes, update its coherence group. That includes the tutorial, which walks through typescript-ui's BUILD,
   and [docs/user/reference/build.md](../../../docs/user/reference/build.md)'s example `pull`.
   Then run `vamos update` again.

Leave `busybox:1.36` and `BusyboxIndex` in [internal/testenv/testenv.go](../../../internal/testenv/testenv.go) alone.
The test suite pins them on purpose. Change them only if Docker Hub stops serving that digest, as its comment says.
The image names and tags in Go tests and feature files are test data, not dependencies.

### Remote environment

- `registry:3` in its coherence group: check for a newer major with
  `curl -fsSL 'https://hub.docker.com/v2/repositories/library/registry/tags?page_size=100' | jq -r '.results[].name'`.
- The chart's own `version` and `appVersion` in [Chart.yaml](../../../remote/helm/vamos/Chart.yaml)
  version vamos itself, not a dependency. Leave them alone.

### GitHub Actions: [.github/workflows/release.yml](../../../.github/workflows/release.yml)

Each `uses: owner/action@vN` follows its newest major. Find it with `gh release list -R <owner>/<action> --exclude-pre-releases`.
A new major here is routine, so propose it with the others.
Read its release notes for changed inputs, such as `docker/build-push-action`'s `outputs` and `cache-*`,
and for a newer Node.js runtime.
The runners `ubuntu-24.04` and `ubuntu-24.04-arm`, and the smoke test's `ubuntu:24.04`, are the glibc floor's platform.
Don't change them without changing that promise.

### OpenSpec's generated files

If phase 1 moved the OpenSpec CLI, run `openspec update`. It refreshes `.agents/skills/openspec-*`,
`.claude/skills/openspec-*`, `.claude/commands/opsx/` and `.agents/workflows/`, which record `generatedBy`.
Review the diff like any other.

### Sweep for stragglers

New pins appear over time. Before validating, search for versions this list doesn't cover:
```sh
git grep -nE '(@v?[0-9]+\.[0-9]+|:[0-9]+(\.[0-9]+)*(-[a-z]+)?["[:space:]]|tag *= *"|version *[:=])' \
  -- ':!*lock*' ':!go.sum' ':!openspec/changes/archive' ':!internal' ':!docs/site/src/content'
```
Also run `git ls-files | grep -iE 'dockerfile|requirements|package\.json|go\.mod|\.lock$|ya?ml$'`.
Compare both against the files named here.
Update anything new, and add it to this skill, so that the next run covers it.

### Validate

Follow [.agents/kb/testing.md](../../../.agents/kb/testing.md) for every scope touched. That means:
- `go build -o vamos .`, `go vet ./...`, `gofmt -l .`, `go test -short ./...`, then the full `go test ./...`.
- Each changed example's own checks, from its README:
  - typescript-ui: `pnpm check`, `pnpm build` and `./examples/typescript-ui/check.sh`
  - typescript-mcp: `npm run check`
  - fastmcp: in `.venv`, `ruff check .`, `ruff format --check .`, `pyright` and `pytest`
  - strawberry-react: the backend's `pyright && pytest` and the UI's `pnpm check`
- The smoke checks in [.agents/kb/smoke-checks.md](../../../.agents/kb/smoke-checks.md) for the examples and the Dockerfile.
  For the release workflow, smoke check 11, which runs `actionlint`.
  For the chart, `helm lint --strict remote/helm/vamos` and `helm template vamos remote/helm/vamos | kubeconform -strict -summary`.
- docs/site: `pnpm install && pnpm build && pnpm test`. Neither starts workerd, so if Wrangler or the date moved,
  also run `pnpm exec wrangler dev --port 8799` and fetch a page, as HTML and with `Accept: text/markdown`.
  If Mermaid moved, ask the user to look at the tutorial's diagrams in a browser: they render client-side.

If an update breaks something you can't fix with a small, clear change, revert that one update.
Record it in the report instead of forcing it through.

### Commit and report

Commit as "Update dependencies", or as separate commits per area if the diff is large.
Then report to the user:
- what moved, per area
- the majors that were declined or deferred
- any update that was reverted, and why
- which validation ran, and what it showed
