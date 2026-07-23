FROM node:22.23.1-bookworm-slim@sha256:6c74791e557ce11fc957704f6d4fe134a7bc8d6f5ca4403205b2966bd488f6b3

ARG TARGETARCH
ARG CODE_SERVER_VERSION=4.129.0
ARG CODE_SERVER_SHA256_AMD64=66160d99431595b0e9f4245abae8d0f472289fb77c97007ce79f2d64d06507f8
ARG CODE_SERVER_SHA256_ARM64=e504da34067eacc04e03265eccf90e5d2c3fdc3f06c91c2028a5a80f1d05a5b8
ARG CODEX_VERSION=0.144.6
ARG CODEX_INTEGRITY=sha512-wk+2CWiBNXiJLBoN2D08N9RceWkSBnlgk5g2K1a4CXrP/C0gdlHyRUG7RFzm9y41DCK/7tvCct233JVxyFmznw==
ARG CLAUDE_CODE_VERSION=2.1.215
ARG CLAUDE_CODE_INTEGRITY=sha512-lsWBvyMyBqg/rOZ06o/HEhlpOzHsH8IBf9RH4u5gzizQ1LWS/oXAh5nqWNUu3hn2d8QkyYg0aseNzMDlGD+3qg==
ARG PI_VERSION=0.80.10
ARG PI_INTEGRITY=sha512-aL4apbupCHiVLSXASXvRzH4Q2vmtfrDa+0s909CJuVu/GgGylbDzr7oyF1mPmip5E+VxYYxKWmph4hV04wUcQg==
ARG OPENCODE_VERSION=1.18.3
ARG OPENCODE_INTEGRITY=sha512-HnItl/+uhSpj7JV9x6ITiE0XFq4b/PKF5OM03TIyiFoFiLw3MQoJOAXZFTEzC7IOgAIYcysRQBBmCmlXILkxww==
ARG TTYD_VERSION=1.7.7
ARG TTYD_SHA256_AMD64=8a217c968aba172e0dbf3f34447218dc015bc4d5e59bf51db2f2cd12b7be4f55
ARG TTYD_SHA256_ARM64=b38acadd89d1d396a0f5649aa52c539edbad07f4bc7348b27b4f4b7219dd4165

ENV DISABLE_AUTOUPDATER=1

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates curl git gosu jq openssh-client python3 python3-pip \
    python3-venv ripgrep tmux unzip build-essential \
    && rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
      amd64) ttyd_arch=x86_64; ttyd_sha256="${TTYD_SHA256_AMD64}" ;; \
      arm64) ttyd_arch=aarch64; ttyd_sha256="${TTYD_SHA256_ARM64}" ;; \
      *) echo "Unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /usr/local/bin/ttyd \
      "https://github.com/tsl0922/ttyd/releases/download/${TTYD_VERSION}/ttyd.${ttyd_arch}"; \
    printf '%s  %s\n' "$ttyd_sha256" /usr/local/bin/ttyd | sha256sum -c -; \
    chmod +x /usr/local/bin/ttyd

RUN set -eux; \
    case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
      amd64) cs_arch=amd64; cs_sha256="${CODE_SERVER_SHA256_AMD64}" ;; \
      arm64) cs_arch=arm64; cs_sha256="${CODE_SERVER_SHA256_ARM64}" ;; \
      *) echo "Unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/code-server.deb \
      "https://github.com/coder/code-server/releases/download/v${CODE_SERVER_VERSION}/code-server_${CODE_SERVER_VERSION}_${cs_arch}.deb"; \
    printf '%s  %s\n' "$cs_sha256" /tmp/code-server.deb | sha256sum -c -; \
    apt-get update; \
    apt-get install -y /tmp/code-server.deb; \
    rm -f /tmp/code-server.deb; \
    rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    verify_npm_integrity() { \
      package="$1"; version="$2"; expected="$3"; \
      actual="$(npm view "${package}@${version}" dist.integrity)"; \
      test "$actual" = "$expected"; \
    }; \
    verify_npm_integrity @openai/codex "$CODEX_VERSION" "$CODEX_INTEGRITY"; \
    verify_npm_integrity @anthropic-ai/claude-code "$CLAUDE_CODE_VERSION" "$CLAUDE_CODE_INTEGRITY"; \
    verify_npm_integrity @earendil-works/pi-coding-agent "$PI_VERSION" "$PI_INTEGRITY"; \
    verify_npm_integrity opencode-ai "$OPENCODE_VERSION" "$OPENCODE_INTEGRITY"

# This image is Debian/glibc, so keep the glibc binaries and remove only
# musl fallbacks that npm installs for cross-platform package support.
RUN mkdir -p /opt/vibedock-cli \
    && npm install --prefix /opt/vibedock-cli --include=optional \
      "@openai/codex@${CODEX_VERSION}" \
      "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
      "opencode-ai@${OPENCODE_VERSION}" \
    && npm install --prefix /opt/vibedock-cli --ignore-scripts \
      "@earendil-works/pi-coding-agent@${PI_VERSION}" \
    && case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
         amd64) \
           rm -rf \
             /opt/vibedock-cli/node_modules/@anthropic-ai/claude-code-linux-x64-musl \
             /opt/vibedock-cli/node_modules/opencode-linux-x64-musl \
             /opt/vibedock-cli/node_modules/opencode-linux-x64-baseline-musl ;; \
         arm64) \
           rm -rf \
             /opt/vibedock-cli/node_modules/@anthropic-ai/claude-code-linux-arm64-musl \
             /opt/vibedock-cli/node_modules/opencode-linux-arm64-musl \
             /opt/vibedock-cli/node_modules/opencode-linux-arm64-baseline-musl ;; \
         *) echo "Unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
       esac \
    && npm cache clean --force

ENV PATH=/opt/vibedock-cli/node_modules/.bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
RUN set -eux; \
    node --version; \
    npm --version; \
    code-server --version; \
    codex --version; \
    claude --version; \
    pi --version; \
    opencode --version; \
    ttyd --version

RUN useradd --create-home --uid 1001 --shell /bin/bash coder \
    && mkdir -p /workspace /vibedock/credentials /home/coder/.config /home/coder/.local/bin /home/coder/.local/lib /home/coder/.local/share /home/coder/.vibedock/sessions \
    && chown -R coder:coder /workspace /vibedock/credentials /home/coder

# Keep ttyd/tmux and terminal applications in UTF-8 mode. Without a UTF-8
# locale tmux renders wide characters as underscores for browser clients.
ENV LANG=C.UTF-8 \
    LC_ALL=C.UTF-8

COPY entrypoint.sh /usr/local/bin/vibedock-entrypoint
COPY attach-session.sh /usr/local/bin/attach-session
COPY list-sessions.sh /usr/local/bin/list-sessions
COPY agent-runner.sh /usr/local/bin/vibedock-agent-runner
COPY auth-helper.py /usr/local/bin/vibedock-auth-helper
RUN chmod 0555 /usr/local/bin/vibedock-entrypoint /usr/local/bin/attach-session /usr/local/bin/list-sessions /usr/local/bin/vibedock-agent-runner /usr/local/bin/vibedock-auth-helper \
    && /usr/local/bin/vibedock-auth-helper protocol >/dev/null

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/vibedock-entrypoint"]
