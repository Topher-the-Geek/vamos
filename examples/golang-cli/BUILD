# Match the tag to the Go version in go.mod.
# The tag names an index, and its manifest for whichever platform names this target is pulled.
# Here that is only ever the exec platform, since "build" runs on it.
pull(
    name = "golang",
    image = "docker.io/library/golang",
    tag = "1.25-alpine",
)

glob(
    name = "build-files",
    include = [
        "go.mod",
        "main.go",
    ],
)

# Go's flags, by mode: a debugger needs optimizations off, and a release drops the symbol tables.
GO_FLAGS = {
    "debug": ["-gcflags=all=-N -l"],
    "release": [
        "-trimpath",
        "-ldflags=-s -w",
    ],
}

# Cross-compiles natively: the compiler is built for the exec platform, and the binary for the target one.
# Reading ctx.os and ctx.arch is what makes each target platform's binary its own build.
task(
    name = "build",
    params = {"mode": param(default = "release", values = GO_FLAGS.keys())},
    base = dep("golang", platform = EXEC_PLATFORM),
    steps = lambda ctx: [
        workdir("/workspace"),
        add("build-files", "/workspace"),
        build_env(
            CGO_ENABLED = "0",  # scratch has no libc
            GOARCH = ctx.arch,
            GOOS = ctx.os,
        ),
        run(["go", "build"] + GO_FLAGS[ctx.mode] + [
            "-o",
            "echo",
            "main.go",
        ]),
    ],
    source = "/workspace",
    include = ["echo"],
)

# Runs nothing, so it needs no emulation: its platform is only recorded in the image's config.
layer(
    name = "layer",
    params = {"mode": "release"},
    base = "scratch",
    steps = [
        add(dep("build", mode = arg("mode")), "/"),
    ],
    command = [
        "/echo",
        "hola",
    ],
)

index(
    name = "echo",
    params = {"mode": "release"},
    layer = dep("layer", mode = arg("mode")),
    platforms = [
        "linux/amd64",
        "linux/arm64",
    ],
)
