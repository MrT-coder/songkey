# Runtime-only image: the Go binary is cross-compiled on a development machine
# and copied in, so the server never downloads a toolchain or runs a compile.
#
# Build the binary first (see build.sh / build.ps1), then:
#   docker compose -f songkey.yml up -d --build
#
# Debian rather than Alpine because the standalone yt-dlp build is linked
# against glibc and will not run on musl.
FROM debian:bookworm-slim

# yt-dlp ships as a self-contained binary, so the image needs no Python runtime
# of its own. It is only executed on demand and never stays resident.
#
# The version is deliberately "latest": YouTube changes break older builds
# within weeks, so pinning turns into a broken service. Rebuild the image to
# pick up a newer one. The service also falls back to YouTube's oEmbed API when
# the extractor fails, so a stale binary degrades instead of blocking.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && curl -fsSL -o /usr/local/bin/yt-dlp \
        "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_linux" \
    && chmod +x /usr/local/bin/yt-dlp \
    && apt-get purge -y curl \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*

COPY bin/songkey /usr/local/bin/songkey
RUN chmod +x /usr/local/bin/songkey

# The catalog lives here and must be mounted, otherwise the duplicate index is
# lost every time the container is recreated.
VOLUME ["/data"]

EXPOSE 8090

ENTRYPOINT ["/usr/local/bin/songkey"]
