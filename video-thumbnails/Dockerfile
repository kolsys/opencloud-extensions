# syntax=docker/dockerfile:1
# Build context is the root of the repository: the extension is built together
# with the common module it replaces to ../common, and with its web app.
#   docker build -f video-thumbnails/Dockerfile .

ARG GO_VERSION=1.26
ARG NODE_VERSION=22
ARG ALPINE_VERSION=3.22

# The web app of the extension, embedded into the binary below.
FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-alpine AS web

WORKDIR /src/web-app
COPY video-thumbnails/web-app/package.json video-thumbnails/web-app/package-lock.json video-thumbnails/web-app/.npmrc ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY video-thumbnails/web-app .
# The build lands in ../internal/web/dist, see vite.config.ts.
RUN npm run check && npm run build

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0 GOWORK=off
WORKDIR /src/video-thumbnails

COPY common/go.mod common/go.sum /src/common/
COPY video-thumbnails/go.mod video-thumbnails/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY common /src/common
COPY video-thumbnails .
COPY --from=web /src/internal/web/dist ./internal/web/dist

ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/video-thumbnails ./cmd/video-thumbnails

FROM alpine:${ALPINE_VERSION}

RUN apk add --no-cache ca-certificates ffmpeg \
    && adduser -D -u 1000 video-thumbnails \
    && mkdir -p /var/cache/video-thumbnails \
    && chown video-thumbnails:video-thumbnails /var/cache/video-thumbnails

COPY --from=build /out/video-thumbnails /usr/local/bin/video-thumbnails

USER 1000:1000
EXPOSE 9200
VOLUME /var/cache/video-thumbnails

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O - http://127.0.0.1:9200/healthz || exit 1

ENTRYPOINT ["video-thumbnails"]
CMD ["serve"]
