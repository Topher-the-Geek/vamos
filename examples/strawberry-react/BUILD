# The release: the UI's and the backend's indexes, published together under every --tag,
# only once the backend's tests and both packages' lint pass.
# `registry`, which has no default, chooses where, such as a local registry to try it:
# vamos publish //:release --tag 1.0 --set registry=localhost:5000/strawberry
publication(
    name = "release",
    params = {"registry": param(type = "string")},
    checks = [
        "//backend:lint",
        "//backend:test",
        "//ui:lint",
    ],
    images = lambda ctx: {
        ctx.registry + "/strawberry-ui": "//ui:server",
        ctx.registry + "/strawberry-backend": "//backend:server",
    },
)
