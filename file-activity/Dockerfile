# syntax=docker/dockerfile:1
# Build context is the root of the repository: the extension is built together
# with the common module it replaces to ../common.
#   docker build -f file-activity/Dockerfile .

ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.22

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0 GOWORK=off
WORKDIR /src/file-activity

COPY common/go.mod common/go.sum /src/common/
COPY file-activity/go.mod file-activity/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY common /src/common
COPY file-activity .

ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/file-activity ./cmd/file-activity

FROM alpine:${ALPINE_VERSION}

RUN apk add --no-cache ca-certificates && adduser -D -u 1000 file-activity

COPY --from=build /out/file-activity /usr/local/bin/file-activity

USER 1000:1000
EXPOSE 9201

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O - http://127.0.0.1:9201/healthz || exit 1

ENTRYPOINT ["file-activity"]
CMD ["serve"]
