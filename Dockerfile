# Multi-stage build: compile the static (CGO-free) dcms binary, then ship it on a
# minimal distroless base. Pure-Go SQLite means no C toolchain and no libc in the
# final image.
#
#   docker build -t dcms .
#   docker run -p 8080:8080 -v "$PWD:/data" -w /data \
#     -e DCMS_ADMIN_EMAIL=you@example.com -e DCMS_ADMIN_PASSWORD=change-me dcms dev
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
WORKDIR /src

# Cache modules first for faster rebuilds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# TARGETOS/TARGETARCH are provided by buildx for multi-arch builds.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.0.0-dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/dcms ./cmd/dcms

# distroless/static: no shell, no package manager — just the binary + CA certs.
# Runs as root so a bind-mounted project directory (holding the schema, config
# and SQLite database) is writable out of the box; add `--user 65532` to run
# unprivileged when the mounted dir is owned accordingly.
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/dcms /usr/local/bin/dcms

# Content, config and the SQLite database live here; mount a volume to persist.
WORKDIR /data
EXPOSE 8080

ENTRYPOINT ["dcms"]
CMD ["dev"]
