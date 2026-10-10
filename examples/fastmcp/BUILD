build(
    base="docker.io/library/python:3.14-slim",
    sources=["requirements_lock.txt", "server.py"],
    destination="/app",
    run=[
        "pip", "install", "--no-cache-dir", "--root-user-action=ignore",
        "--require-hashes", "-r", "/app/requirements_lock.txt",
    ],
    command=["python", "/app/server.py"],
)
