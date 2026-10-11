pull(
    name="python",
    image="docker.io/library/python",
    tag="3.14-slim",
)

glob(
    name="package-manifest",
    include=["requirements_lock.txt"],
)

layer(
    name="packages",
    base="python",
    steps=[
        add("package-manifest", "/tmp"),
        build_env(PIP_NO_CACHE_DIR="1", PIP_ROOT_USER_ACTION="ignore"),
        run(["pip", "install", "--require-hashes", "-r", "/tmp/requirements_lock.txt"]),
        # The lock file is needed only to install, so leave it out of the image.
        run(["rm", "/tmp/requirements_lock.txt"]),
    ],
)

glob(
    name="server-source",
    include=["server.py"],
)

layer(
    name="server",
    base="packages",
    steps=[
        add("server-source", "/app"),
        workdir("/app"),
    ],
    command=["python", "server.py"],
)
