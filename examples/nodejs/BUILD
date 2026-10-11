pull(
    name="node",
    image="docker.io/library/node",
    tag="24-alpine",
)

glob(
    name="package-manifest",
    include=["package.json", "package-lock.json"],
)

layer(
    name="packages",
    base="node",
    steps=[
        add("package-manifest", "/app"),
        workdir("/app"),
        # Keep npm's cache out of the image.
        build_env(npm_config_cache="/tmp/npm"),
        run(["npm", "ci", "--omit=dev"]),
        run(["rm", "-rf", "/tmp/npm"]),
    ],
)

# The compiler is a dev dependency, so it's installed only for the build task.
layer(
    name="dev-packages",
    base="node",
    steps=[
        add("package-manifest", "/build"),
        workdir("/build"),
        run(["npm", "ci"]),
    ],
)

glob(
    name="server-source",
    include=["server.ts", "tsconfig.json", "tsconfig.build.json"],
)

# Runs in /build, the working directory of its base.
task(
    name="build",
    base="dev-packages",
    steps=[
        add("server-source", "/build"),
        run(["npm", "run", "build"]),
    ],
    source="/build/dist",
    include=["server.js"],
)

layer(
    name="server",
    base="packages",
    steps=[
        add("build", "/app"),
        env(NODE_ENV="production"),
    ],
    command=["node", "server.js"],
)
