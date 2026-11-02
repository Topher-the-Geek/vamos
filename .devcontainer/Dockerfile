FROM fedora:44

# - useful dev tools
# - build dependencies
# - VS Code server dependencies
# - Kubernetes tools for the Helm chart: kubectl matches the node image of kind's release
RUN dnf -y install \
    btrfs-progs-devel \
    buildah \
    containers-common \
    docker-cli \
    docker-compose \
    file \
    findutils \
    gh \
    git \
    golang \
    gpgme-devel \
    gzip \
    helm \
    jq \
    kind \
    kubernetes1.35-client \
    less \
    netavark \
    nodejs24 \
    nodejs24-npm \
    openssh-clients \
    pnpm \
    procps-ng \
    python3 \
    ShellCheck \
    tar \
    tree \
    unzip \
    uv \
    which \
    zip \
 && dnf clean all

# Perfetto's trace processor, to query the traces that `vamos --trace` writes, by SQL.
RUN case "$(uname -m)" in \
      x86_64) arch=amd64 sum=32c739f71b2d39721afd294c0b038f2499a44a71b5b6bcdd85b83faca0b240b9 ;; \
      aarch64) arch=arm64 sum=a82bf4111a340a7ea8577bcfd62e014e8e81b9e6a35a3190f5415fb800051ab0 ;; \
      *) echo "no trace_processor_shell for $(uname -m)" >&2; exit 1 ;; \
    esac \
 && curl -fsSL -o /tmp/perfetto.zip \
      "https://github.com/google/perfetto/releases/download/v58.2/linux-$arch.zip" \
 && echo "$sum  /tmp/perfetto.zip" | sha256sum -c - \
 && unzip -j /tmp/perfetto.zip "linux-$arch/trace_processor_shell" -d /usr/local/bin \
 && chmod 755 /usr/local/bin/trace_processor_shell \
 && rm /tmp/perfetto.zip

# OpenSpec's standalone build, which runs without Node.js, for the change proposals under openspec/.
RUN case "$(uname -m)" in \
      x86_64) arch=x64 sum=f27fb2ff5c6c4aafb207cb0b797189215adb3a1150d4b4600a2d6a0b6e192ae1 ;; \
      aarch64) arch=arm64 sum=b9b61371a7caa1819da34f766cc24896cfe7a743841f53121b66bd1e2f1b9e93 ;; \
      *) echo "no openspec for $(uname -m)" >&2; exit 1 ;; \
    esac \
 && curl -fsSL -o /tmp/openspec.tar.gz \
      "https://github.com/Fission-AI/OpenSpec/releases/download/v1.16.0/openspec-linux-$arch.tar.gz" \
 && echo "$sum  /tmp/openspec.tar.gz" | sha256sum -c - \
 && tar -xzf /tmp/openspec.tar.gz -C /usr/local/bin openspec \
 && rm /tmp/openspec.tar.gz

# Tools with no Fedora package:
# - kubeconform, to check rendered Helm manifests against the Kubernetes schemas
# - actionlint, to check the release workflow
RUN GOBIN=/usr/local/bin GOMODCACHE=/tmp/gomodcache GOCACHE=/tmp/gocache \
      go install github.com/yannh/kubeconform/cmd/kubeconform@v0.8.0 \
 && GOBIN=/usr/local/bin GOMODCACHE=/tmp/gomodcache GOCACHE=/tmp/gocache \
      go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 \
 && rm -rf /tmp/gomodcache /tmp/gocache

# Coding agents, which update themselves in ~/.local/bin.
# Their installers append PATH lines to .bashrc or .bash_profile; restore both, and set PATH once instead,
# for every process rather than only interactive shells.
# Fedora's .bashrc adds ~/.local/bin and ~/bin unless PATH already starts with them, so it starts with both.
ENV PATH=/root/.local/bin:/root/bin:$PATH
RUN cp /root/.bashrc /root/.bash_profile /tmp/ \
 && curl -LfsS https://claude.ai/install.sh | bash \
 && curl -LfsS https://chatgpt.com/codex/install.sh | bash \
 && curl -LfsS https://herdr.dev/install.sh | bash \
 && curl -LfsS https://antigravity.google/cli/install.sh | bash \
 && mv /tmp/.bashrc /tmp/.bash_profile /root/

# Run build steps with chroot isolation, so no OCI runtime is needed.
ENV BUILDAH_ISOLATION=chroot

# Container storage must not live on the container's own overlay filesystem.
# Fedora's default storage.conf uses kernel overlayfs, not fuse-overlayfs.
VOLUME /var/lib/containers
