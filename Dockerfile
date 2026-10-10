FROM fedora:44 AS build

RUN dnf -y install \
    btrfs-progs-devel \
    golang \
    gpgme-devel \
    && dnf clean all

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
COPY internal/ ./internal/
RUN go build -o vamos .

FROM fedora:44

RUN dnf -y install \
    containers-common \
    gpgme \
    netavark \
    && dnf clean all

# Run build steps with chroot isolation, so no OCI runtime is needed.
ENV BUILDAH_ISOLATION=chroot

# Container storage must not live on the container's own overlay filesystem.
VOLUME /var/lib/containers

WORKDIR /build
COPY --from=build /build/vamos /usr/local/bin/vamos

ENTRYPOINT ["vamos"]
