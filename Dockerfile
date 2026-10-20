# syntax=docker/dockerfile:1

# Frontend built from the sibling checkout. BuildKit resolves it through the
# named build context `frontend`; Compose maps it to ../frontend, and a manual
# build passes --build-context frontend=../frontend. The `full-prebuilt` target
# instead expects a context that already contains dist/, which the release
# workflow feeds from the frontend tag build.
FROM --platform=$BUILDPLATFORM node:24-bookworm-slim AS web
WORKDIR /app
RUN corepack enable
COPY --from=frontend package.json pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY --from=frontend . .
RUN pnpm build

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}" \
    -o /out/mailquay-core ./cmd/mailquay

FROM build AS build-full
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
COPY --from=web /app/dist /src/internal/webassets/dist
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -tags full \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}" \
    -o /out/mailquay ./cmd/mailquay

FROM build AS build-full-prebuilt
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
COPY --from=frontend dist /src/internal/webassets/dist
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -tags full \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}" \
    -o /out/mailquay ./cmd/mailquay

FROM alpine:3.22 AS base
RUN apk add --no-cache ca-certificates tzdata && addgroup -S mailquay && adduser -S -G mailquay mailquay \
    && mkdir -p /data && chown mailquay:mailquay /data
WORKDIR /app
USER mailquay
ENV MAILQUAY_DATA_DIR=/data MAILQUAY_LISTEN_ADDR=:8080
EXPOSE 8080

FROM base AS core
COPY --from=build /out/mailquay-core /usr/local/bin/mailquay-core
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s CMD ["/usr/local/bin/mailquay-core", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/mailquay-core", "serve"]

FROM base AS full
COPY --from=build-full /out/mailquay /usr/local/bin/mailquay
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s CMD ["/usr/local/bin/mailquay", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/mailquay", "serve"]

FROM base AS full-prebuilt
COPY --from=build-full-prebuilt /out/mailquay /usr/local/bin/mailquay
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s CMD ["/usr/local/bin/mailquay", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/mailquay", "serve"]
