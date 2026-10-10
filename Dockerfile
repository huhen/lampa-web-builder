# syntax=docker/dockerfile:1

# Stage 1: build the static server binary.
FROM golang:1.27 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION}" -o /out/lampa-web-builder .

# Stage 2: runtime with the node toolchain and git for the build pipeline.
# Runs as an unprivileged user (issue #8): /data is created with builder as its
# owner, so a fresh named volume inherits that ownership when Docker populates
# it. HOME must point at the builder's home or npm writes its cache to /root.
FROM node:24-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --gid 10001 builder \
 && useradd --uid 10001 --gid 10001 --create-home --shell /usr/sbin/nologin builder \
 && mkdir -p /data \
 && chown builder:builder /data
COPY --from=build /out/lampa-web-builder /usr/local/bin/lampa-web-builder
WORKDIR /app
COPY patches/ /app/patches/
COPY overlay/ /app/overlay/
COPY package-lock.json /app/package-lock.json
ENV DATA_DIR=/data \
    ASSETS_DIR=/app \
    HOME=/home/builder
VOLUME /data
EXPOSE 8080
USER builder
ENTRYPOINT ["lampa-web-builder"]
