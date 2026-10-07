# syntax=docker/dockerfile:1
#
# Build from the repository root: docker build -f .infra/docker/Dockerfile .
# The build context whitelist is .infra/docker/Dockerfile.dockerignore.
#
# Stages (ADR 0019):
#   natives        csdr (libcsdr++) and owrx_connector from pinned tarballs
#   base → dev     Go toolchain + C/C++ toolchain + natives (Air, linters)
#   base → build   cgo binaries: meshsdr (hub, node, all) and meshsdr-node
#   runtime        Debian slim, nonroot (65532), runtime libraries + natives
#   runtime → prod, prod-node
#   sources        GPL sources of the bundled native components (OCI artifact)

ARG GO_VERSION=1.26
# golang:1.26-trixie (index digest); bump with GO_VERSION.
ARG GO_IMAGE=golang:${GO_VERSION}-trixie@sha256:aae9c439b447d24c93ee483408992857cf39a3fb6ab7fe53affc65d834891fa8
ARG DEBIAN_IMAGE=debian:trixie-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132

# Debian package pins (trixie). Bump together with DEBIAN_IMAGE.
ARG FFTW_VERSION=3.3.10-2+b1
ARG SAMPLERATE_VERSION=0.2.2-4+b2
ARG RTLSDR_VERSION=2.0.2-2+b1
ARG LIBUSB_VERSION=2:1.0.28-1
ARG CA_CERTIFICATES_VERSION=20250419

# Native components built from source (ADR 0014, ADR 0019). Upstream
# publishes no release tarballs: the GitHub tag archives are pinned by
# sha256.
ARG CSDR_VERSION=0.18.41
ARG CSDR_SHA256=aa903a61db5af280bf65aef75f0d850c68552621a086989ef86778294dff511a
ARG OWRX_CONNECTOR_VERSION=0.6.5
ARG OWRX_CONNECTOR_SHA256=63a7bccf5beab0220136829850b46df56c79ddb4b6d77d9ea191d1fd527ed7ec


# csdr 0.18.x (luarvique fork) and owrx_connector (rtl_connector,
# rtl_tcp_connector; no Soapy), installed under /usr/local and staged in
# /opt/natives with their licence texts.
FROM ${DEBIAN_IMAGE} AS natives

SHELL ["/bin/bash", "-euxo", "pipefail", "-c"]

ARG FFTW_VERSION
ARG SAMPLERATE_VERSION
ARG RTLSDR_VERSION
ARG CSDR_VERSION
ARG CSDR_SHA256
ARG OWRX_CONNECTOR_VERSION
ARG OWRX_CONNECTOR_SHA256

RUN apt-get update; \
    apt-get install -y --no-install-recommends \
      ca-certificates curl g++ cmake make pkg-config \
      "libfftw3-dev=${FFTW_VERSION}" "libsamplerate0-dev=${SAMPLERATE_VERSION}" "librtlsdr-dev=${RTLSDR_VERSION}"; \
    rm -rf /var/lib/apt/lists/*

WORKDIR /src

RUN curl -fsSL -o csdr.tar.gz "https://github.com/luarvique/csdr/archive/refs/tags/${CSDR_VERSION}.tar.gz"; \
    echo "${CSDR_SHA256}  csdr.tar.gz" | sha256sum -c -; \
    curl -fsSL -o owrx_connector.tar.gz "https://github.com/luarvique/owrx_connector/archive/refs/tags/${OWRX_CONNECTOR_VERSION}.tar.gz"; \
    echo "${OWRX_CONNECTOR_SHA256}  owrx_connector.tar.gz" | sha256sum -c -; \
    tar xzf csdr.tar.gz; \
    tar xzf owrx_connector.tar.gz; \
    cmake -S "csdr-${CSDR_VERSION}" -B build-csdr -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr/local; \
    cmake --build build-csdr -j"$(nproc)"; \
    cmake --install build-csdr; \
    ldconfig; \
    cmake -S "owrx_connector-${OWRX_CONNECTOR_VERSION}" -B build-owrx -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr/local; \
    cmake --build build-owrx -j"$(nproc)"; \
    cmake --install build-owrx; \
    out=/opt/natives; \
    mkdir -p "${out}/usr/local/lib/pkgconfig" "${out}/usr/local/include" "${out}/usr/local/bin" \
      "${out}/usr/share/doc/csdr" "${out}/usr/share/doc/owrx-connector"; \
    cp -a /usr/local/lib/libcsdr++.so* /usr/local/lib/libowrx-connector.so* "${out}/usr/local/lib/"; \
    cp -a /usr/local/lib/pkgconfig/csdr.pc "${out}/usr/local/lib/pkgconfig/"; \
    cp -a /usr/local/include/csdr "${out}/usr/local/include/"; \
    cp -a /usr/local/bin/rtl_connector /usr/local/bin/rtl_tcp_connector "${out}/usr/local/bin/"; \
    cp "csdr-${CSDR_VERSION}/LICENSE-GPL" "${out}/usr/share/doc/csdr/"; \
    cp "csdr-${CSDR_VERSION}/README.md" "${out}/usr/share/doc/csdr/"; \
    cp "owrx_connector-${OWRX_CONNECTOR_VERSION}/LICENSE" "${out}/usr/share/doc/owrx-connector/"


FROM ${GO_IMAGE} AS base

SHELL ["/bin/bash", "-euxo", "pipefail", "-c"]

ARG TARGETARCH
ARG TAILWIND_VERSION=4.3.3
ARG FFTW_VERSION
ARG SAMPLERATE_VERSION
ARG RTLSDR_VERSION
ARG LIBUSB_VERSION

# cgo (ADR 0014): g++, libcsdr++ and its libraries; librtlsdr for the
# connectors run by the dev stack and the tests.
ENV GOCACHE=/cache/go-build \
    GOMODCACHE=/cache/go-mod \
    CGO_ENABLED=1

RUN apt-get update; \
    apt-get install -y --no-install-recommends g++ \
      "libfftw3-dev=${FFTW_VERSION}" "libsamplerate0-dev=${SAMPLERATE_VERSION}" "librtlsdr0=${RTLSDR_VERSION}" \
      "libusb-1.0-0=${LIBUSB_VERSION}"; \
    rm -rf /var/lib/apt/lists/*

COPY --from=natives /opt/natives/usr/local/ /usr/local/

RUN ldconfig; \
    case "${TARGETARCH}" in \
      amd64) tw_arch=x64 ;; \
      arm64) tw_arch=arm64 ;; \
      *) echo "unsupported arch: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /usr/local/bin/tailwindcss \
      "https://github.com/tailwindlabs/tailwindcss/releases/download/v${TAILWIND_VERSION}/tailwindcss-linux-${tw_arch}"; \
    chmod +x /usr/local/bin/tailwindcss; \
    mkdir -p /cache

WORKDIR /app


# Dev + DevContainer: Air, LSP (gopls, templ), debugger, linter. Runs as host UID/GID.
FROM base AS dev

ARG TARGETARCH
ARG GOLANGCI_LINT_VERSION=2.14.0
ARG GOPLS_VERSION=0.23.0
ARG DELVE_VERSION=1.27.2
ARG UID=1000
ARG GID=1000

# Root-installed binaries, built with throwaway caches so /cache stays empty for the dev user
RUN --mount=type=cache,target=/tmp/go-cache,sharing=locked \
    export GOCACHE=/tmp/go-cache/build GOMODCACHE=/tmp/go-cache/mod GOBIN=/usr/local/bin; \
    go install "golang.org/x/tools/gopls@v${GOPLS_VERSION}"; \
    go install "github.com/go-delve/delve/cmd/dlv@v${DELVE_VERSION}"; \
    curl -fsSL "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-${TARGETARCH}.tar.gz" \
      | tar -xz -C /usr/local/bin --strip-components=1 "golangci-lint-${GOLANGCI_LINT_VERSION}-linux-${TARGETARCH}/golangci-lint"

# -o: tolerate host UID/GID already used in the base image.
# /go is chowned recursively: root's installs above leave /go/pkg/sumdb behind,
# which `go get` must write as the dev user.
# /run/meshsdr-node is node.runtime_dir of the dev node (0700, dev user).
RUN groupadd -o -g "${GID}" app; \
    useradd -o -m -u "${UID}" -g "${GID}" -s /bin/bash app; \
    chown app:app /app /cache; \
    chown -R app:app /go; \
    install -d -m 0700 -o app -g app /run/meshsdr-node

USER app

ENV GOLANGCI_LINT_CACHE=/cache/golangci-lint

# go.mod tools (templ, sqlc, goose, air, oapi-codegen) in PATH (/go/bin), templ LSP needs the binary
COPY --chown=app:app go.mod go.sum ./
RUN go mod download; \
    go install tool

CMD ["air", "-c", ".infra/docker/dev/air.toml"]


FROM base AS build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Two artifacts (ADR 0012), both cgo (ADR 0014): meshsdr with the embedded
# gateway (hub, node, all) and meshsdr-node without it (-tags nogateway,
# node role only). /etc/meshsdr/tls links to the data volume: the CA and
# node certificates written by `meshsdr all` and `meshsdr node enroll`
# survive the container. The licence texts of the Go modules linked into
# the binaries are collected for the image notices.
RUN --mount=type=cache,target=/cache/go-build \
    go generate ./...; \
    go build -trimpath -ldflags="-s -w" -o /out/meshsdr ./cmd/meshsdr; \
    go build -tags nogateway -trimpath -ldflags="-s -w" -o /out/meshsdr-node ./cmd/meshsdr; \
    if go list -deps -tags nogateway ./cmd/meshsdr | grep -q caddyserver; then \
      echo "the nogateway build links Caddy" >&2; exit 1; \
    fi; \
    mkdir -p /out/var/lib/meshsdr/tls /out/etc/meshsdr /out/usr/share/doc/meshsdr; \
    ln -s /var/lib/meshsdr/tls /out/etc/meshsdr/tls; \
    go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' ./cmd/meshsdr | sort -u | \
      while read -r path version dir; do \
        printf '\n==== %s %s ====\n' "${path}" "${version}"; \
        for f in "${dir}"/LICENSE* "${dir}"/LICENCE* "${dir}"/COPYING* "${dir}"/NOTICE*; do \
          if [ -f "${f}" ]; then printf -- '--- %s\n' "$(basename "${f}")"; cat "${f}"; fi; \
        done; \
      done > /out/usr/share/doc/meshsdr/go-modules.txt


# Runtime of every image (ADR 0019): Debian slim, nonroot uid/gid 65532
# (the distroless nonroot ids, so existing volumes keep their owner).
FROM ${DEBIAN_IMAGE} AS runtime

SHELL ["/bin/bash", "-euxo", "pipefail", "-c"]

ARG FFTW_VERSION
ARG SAMPLERATE_VERSION
ARG RTLSDR_VERSION
ARG LIBUSB_VERSION
ARG CA_CERTIFICATES_VERSION

RUN apt-get update; \
    apt-get install -y --no-install-recommends \
      "ca-certificates=${CA_CERTIFICATES_VERSION}" "libfftw3-single3=${FFTW_VERSION}" \
      "libsamplerate0=${SAMPLERATE_VERSION}" "librtlsdr0=${RTLSDR_VERSION}" "libusb-1.0-0=${LIBUSB_VERSION}"; \
    rm -rf /var/lib/apt/lists/*; \
    groupadd -g 65532 nonroot; \
    useradd -u 65532 -g 65532 -d /home/nonroot -m -s /usr/sbin/nologin nonroot; \
    install -d -m 0700 -o nonroot -g nonroot /run/meshsdr-node

COPY --from=natives /opt/natives/ /
COPY .infra/docker/prod/THIRD_PARTY_NOTICES /usr/share/doc/meshsdr/THIRD_PARTY_NOTICES
COPY LICENSE /usr/share/doc/meshsdr/LICENSE
COPY --from=build /out/usr/share/doc/meshsdr/go-modules.txt /usr/share/doc/meshsdr/go-modules.txt

RUN ldconfig


# Hub, node or all. Default: all (hub + local node). The gateway listens on
# :443 with ACME for the host of hub.url (gateway.* keys to change it).
# Run with `init: true` (docker run --init): the node supervises connector
# processes (ADR 0017).
FROM runtime AS prod

COPY --from=build --chown=nonroot:nonroot /out/var/lib/meshsdr /var/lib/meshsdr
COPY .infra/docker/prod/etc/meshsdr /etc/meshsdr
COPY --from=build /out/etc/meshsdr/ /etc/meshsdr/
COPY --from=build /out/meshsdr /meshsdr

USER nonroot

VOLUME /var/lib/meshsdr
# gateway.https_listen (gateway.http_listen when set, :80)
EXPOSE 443

ENTRYPOINT ["/meshsdr"]
CMD ["all"]


# Node only, without the gateway (published with the -node tag suffix).
FROM runtime AS prod-node

COPY --from=build --chown=nonroot:nonroot /out/var/lib/meshsdr /var/lib/meshsdr
COPY .infra/docker/prod/etc/meshsdr /etc/meshsdr
COPY --from=build /out/etc/meshsdr/ /etc/meshsdr/
COPY --from=build /out/meshsdr-node /meshsdr

USER nonroot

VOLUME /var/lib/meshsdr
# node.listen
EXPOSE 8074

ENTRYPOINT ["/meshsdr"]
CMD ["node"]


# Corresponding sources of the GPL components bundled in the images (§8.7
# licensing note 3): published per release as the -sources tag.
FROM ${DEBIAN_IMAGE} AS sources-fetch

SHELL ["/bin/bash", "-euxo", "pipefail", "-c"]

ARG FFTW_VERSION
ARG SAMPLERATE_VERSION
ARG RTLSDR_VERSION
ARG LIBUSB_VERSION
ARG CSDR_VERSION
ARG CSDR_SHA256
ARG OWRX_CONNECTOR_VERSION
ARG OWRX_CONNECTOR_SHA256

# Debian source versions are the binary versions without the binNMU suffix.
RUN sed -i 's/^Types: deb$/Types: deb deb-src/' /etc/apt/sources.list.d/debian.sources; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl dpkg-dev; \
    mkdir -p /sources/debian; \
    cd /sources; \
    curl -fsSL -o "csdr-${CSDR_VERSION}.tar.gz" "https://github.com/luarvique/csdr/archive/refs/tags/${CSDR_VERSION}.tar.gz"; \
    echo "${CSDR_SHA256}  csdr-${CSDR_VERSION}.tar.gz" | sha256sum -c -; \
    curl -fsSL -o "owrx_connector-${OWRX_CONNECTOR_VERSION}.tar.gz" "https://github.com/luarvique/owrx_connector/archive/refs/tags/${OWRX_CONNECTOR_VERSION}.tar.gz"; \
    echo "${OWRX_CONNECTOR_SHA256}  owrx_connector-${OWRX_CONNECTOR_VERSION}.tar.gz" | sha256sum -c -; \
    cd /sources/debian; \
    apt-get source --download-only "fftw3=${FFTW_VERSION%+b*}" "rtl-sdr=${RTLSDR_VERSION%+b*}" \
      "libsamplerate=${SAMPLERATE_VERSION%+b*}" "libusb-1.0=${LIBUSB_VERSION%+b*}"; \
    rm -rf /var/lib/apt/lists/*

# The source of MeshSDR itself (the files the binaries are built from),
# next to the third-party sources.
WORKDIR /meshsdr
COPY go.mod go.sum LICENSE ./
COPY cmd ./cmd
COPY internal ./internal
COPY .infra/docker/prod ./.infra/docker/prod
RUN tar -czf /sources/meshsdr-src.tar.gz -C /meshsdr .

FROM scratch AS sources

COPY --from=sources-fetch /sources/ /sources/
