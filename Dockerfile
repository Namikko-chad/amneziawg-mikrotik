# syntax=docker/dockerfile:1
ARG ALPINE_VERSION=3.22
ARG GO_VERSION=1.26

# --- Go binaries: cross-compiled on the build host -------------------------
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS go-build
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG AWG_GO_VERSION=v3.1.20260828
RUN apk add --no-cache git
ENV CGO_ENABLED=0
RUN git clone --depth 1 --branch ${AWG_GO_VERSION} https://github.com/amnezia-vpn/amneziawg-go /src/amneziawg-go
WORKDIR /src/amneziawg-go
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags "-s -w" -o /out/amneziawg-go .

WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod go mod download
COPY backend/ ./
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags "-s -w" -o /out/awg-manager .

# --- awg tool (C), built for the target platform ---------------------------
FROM alpine:${ALPINE_VERSION} AS tools-build
ARG AWG_TOOLS_VERSION=v3.1.20260812
RUN apk add --no-cache git build-base linux-headers
RUN git clone --depth 1 --branch ${AWG_TOOLS_VERSION} https://github.com/amnezia-vpn/amneziawg-tools /src
RUN make -C /src/src -j"$(nproc)" wg && strip /src/src/wg

# --- runtime ---------------------------------------------------------------
FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache nginx iproute2 iptables iptables-legacy \
    && rm -rf /etc/nginx/http.d /var/www
COPY --from=go-build /out/amneziawg-go /out/awg-manager /usr/bin/
COPY --from=tools-build /src/src/wg /usr/bin/awg
COPY docker/nginx.conf /etc/nginx/nginx.conf
COPY docker/entrypoint.sh /entrypoint.sh
COPY web/ /usr/share/nginx/html/
RUN chmod +x /entrypoint.sh

ENV AWG_DATA_DIR=/etc/amnezia
VOLUME /etc/amnezia
EXPOSE 80
ENTRYPOINT ["/entrypoint.sh"]
