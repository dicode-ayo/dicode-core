# Use Microsoft's maintained devcontainer Go image — regularly patched for CVEs
FROM mcr.microsoft.com/devcontainers/go:1.25-bookworm

# Install Podman + rootless container dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    podman \
    fuse-overlayfs \
    slirp4netns \
    uidmap \
    && rm -rf /var/lib/apt/lists/*

# Configure Podman storage to use fuse-overlayfs (required inside containers)
RUN mkdir -p /etc/containers && \
    printf '[storage]\ndriver = "overlay"\n[storage.options.overlay]\nmount_program = "/usr/bin/fuse-overlayfs"\n' \
    > /etc/containers/storage.conf

# Install Go tools (as root, then fix ownership for the vscode user)
RUN go install golang.org/x/tools/gopls@latest \
    && go install github.com/go-delve/delve/cmd/dlv@latest \
    && go install golang.org/x/tools/cmd/goimports@latest \
    && chown -R vscode:vscode /go

# Install Oh My Zsh with git plugin for the vscode user
RUN su vscode -c 'sh -c "$(curl -fsSL https://raw.githubusercontent.com/ohmyzsh/ohmyzsh/master/tools/install.sh)" "" --unattended' \
    && sed -i 's/^plugins=(git)/plugins=(git gitfast git-extras)/' /home/vscode/.zshrc

# Install Playwright's Chrome and its OS dependencies (for MCP browser tools)
RUN npx playwright install --with-deps chrome

# Install cloudflared for Cloudflare Tunnel access
RUN curl -fsSL https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64.deb -o /tmp/cloudflared.deb \
    && dpkg -i /tmp/cloudflared.deb \
    && rm /tmp/cloudflared.deb

WORKDIR /workspace
