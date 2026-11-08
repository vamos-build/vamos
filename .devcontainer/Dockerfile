FROM fedora:44

# - useful dev tools
# - build dependencies
# - VS Code server dependencies
# - Kubernetes tools for the Helm chart: kubectl matches the node image of kind's release
# - headless Chromium, which renders Mermaid diagrams for mmdc, below
RUN dnf -y install \
    btrfs-progs-devel \
    buildah \
    chromium-headless \
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

# golangci-lint, which scripts/check runs: go vet, gofmt and the linters that .golangci.yml enables.
# Its build's Go must be at least go.mod's, which `golangci-lint version` reports.
RUN case "$(uname -m)" in \
      x86_64) arch=amd64 sum=9b7fe6a6bf6334b98decf8d86b11415ea7889afdabdd1959fdce23af2a9ebcea ;; \
      aarch64) arch=arm64 sum=b4d8f17068b35ee6060cc2f69a16fb48740d706fbba81661c318649c114a6998 ;; \
      *) echo "no golangci-lint for $(uname -m)" >&2; exit 1 ;; \
    esac \
 && curl -fsSL -o /tmp/golangci-lint.tar.gz \
      "https://github.com/golangci/golangci-lint/releases/download/v2.15.0/golangci-lint-2.15.0-linux-$arch.tar.gz" \
 && echo "$sum  /tmp/golangci-lint.tar.gz" | sha256sum -c - \
 && tar -xzf /tmp/golangci-lint.tar.gz -C /usr/local/bin --strip-components=1 \
      "golangci-lint-2.15.0-linux-$arch/golangci-lint" \
 && rm /tmp/golangci-lint.tar.gz

# Tools with no Fedora package:
# - kubeconform, to check rendered Helm manifests against the Kubernetes schemas
# - actionlint, to check the release workflow
# - govulncheck, which scripts/check runs, to find known vulnerabilities that vamos's code reaches
RUN GOBIN=/usr/local/bin GOMODCACHE=/tmp/gomodcache GOCACHE=/tmp/gocache \
      go install github.com/yannh/kubeconform/cmd/kubeconform@v0.8.0 \
 && GOBIN=/usr/local/bin GOMODCACHE=/tmp/gomodcache GOCACHE=/tmp/gocache \
      go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 \
 && GOBIN=/usr/local/bin GOMODCACHE=/tmp/gomodcache GOCACHE=/tmp/gocache \
      go install golang.org/x/vuln/cmd/govulncheck@v1.9.0 \
 && rm -rf /tmp/gomodcache /tmp/gocache

# Mermaid's CLI, mmdc, to render the diagrams in Markdown, such as docs/design's, to images for review.
# Puppeteer drives Fedora's headless Chromium rather than downloading its own, with Chromium's sandbox off,
# since Chromium won't start as root with it on; the wrapper gives mmdc both through a Puppeteer config.
# min-release-age takes dependencies at least two weeks old, as the repository's npm projects do.
RUN PUPPETEER_SKIP_DOWNLOAD=1 npm install -g --min-release-age=14 --no-fund --no-audit \
      @mermaid-js/mermaid-cli@12.0.0 puppeteer@25.16.0 \
 && printf '%s\n' '{ "executablePath": "/usr/lib64/chromium-browser/headless_shell", "args": ["--no-sandbox"] }' \
      > /usr/local/etc/mmdc-puppeteer.json \
 && rm -f /usr/local/bin/mmdc \
 && printf '%s\n' '#!/bin/sh' \
      'exec node /usr/local/lib/node_modules/@mermaid-js/mermaid-cli/src/cli.js -p /usr/local/etc/mmdc-puppeteer.json "$@"' \
      > /usr/local/bin/mmdc \
 && chmod 755 /usr/local/bin/mmdc

# Coding agents, which update themselves in ~/.local/bin, and the Dosu CLI, which npm installs globally.
# Their installers append PATH lines to .bashrc or .bash_profile; restore both, and set PATH once instead,
# for every process rather than only interactive shells.
# Fedora's .bashrc adds ~/.local/bin and ~/bin unless PATH already starts with them, so it starts with both.
ENV PATH=/root/.local/bin:/root/bin:$PATH
RUN cp /root/.bashrc /root/.bash_profile /tmp/ \
 && curl -LfsS https://antigravity.google/cli/install.sh | bash \
 && curl -LfsS https://claude.ai/install.sh | bash \
 && curl -LfsS https://chatgpt.com/codex/install.sh | bash \
 && curl -LfsS https://herdr.dev/install.sh | bash \
 && curl -fsSL https://cli.dosu.dev/install | sh \
 && mv /tmp/.bashrc /tmp/.bash_profile /root/

# Run build steps with chroot isolation, so no OCI runtime is needed.
ENV BUILDAH_ISOLATION=chroot

# Container storage must not live on the container's own overlay filesystem.
# Fedora's default storage.conf uses kernel overlayfs, not fuse-overlayfs.
VOLUME /var/lib/containers
