pull(
    name = "nodejs",
    image = "docker.io/library/node",
    tag = "24-alpine",
)

pull(
    name = "nginx",
    image = "docker.io/library/nginx",
    tag = "alpine",
)

layer(
    name = "pnpm",
    base = "nodejs",
    steps = [
        run([
            "npm",
            "install",
            "-g",
            "pnpm@10.33.0",
        ]),
    ],
)

glob(
    name = "pnpm-install-files",
    include = [
        "package.json",
        "pnpm-lock.yaml",
        "pnpm-workspace.yaml",
    ],
)

layer(
    name = "packages",
    base = "pnpm",
    steps = [
        workdir("/workspace"),
        add("pnpm-install-files", "/workspace"),
        run([
            "pnpm",
            "install",
            "--frozen-lockfile",
        ]),
    ],
)

glob(
    name = "graphql-codegen-files",
    include = [
        "codegen.ts",
        "src/**/*queries.ts",  # codegen.ts's documents
    ],
)

# Generates src/gql/types.ts from the backend's schema.
task(
    name = "graphql-codegen",
    base = "packages",
    steps = [
        add("//backend:schema", "/workspace"),
        add("graphql-codegen-files", "/workspace"),
        run([
            "pnpm",
            "graphql-codegen",
        ]),
    ],
    source = "/workspace",
    include = ["src/gql"],
)

# src/gql is generated, so it is left out, in case an earlier `pnpm codegen` made it locally.
glob(
    name = "build-files",
    include = [
        "eslint.config.js",
        "index.html",
        "public",
        "src",
        "tsconfig*.json",
        "vite.config.ts",
    ],
    exclude = ["src/gql"],
)

task(
    name = "build",
    base = "packages",
    steps = [
        add("build-files", "/workspace"),
        add("graphql-codegen", "/workspace"),
        run([
            "pnpm",
            "build",
        ]),
    ],
    source = "/workspace/dist",
    include = ["."],
)

layer(
    name = "layer",
    base = "nginx",
    steps = [
        add("build", "/usr/share/nginx/html"),
    ],
)

# Makes no files. Run it with `vamos build //ui:lint`.
task(
    name = "lint",
    base = "packages",
    steps = [
        add("build-files", "/workspace"),
        add("graphql-codegen", "/workspace"),
        run([
            "pnpm",
            "lint",
        ]),
    ],
)
