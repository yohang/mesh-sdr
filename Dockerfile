# syntax=docker/dockerfile:1

ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-trixie AS base

SHELL ["/bin/bash", "-euxo", "pipefail", "-c"]

ARG TARGETARCH
ARG TAILWIND_VERSION=4.3.3

ENV GOCACHE=/cache/go-build \
    GOMODCACHE=/cache/go-mod

RUN case "${TARGETARCH}" in \
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

# -o: tolerate host UID/GID already used in the base image
RUN groupadd -o -g "${GID}" app; \
    useradd -o -m -u "${UID}" -g "${GID}" -s /bin/bash app; \
    chown app:app /app /cache /go /go/bin

USER app

ENV GOLANGCI_LINT_CACHE=/cache/golangci-lint

# go.mod tools (templ, sqlc, goose, air) in PATH (/go/bin), templ LSP needs the binary
COPY --chown=app:app go.mod go.sum ./
RUN go mod download; \
    go install tool

CMD ["air"]


FROM base AS build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN --mount=type=cache,target=/cache/go-build \
    go generate ./...; \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/meshsdr ./cmd/meshsdr; \
    mkdir -p /out/data


FROM gcr.io/distroless/static-debian13:nonroot AS prod

COPY --from=build --chown=nonroot:nonroot /out/data /data
COPY --from=build /out/meshsdr /meshsdr

VOLUME /data
EXPOSE 3000

ENTRYPOINT ["/meshsdr"]
CMD ["serve"]
