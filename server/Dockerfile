# 归途（Homing）服务端一体化镜像
#
# 一个镜像包含全部组件，由 compose 用不同 command 分别启动：
#   homing  core  ...   隧道核心（内核隧道 + NAT + 客户端管理 API）
#   homing  stun  ...   打洞 / 中继
#   homing  admin       管理台
#   homing  plumb       可选出口：IPv4 策略路由 / DNS 劫持
#   homing  proxy       可选出口：远程代理模式的本机透明代理
#
# 运行时基于 docker:cli（管理台需要 docker CLI + compose），
# 同时包含 core 需要的 iptables / iproute2。
#
# 可选「远程代理」出口需要一个本机透明代理；这里把对应静态二进制一并打进
# 镜像，保证发布物仍然只有一个 homing 镜像。可用 --build-arg MIHOMO_IMAGE=... 固定版本。

ARG MIHOMO_IMAGE=metacubex/mihomo:v1.19.34
# 受限网络下可覆盖 Go 模块代理，例如 --build-arg GOPROXY=https://goproxy.cn,direct
ARG GOPROXY=https://proxy.golang.org,direct

# ---------- 构建 homing-core ----------
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build-core
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
ENV CGO_ENABLED=0
ARG GOPROXY
ENV GOPROXY=${GOPROXY}
COPY homing-core/go.mod homing-core/go.sum ./
RUN go mod download
COPY homing-core/*.go ./
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/homing-core .

# ---------- 构建 homing-stun ----------
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build-stun
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
ENV CGO_ENABLED=0
COPY stun-gateway/go.mod ./
COPY stun-gateway/*.go ./
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/gateway-stun .

# ---------- 构建 homing-admin ----------
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build-admin
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
ENV CGO_ENABLED=0
ARG GOPROXY
ENV GOPROXY=${GOPROXY}
COPY gateway-admin/go.mod gateway-admin/go.sum ./
RUN go mod download
COPY gateway-admin/main.go gateway-admin/ui.html ./
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/gateway-admin .

# ---------- 取代理内核静态二进制（仅用于 remote 模式的本机透明代理） ----------
FROM ${MIHOMO_IMAGE} AS mihomo

# ---------- 运行阶段 ----------
FROM docker:cli
ARG VERSION=dev
LABEL org.opencontainers.image.title="Homing" \
      org.opencontainers.image.description="Self-hosted dual-stack VPN gateway (core, hole punching, admin console)" \
      org.opencontainers.image.source="https://github.com/gjl6/homing-vpn" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="GPL-2.0-only"

RUN apk add --no-cache \
      iptables iproute2 \
      ca-certificates tzdata \
      runit tini libcap

COPY --from=build-core  /out/homing-core   /usr/local/bin/homing-core
COPY --from=build-stun  /out/gateway-stun  /usr/local/bin/gateway-stun
COPY --from=build-admin /out/gateway-admin /usr/local/bin/gateway-admin
COPY --from=mihomo      /mihomo            /usr/local/bin/mihomo

COPY docker/entrypoint.sh    /usr/local/bin/homing
COPY docker/plumber.sh       /usr/local/bin/homing-plumber
COPY docker/all-init.sh      /usr/local/bin/homing-init
COPY docker/homing-health    /usr/local/bin/homing-health
COPY docker/proxy-runner.sh  /usr/local/bin/homing-proxy-runner
COPY docker/stun-runner.sh   /usr/local/bin/homing-stun-runner
COPY docker/plumb-runner.sh  /usr/local/bin/homing-plumb-runner
COPY docker/core-port.sh     /usr/local/bin/homing-core-port
COPY docker/nocaps.sh        /usr/local/bin/homing-nocaps
COPY docker/capdrop.sh       /usr/local/bin/homing-capdrop
COPY docker/services         /etc/homing/services

RUN chmod +x \
      /usr/local/bin/homing \
      /usr/local/bin/homing-plumber \
      /usr/local/bin/mihomo \
      /usr/local/bin/homing-init \
      /usr/local/bin/homing-health \
      /usr/local/bin/homing-proxy-runner \
      /usr/local/bin/homing-stun-runner \
      /usr/local/bin/homing-plumb-runner \
      /usr/local/bin/homing-core-port \
      /usr/local/bin/homing-nocaps \
      /usr/local/bin/homing-capdrop \
      /etc/homing/services/*/run

# tini 作为所有模式的 PID 1：负责收尸和信号转发。
# all 模式下 homing-init 再用 runit 监督子进程；分容器模式行为不变。
ENTRYPOINT ["/sbin/tini", "-g", "--", "/usr/local/bin/homing"]
