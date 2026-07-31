# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, so a source change does not re-download the module cache.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
# CGO stays off: the binary is fully static, so the runtime stage needs no libc.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/yandex-bridge ./cmd/yandex-bridge

FROM alpine:3.20

# ca-certificates for the TLS calls to oauth.yandex.ru and api.iot.yandex.net;
# tzdata so log timestamps make sense in the user's own timezone.
RUN apk add --no-cache ca-certificates tzdata wget

COPY --from=build /out/yandex-bridge /usr/local/bin/yandex-bridge

# The bridge runs unprivileged. The HAP port is above 1024 and mDNS needs no
# privileges, so there is nothing here that requires root.
RUN adduser -D -u 10001 bridge && mkdir -p /data && chown bridge:bridge /data
USER bridge

VOLUME ["/data"]
ENV DATA_DIR=/data

# 51826/tcp is HAP; 5353/udp is mDNS. Both only work with host networking —
# see compose.yaml.
EXPOSE 51826/tcp 5353/udp 8080/tcp

HEALTHCHECK --interval=60s --timeout=5s --start-period=30s --retries=3 \
    CMD wget --quiet --tries=1 --spider http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/yandex-bridge"]
