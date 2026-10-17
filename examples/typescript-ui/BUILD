pull(
    name="nodejs",
    image="docker.io/library/node",
    tag="24-alpine",
)

glob(
    name="install-files",
    include=["package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"],
)

glob(
    name="source-files",
    include=["index.html", "tsconfig.json", "src/**"],
)

layer(
    name="packages",
    base="nodejs",
    steps=[
        workdir("/workspace"),
        add("install-files", "/workspace"),
        run(["npm", "install", "-g", "pnpm@10.33.0"]),
        run(["pnpm", "install", "--frozen-lockfile"]),
    ],
)

task(
    name="build",
    base="packages",
    steps=[
        add("source-files", "/workspace"),
        run(["pnpm", "build"]),
    ],
    source="/workspace/dist",
    include=["."],
)

pull(
    name="nginx",
    image="docker.io/library/nginx",
    tag="alpine",
)

layer(
    name="frontend",
    base="nginx",
    steps=[add("build", "/usr/share/nginx/html")],
)
