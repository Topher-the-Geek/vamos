# golang-cli example

A Go command, `echo`, built into an image of the binary alone, for amd64 and arm64.

The `build` task cross-compiles: its base is the `golang` image for the platform vamos runs on, so the
compiler runs natively, while `GOOS` and `GOARCH` come from the platform the image is for. Since it reads
`ctx.os` and `ctx.arch`, each platform's binary is its own build. The image itself is a layer on `scratch` that only adds
the binary, so building it for another platform needs no emulator.

The `build` task takes a `mode` parameter, which the layer and the index pass on: `release`, the default, strips
the binary's symbols, and `debug` turns off optimizations for a debugger.

## Develop

```
go vet ./... && gofmt -l .
go run . -n hola
```

## Build with vamos

From the repository root:

```
./vamos load echo vamos-echo -C examples/golang-cli
docker run --rm vamos-echo                          # hola
docker run --rm vamos-echo /echo -n hola            # hola, without a newline
./vamos load echo vamos-echo:debug --set mode=debug -C examples/golang-cli
./vamos push echo ghcr.io/me/echo:1.0 -C examples/golang-cli
```

`echo` is an index of the image for each platform. `push` sends all of it, while `load` sends the image for the
Docker daemon's platform.
