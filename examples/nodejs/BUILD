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
    sources=["package-manifest"],
    destination="/app",
    # Keep npm's cache and logs out of the image.
    run=["sh", "-c", "npm ci --omit=dev --prefix /app --cache /tmp/npm && rm -rf /tmp/npm"],
)

# The compiler is a dev dependency, so it's installed only for the build task.
layer(
    name="dev-packages",
    base="node",
    sources=["package-manifest"],
    destination="/build",
    run=["npm", "ci", "--prefix", "/build"],
)

glob(
    name="server-source",
    include=["server.ts", "tsconfig.json", "tsconfig.build.json"],
)

task(
    name="build",
    base="dev-packages",
    sources=["server-source"],
    destination="/build",
    command=["npm", "run", "build", "--prefix", "/build"],
    source="/build/dist",
    include=["server.js"],
)

layer(
    name="server",
    base="packages",
    sources=["build"],
    destination="/app",
    command=["node", "/app/server.js"],
)
