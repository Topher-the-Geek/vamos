# publication Specification

## Purpose

Lets a project declare a release in its BUILD file, the checks that gate it and the images it publishes where,
and carry it out with one command that publishes nothing unless everything it depends on succeeded.

## Requirements

### Requirement: A publication declares its checks and its images

A BUILD file SHALL be able to declare `publication(name, images, checks?, params?)`,
where the optional `checks` is a list of task targets and `images` is a nonempty dictionary from a repository,
a lowercase registry repository name without a tag or digest, to the pull, layer or index target published there.
Mistakes in a publication SHALL be reported as the BUILD file loads, before anything is built,
naming the file, line and column.

#### Scenario: A publication names its checks and images

- **GIVEN** a BUILD file declaring tasks `lint` and `test`, a layer `app`, and
  `publication(name="release", checks=["lint", "test"], images={"<registry>/team/app": "app"})`
- **WHEN** I run `vamos publish release --tag v1`
- **THEN** it succeeds and `<registry>/team/app:v1` holds the image of `app`

#### Scenario: A check that isn't a task is refused

- **GIVEN** a publication whose `checks` names the layer `app`
- **WHEN** I run `vamos publish release --tag v1`
- **THEN** it fails, saying where the publication is declared and that a check must be a task
- **AND** no target ran and nothing is pushed

#### Scenario: An image that isn't a pull, layer or index is refused

- **GIVEN** a publication whose `images` maps a repository to the task `test`
- **WHEN** I run `vamos publish release --tag v1`
- **THEN** it fails, saying where the publication is declared and that a published target must be a pull,
  layer or index
- **AND** no target ran and nothing is pushed

#### Scenario: A repository with a tag or digest is refused

- **GIVEN** a publication whose `images` has the key `<registry>/team/app:v1`
- **WHEN** I run `vamos publish release --tag v1`
- **THEN** it fails, naming the repository and saying the tag comes from `--tag`
- **AND** no target ran and nothing is pushed

#### Scenario: A publication without images is refused

- **GIVEN** a publication whose `images` is empty
- **WHEN** I run `vamos publish release --tag v1`
- **THEN** it fails, saying where the publication is declared and that it publishes no image

### Requirement: One target can be published to several repositories

The same target SHALL be publishable to more than one repository of one publication,
each receiving the same image, or index, under every tag given.

#### Scenario: An image goes to two repositories

- **GIVEN** a publication whose `images` maps both `<registry>/team/app` and `<registry>/mirror/app` to `app`
- **WHEN** I run `vamos publish release --tag v1`
- **THEN** both `<registry>/team/app:v1` and `<registry>/mirror/app:v1` name the same manifest

### Requirement: A publication is a root, never a dependency

No target SHALL be able to depend on a publication, and a publication's own result SHALL never be cached:
publishing it again pushes again, while its checks and images are reused from the build cache as usual.

#### Scenario: Depending on a publication is refused

- **GIVEN** a layer whose base names the publication `release`
- **WHEN** I build that layer
- **THEN** it fails as the BUILD file loads, saying that a publication can't be depended on

#### Scenario: Publishing again pushes again from the cache

- **GIVEN** `vamos publish release --tag v1` has succeeded
- **WHEN** I run `vamos publish release --tag v2`
- **THEN** its checks and images are reused rather than run
- **AND** each repository holds the image under both `v1` and `v2`

### Requirement: A publication's repositories can depend on its parameters

A publication SHALL take `params`, set with `--set`, and its attributes SHALL accept functions of `ctx`,
so that a parameter can choose its repositories.

#### Scenario: A parameter chooses the registry

- **GIVEN** a publication with a parameter `env` whose `images` is a function of `ctx`
  mapping `<registry>/staging/app` to `app` when `env` is `staging`, and `<registry>/prod/app` otherwise
- **WHEN** I run `vamos publish release --tag v1 --set env=staging`
- **THEN** `<registry>/staging/app:v1` holds the image and `<registry>/prod/app` holds nothing

### Requirement: Publish requires at least one tag

`vamos publish <publication>` SHALL require `--tag`, which MAY be repeated,
and every image SHALL be published under every tag given.
A missing or invalid tag SHALL fail as a command-line mistake, showing usage, before anything is built.

#### Scenario: Every image under every tag

- **GIVEN** a publication of `app` to `<registry>/team/app`
- **WHEN** I run `vamos publish release --tag v1 --tag latest`
- **THEN** `<registry>/team/app:v1` and `<registry>/team/app:latest` name the same manifest

#### Scenario: No tag is a command-line mistake

- **WHEN** I run `vamos publish release`
- **THEN** it fails showing usage, saying `--tag` is required
- **AND** no target ran and nothing is pushed

#### Scenario: An invalid tag is a command-line mistake

- **WHEN** I run `vamos publish release --tag "not a tag"`
- **THEN** it fails showing usage, naming the tag
- **AND** no target ran and nothing is pushed

#### Scenario: Publish names something that isn't a publication

- **WHEN** I run `vamos publish app --tag v1`, where `app` is a layer
- **THEN** it fails, saying that `app` is a layer and `publish` takes a publication

### Requirement: Nothing is published unless every check and image succeeds

`vamos publish` SHALL build every check and every image, with their dependencies, in one run,
and SHALL push nothing to any repository unless all of them succeed.

#### Scenario: A failing check publishes nothing

- **GIVEN** a publication whose check `test` fails
- **WHEN** I run `vamos publish release --tag v1`
- **THEN** it fails, reporting the failure of `test`
- **AND** nothing is pushed to any of its repositories

#### Scenario: A failing image publishes nothing, not even the others

- **GIVEN** a publication of two layers, of which one fails to build
- **WHEN** I run `vamos publish release --tag v1`
- **THEN** it fails, reporting that layer's failure
- **AND** nothing is pushed to either repository

### Requirement: Every image is uploaded before any tag is written

Publishing SHALL first upload every image's or index's content to each of its repositories by digest, with no tag,
and SHALL write tags only after every upload has succeeded.
A failed upload SHALL leave every tag of the publication as it was.

#### Scenario: A failed upload writes no tag

- **GIVEN** a publication of two images, one to a registry that refuses the upload
- **WHEN** I run `vamos publish release --tag v1`
- **THEN** it fails, naming the repository whose upload failed and why
- **AND** no repository of the publication has the tag `v1`

### Requirement: A failure while tagging reports what was tagged

Once tagging starts, publishing SHALL attempt every tag of every repository even if some fail,
SHALL read each written tag back from the registry it wrote to and check that it names the uploaded manifest,
and SHALL fail if any tag was not written, naming each that failed and each that was written.

#### Scenario: Some tags fail

- **GIVEN** a publication of two repositories, of which one starts refusing writes after its upload
- **WHEN** I run `vamos publish release --tag v1`
- **THEN** it fails, naming the tag that wasn't written and why
- **AND** it reports the tag that was written, which names the uploaded manifest

#### Scenario: Running it again finishes the job

- **GIVEN** a publication that failed while tagging
- **WHEN** the registry accepts writes again and I run the same `vamos publish` again
- **THEN** it succeeds and every repository has every tag

### Requirement: Publish reports each destination

On success, `vamos publish` SHALL report one line for each repository and tag,
naming the destination and the image ID there, or for an index its manifest digest.

#### Scenario: One line per destination

- **WHEN** I run `vamos publish release --tag v1 --tag latest` for a publication of one image
- **THEN** it reports `Image built! <registry>/team/app:v1 <id>` and `Image built! <registry>/team/app:latest <id>`

### Requirement: Each side publishes with its own credentials

`vamos publish` SHALL push from the executor, with the executor's registry settings and credentials, by default,
and from the client, with the client's and the command line's `--tls-verify`, given `--push-from=client`,
as `vamos push` does.
On a coordinator, publishing SHALL run on its workers, and the coordinator itself SHALL never contact
a destination registry.

#### Scenario: The client publishes with --push-from=client

- **GIVEN** a registry that registries.conf doesn't mark insecure
- **WHEN** I run `vamos publish release --tag v1 --push-from=client --tls-verify=false`
- **THEN** it succeeds

#### Scenario: A coordinator's workers publish

- **GIVEN** a coordinator with workers
- **WHEN** I run `vamos publish release --tag v1 --coordinator <coordinator>`
- **THEN** it succeeds and every repository has the tag

### Requirement: A coordinator admits a publication as one job

A coordinator SHALL count a publication as one job against its limit of jobs that haven't ended,
however many repositories and tags it has, so that it is refused, if at all, before anything is pushed.
Its build is a job of its own, admitted the same way before the publication is submitted,
so a coordinator that is busy refuses that build first.

#### Scenario: A busy coordinator refuses before pushing

- **GIVEN** a coordinator whose limit of jobs is reached by another client's build, which hasn't ended
- **WHEN** I run `vamos publish release --tag v1` on it
- **THEN** it fails, saying the executor is busy
- **AND** nothing is pushed

### Requirement: A publication interrupted by a coordinator restart fails

A publication still running when its coordinator restarts SHALL fail,
saying that whether its tags were written is unknown and that publishing again finishes it.

#### Scenario: The coordinator restarts while tagging

- **GIVEN** a publication running on a coordinator
- **WHEN** the coordinator restarts before the publication ends
- **THEN** the publication fails, saying its tags may or may not have been written

### Requirement: The trace shows publishing

A trace of `vamos publish`, given `--trace`, SHALL show one publish job of the executor within the client's span
of publishing, and within it a span for each upload and each tag written,
every upload ending before the first tag starts.
Each copy SHALL be shown in the process that made it: the executor, the client given `--push-from=client`,
or a coordinator's worker, never the coordinator itself.

#### Scenario: Uploads, then tags, within one publish job

- **GIVEN** a publication of two images to two repositories
- **WHEN** I run `vamos publish release --tag v1 --tag latest --trace <file>`
- **THEN** the trace shows the executor's publish job within the client's span of publishing
- **AND** within it, one upload for each repository and one tag for each repository and tag
- **AND** every upload ends before the first tag starts

#### Scenario: Each side shows its own copies

- **WHEN** I publish with a trace from the executor, from the client given `--push-from=client`,
  and on a coordinator with workers
- **THEN** the trace shows the uploads and tags in the executor's process, in the client's, and in the workers'
  processes, respectively, and none in the coordinator's

#### Scenario: Publishing again skips what is uploaded

- **GIVEN** `vamos publish release --tag v1` has succeeded
- **WHEN** I run `vamos publish release --tag v2 --trace <file>`
- **THEN** the trace shows each upload skipping every blob, as the registry already has it

### Requirement: Building a publication publishes nothing

`vamos build <publication>` SHALL build its checks and its images as `vamos publish` would, and push nothing.

#### Scenario: A dry run

- **WHEN** I run `vamos build release`
- **THEN** it succeeds, its checks and images ran
- **AND** nothing is pushed to any of its repositories
