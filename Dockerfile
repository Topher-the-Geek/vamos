# The release publishes the binary that the build stage makes, which install.sh installs,
# and Debian 12's glibc is old enough for it to run on Ubuntu 24.04 and later: it needs glibc 2.34.
# Buildah's namespace setup needs cgo, so the binary can't be static;
# these tags drop its other C libraries: signatures are checked in Go, not by gpgme, and there is no btrfs storage.
FROM golang:1.26-bookworm AS build

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
COPY internal/ ./internal/
# The release sets VERSION, such as v0.1.0, which `vamos --version` reports.
ARG VERSION
RUN go build -tags "containers_image_openpgp exclude_graphdriver_btrfs" \
    -ldflags "-X github.com/Topher-the-Geek/vamos/internal/run.stampedVersion=${VERSION}" -o vamos .

# The binary alone, for `docker build --target binary --output <dir>`, which the release attaches.
FROM scratch AS binary
COPY --from=build /build/vamos /vamos

FROM fedora:44

RUN dnf -y install \
    containers-common \
    netavark \
    && dnf clean all

# Run build steps with chroot isolation, so no OCI runtime is needed.
ENV BUILDAH_ISOLATION=chroot

# Container storage must not live on the container's own overlay filesystem.
VOLUME /var/lib/containers

WORKDIR /build
COPY --from=build /build/vamos /usr/local/bin/vamos

ENTRYPOINT ["vamos"]
