# vamos

A Go CLI that uses the [buildah](https://github.com/containers/buildah) library to build
a container image described by a `BUILD` file written in [Starlark](https://github.com/bazelbuild/starlark).

```
vamos push <image>              # push to a registry, e.g. ghcr.io/me/app:1.0
vamos load <image>              # load into the local Docker daemon, e.g. app:dev
vamos write <file.tar> [-t app] # write a tarball that `docker load` can import
```

Each command builds from the `BUILD` file in the current directory, or in the directory
given with `-C <dir>`. Image names without a tag get `:latest`.

- `push` uses credentials from `docker login` (`~/.docker/config.json`) or, if set, only those
  in `$REGISTRY_AUTH_FILE`. Use `--tls-verify=false` for a registry served over plain HTTP, or mark
  it `insecure = true` in `registries.conf`.
- `load` talks to the Docker daemon as the `docker` command does: at `$DOCKER_HOST`, or
  `/var/run/docker.sock`, with TLS settings from `$DOCKER_CERT_PATH` and `$DOCKER_TLS_VERIFY`.
- `write` records no image name in the tarball unless `--tag` is given. An existing file is
  replaced only once the new image is complete.

Run `vamos <command> --help` for details.

## BUILD files

A `BUILD` file calls `build()` exactly once:

```python
build(
    base="docker.io/library/node:12-alpine",       # required: image to build from
    sources=["script.js"],                         # files relative to the BUILD file
    destination="/home/node",                      # directory to copy sources into
    run=["sh", "-c", "date > /home/node/build-date.txt"],  # command run at build time
    command=["node", "/home/node/script.js"],      # the image's default command
)
```

Sources and their parent directories within the project must not be symbolic links.
Links inside a source directory are copied as links, as Docker's `COPY` does.

See [examples/nodejs/](examples/nodejs/).

## Build

```
docker build . -t vamos
```

## Test

```
go test ./...          # everything, including image builds; needs root
go test -short ./...   # command line and BUILD file checks only
```

Image builds use `busybox` from Docker Hub. `push` is tested against an in-process registry
and `load` against a fake Docker daemon; if a real Docker daemon is running, `load` is also
checked against it.

## Run

Buildah needs to create namespaces and mounts, which Docker's defaults don't allow.
Without extra permissions the run fails with
`Error during unshare(CLONE_NEWUSER): Operation not permitted`.

Run with full privileges, loading the image straight into the host's Docker:

```
docker run --rm -it --privileged \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD/examples/nodejs:/example" \
  vamos load vamos-example -C /example
```

Or grant only what buildah needs:

```
docker run --rm -it \
  --cap-add SYS_ADMIN \
  --security-opt seccomp=unconfined \
  --security-opt apparmor=unconfined \
  --security-opt label=disable \
  -v "$PWD/examples/nodejs:/example" -v "$PWD/output:/output" \
  vamos write /output/example.tar -t vamos-example -C /example
```

This variant writes a tarball instead. To push, use `vamos push` and mount your credentials,
e.g. `-v ~/.docker/config.json:/root/.docker/config.json:ro`.

On success it prints `Image built! <destination> <image-id>`.

## Run the built image

```
docker run -i --rm vamos-example
```

If you wrote a tarball, `docker load -i output/example.tar` first.

The example is an [MCP](https://modelcontextprotocol.io) server with an `echo` tool, speaking MCP over stdin and
stdout. Send it a request with `docker run -i --rm vamos-example < examples/nodejs/echo-request.jsonl`.
