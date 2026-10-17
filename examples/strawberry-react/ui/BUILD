load("//:defs.star", "PLATFORMS")

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
    base = dep("packages", platform = EXEC_PLATFORM),
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
# The translations are chosen by languages, in "messages".
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
    exclude = [
        "public/locales",
        "src/gql",
    ],
)

glob(
    name = "messages",
    params = {"languages": ["en"]},
    include = lambda ctx: ["public/locales/%s/*.json" % language for language in ctx.languages],
)

# Vite's modes, by mode.
VITE_MODES = {
    "debug": "development",
    "release": "production",
}

# The bundle is the same on every platform, so it is built once, natively.
# It still depends on the languages, through "messages", and on the mode.
task(
    name = "build",
    params = {
        "languages": ["en"],
        "mode": param(default = "release", values = VITE_MODES.keys()),
    },
    base = dep("packages", platform = EXEC_PLATFORM),
    steps = lambda ctx: [
        add("build-files", "/workspace"),
        add(dep("messages", languages = arg("languages")), "/workspace"),
        add("graphql-codegen", "/workspace"),
        run([
            "pnpm",
            "build",
            "--mode",
            VITE_MODES[ctx.mode],
        ]),
    ],
    source = "/workspace/dist",
    include = ["."],
)

layer(
    name = "layer",
    params = {
        "languages": ["en"],
        "mode": "release",
    },
    base = "nginx",
    steps = [
        add(dep("build", languages = arg("languages"), mode = arg("mode")), "/usr/share/nginx/html"),
    ],
)

index(
    name = "server",
    params = {
        "languages": ["en"],
        "mode": "release",
    },
    layer = dep("layer", languages = arg("languages"), mode = arg("mode")),
    platforms = PLATFORMS,
)

# Makes no files.
# Run it with `vamos build //ui:lint`.
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
