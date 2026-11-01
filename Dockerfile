# Ether — multi-stage build.
#   docker build -t ether .
#   docker run --network host -v ether-config:/config -v ether-cache:/cache ether
# Host networking is recommended: Plex clients discover the server via GDM
# (UDP 32410-32414) and remote access uses UPnP, neither of which survive
# port-mapped bridge networking. Bridge mode works for Plex Web if you map
# -p 32400:32400. See docs/install.md.
#
# Bundled ffmpeg: the pinned jellyfin-ffmpeg portable build (GPL) staged by
# scripts/fetch-ffmpeg.sh into dist/ffmpeg/<os>-<arch>. Build with
#   scripts/fetch-ffmpeg.sh linux-$(go env GOARCH) && docker build .
# docker buildx per-arch builds pass TARGETARCH automatically.

# ---- build the server (pure Go) -------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev
ARG COMMIT=
ARG ETHER_UPDATE_PUBLIC_KEY=
ARG ETHER_TMDB_KEY=
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
      -trimpath \
      -ldflags "-s -w \
        -X github.com/braedonsaunders/ether/internal/updater.Version=${VERSION} \
        -X github.com/braedonsaunders/ether/internal/updater.Commit=${COMMIT} \
        -X github.com/braedonsaunders/ether/internal/updater.PublicKeyB64=${ETHER_UPDATE_PUBLIC_KEY} \
        -X github.com/braedonsaunders/ether/internal/tmdb.BundledKey=${ETHER_TMDB_KEY}" \
      -o /out/ether ./cmd/ether

# ---- runtime ----------------------------------------------------------------
# Debian (not Alpine): the jellyfin-ffmpeg portable build and VAAPI/QSV
# userspace drivers are glibc-based. Trixie ships libva 2.22, which
# provides vaMapBuffer2. Bookworm's libva 2.17 does not, and the pinned
# Jellyfin FFmpeg 8.1.3 fails to load VAAPI against it.
FROM debian:trixie-slim AS runtime
ARG TARGETARCH

# Hardware-transcode userspace, loaded by the bundled ffmpeg at runtime:
# libva + libva-drm (VAAPI on /dev/dri), Mesa VA drivers (AMD), and on amd64
# Intel's non-free iHD (Gen8+: full EU kernel shaders for VBR/CBR/QVBR,
# VAEntrypointEncSlice and VAEntrypointVideoProc) and i965 (older Gen) VA
# drivers plus the oneVPL QuickSync runtime (Debian ships those for amd64
# only). Note: Debian's free `intel-media-va-driver` strips EU shader kernels,
# leaving Gen9/Gen10 with only low-power CQP and no video processing;
# `intel-media-va-driver-non-free` and `i965-va-driver-shaders` live in
# non-free-firmware / non-free. Keep in sync with
# packaging/docker/Dockerfile.goreleaser.*.
RUN sed -i 's/^Components: main$/Components: main contrib non-free non-free-firmware/' /etc/apt/sources.list.d/debian.sources \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates tzdata \
      libva2 libva-drm2 vainfo mesa-va-drivers \
      ocl-icd-libopencl1 \
 && if [ "$TARGETARCH" = amd64 ]; then \
      apt-get install -y --no-install-recommends intel-media-va-driver-non-free i965-va-driver-shaders libmfx-gen1.2; \
    fi \
 && rm -rf /var/lib/apt/lists/*

# Fixed UID/GID 999: what --system allocation gave every earlier image on
# this base (the highest free system id), pinned so a future image can
# still read an existing 0700 /config volume. Keep in sync with
# packaging/docker/Dockerfile.goreleaser.*.
RUN groupadd --system --gid 999 ether \
 && useradd --system --uid 999 --gid ether --home-dir /config --shell /usr/sbin/nologin ether \
 && mkdir -p /config /cache /usr/lib/ether \
 && chown -R ether:ether /config /cache

COPY --from=build /out/ether /usr/lib/ether/ether
# ffmpeg/ffprobe next to the binary, where internal/transcode looks first.
# Requires scripts/fetch-ffmpeg.sh to have staged dist/ffmpeg/ (build fails
# loudly otherwise — never ship an image that cannot transcode silently).
COPY dist/ffmpeg/linux-${TARGETARCH}/ /usr/lib/ether/
COPY LICENSE THIRD_PARTY_NOTICES.md /usr/share/doc/ether/
COPY packaging/docker/entrypoint.sh /usr/lib/ether/entrypoint

ENV ETHER_DATA_DIR=/config \
    XDG_CACHE_HOME=/cache

# Plex API + web UI. GDM discovery (32410-32414/udp) and DLNA-ish ports only
# work with host networking; EXPOSE is documentation here.
EXPOSE 32400/tcp 32410-32414/udp
VOLUME ["/config", "/cache"]

# Starts as root only to hand the volumes to PUID:PGID (default 999, the
# ether account) and drop privileges; see packaging/docker/entrypoint.sh.
WORKDIR /config
ENTRYPOINT ["/usr/lib/ether/entrypoint"]
