# syntax=docker/dockerfile:1.7
#
# PulseGuard 多阶段镜像
#   Stage 1 (builder)  : 使用 golang:1.25-alpine 编译静态二进制
#   Stage 2 (runtime)  : 使用 scratch 仅承载二进制 + ca-certificates，体积最小
#
# 构建：
#   docker build -t pulseguard:dev .
#
# 运行：
#   docker run -d --name pulseguard -p 8080:8080 \
#       -v $(pwd)/data:/var/lib/pulseguard \
#       -e PULSEGUARD_SECURITY_MASTER_KEY_B64=$(openssl rand -base64 32) \
#       pulseguard:dev

# ──────────────────────────────────────────────────────────────────────
# Stage 1: builder
# ──────────────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

# ca-certificates 用于 stage 2 COPY；git 偶尔用于 module 抓取
RUN apk add --no-cache ca-certificates git

WORKDIR /src

# 先 copy 依赖描述以利用层缓存
COPY go.mod go.sum ./
RUN go mod download

# 拷贝其余源码（.dockerignore 已排除 git/docs/audit 等）
COPY . .

# 纯静态 Go 二进制：
#   CGO_ENABLED=0   使用纯 Go modernc.org/sqlite，免 cgo
#   -trimpath        去除本地路径
#   -ldflags "-s -w" 去除符号表 / DWARF，体积下降约 25%
ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOFLAGS=-mod=readonly

RUN go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /out/pulseguard \
        ./cmd/pulseguard

# ──────────────────────────────────────────────────────────────────────
# Stage 2: runtime (scratch)
# ──────────────────────────────────────────────────────────────────────
FROM scratch AS runtime

# 调 Telegram HTTPS API 需要 root CA 链
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# 业务二进制
COPY --from=builder /out/pulseguard /pulseguard

# SQLite 数据目录（生产以 -v 卷挂载持久化）
VOLUME ["/var/lib/pulseguard"]

# HTTP 端口
EXPOSE 8080

# 默认 config 路径；env PULSEGUARD_* 仍可覆盖
ENTRYPOINT ["/pulseguard", "-config", "/etc/pulseguard/config.yaml"]
