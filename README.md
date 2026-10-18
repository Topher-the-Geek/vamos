# vamos

A Go CLI that uses the [buildah](https://github.com/containers/buildah) library
to build container images described by `BUILD` files written in [Starlark](https://github.com/bazelbuild/starlark).

Start with the [tutorial](docs/tutorial.md) to learn the core concepts, set up vamos,
and build a website into a running container.
To build your own project, [install vamos in its devcontainer](docs/tutorial.md#in-your-projects-devcontainer).
The [user guide](docs/README.md) links to the CLI, BUILD language, configuration, and cache references.

## Development

Use the repository's [devcontainer](.devcontainer/devcontainer.json) for build dependencies and container privileges.
See the [contributor index](AGENTS.md) for development and validation instructions.

Build the CLI image from the repository root:

```sh
docker build . -t vamos
```

## Test

```
go test ./...          # everything, including image builds; needs root
go test -short ./...   # command line and BUILD file checks only
```

Image builds use `busybox` from Docker Hub.
`push` is tested against an in-process registry and `load` against a fake Docker daemon;
if a real Docker daemon is running, `load` is also checked against it.
The tests use the machine's build cache, and one of them empties it,
so the next build of your own project runs every target again.

## License

vamos is released under the [MIT License](LICENSE).
