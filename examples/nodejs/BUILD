build(
    base="docker.io/library/node:24-alpine",
    sources=["package.json", "package-lock.json", "server.ts"],
    destination="/app",
    # Keep npm's cache and logs out of the image.
    run=["sh", "-c", "npm ci --omit=dev --prefix /app --cache /tmp/npm && rm -rf /tmp/npm"],
    command=["node", "/app/server.ts"],
)
