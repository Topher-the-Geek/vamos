build(
    base="docker.io/library/node:12-alpine",
    sources=["script.js"],
    destination="/home/node",
    run=["sh", "-c", "date > /home/node/build-date.txt"],
    command=["node", "/home/node/script.js"],
)
