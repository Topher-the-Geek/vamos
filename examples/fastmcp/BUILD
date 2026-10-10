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
    sources=["package-manifest"],
    destination="/tmp",
    # The lock file is needed only to install, so leave it out of the image.
    run=[
        "sh", "-c",
        "pip install --no-cache-dir --root-user-action=ignore --require-hashes -r /tmp/requirements_lock.txt"
        + " && rm /tmp/requirements_lock.txt",
    ],
)

glob(
    name="server-source",
    include=["server.py"],
)

layer(
    name="server",
    base="packages",
    sources=["server-source"],
    destination="/app",
    command=["python", "/app/server.py"],
)
