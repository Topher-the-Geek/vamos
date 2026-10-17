load("//:defs.star", "PLATFORMS")

pull(
    name = "python",
    image = "docker.io/library/python",
    tag = "3.14",
)

glob(
    name = "pip-install-files",
    include = [
        "requirements_lock.txt",
    ],
)

layer(
    name = "packages",
    base = "python",
    steps = [
        workdir("/usr/src/app"),
        build_env(PIP_ROOT_USER_ACTION = "ignore"),
        add("pip-install-files", "/usr/src/app"),
        run([
            "pip",
            "install",
            "--no-cache-dir",
            "-r",
            "requirements_lock.txt",
        ]),
        env(PYTHONPATH = "/usr/src/app"),
    ],
)

glob(
    name = "server-files",
    include = ["*.py"],
    exclude = ["*_test.py"],
)

layer(
    name = "layer",
    base = "packages",
    steps = [
        add("server-files", "/usr/src/app"),
    ],
    command = [
        "fastapi",
        "run",
        "/usr/src/app/server.py",
    ],
)

index(
    name = "server",
    layer = "layer",
    platforms = PLATFORMS,
)

# The UI's types are generated from this.
# The schema is the same on every platform,
# so it is exported once, natively, however many platforms the UI is built for.
task(
    name = "schema",
    base = dep("layer", platform = EXEC_PLATFORM),
    steps = [
        run([
            "strawberry",
            "export-schema",
            "-o",
            "schema.graphql",
            "schema",
        ]),
    ],
    source = "/usr/src/app",
    include = ["schema.graphql"],
)

glob(
    name = "pip-dev-install-files",
    include = [
        "requirements_dev_lock.txt",
    ],
)

layer(
    name = "dev-packages",
    base = "packages",
    steps = [
        build_env(PIP_ROOT_USER_ACTION = "ignore"),
        add("pip-dev-install-files", "/usr/src/app"),
        run([
            "pip",
            "install",
            "--no-cache-dir",
            "-r",
            "requirements_dev_lock.txt",
        ]),
        run(["pyright", "--version"]),  # Installs Node.js, which pyright runs on
    ],
)

glob(
    name = "test-files",
    include = [
        "*_test.py",
        "pyrightconfig.json",
        "pytest.ini",
    ],
)

# Checks make no files.
# Run them with `vamos build //backend:lint //backend:test`.
task(
    name = "lint",
    base = "dev-packages",
    steps = [
        add("server-files", "/usr/src/app"),
        add("test-files", "/usr/src/app"),
        run(["pyright"]),
    ],
)

task(
    name = "test",
    base = "dev-packages",
    steps = [
        add("server-files", "/usr/src/app"),
        add("test-files", "/usr/src/app"),
        run(["pytest"]),
    ],
)
