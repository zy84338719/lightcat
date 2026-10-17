# syntax=docker/dockerfile:1
#
# LightCat 光猫管家 —— 容器镜像
#
# 构建：docker build -t lightcat:v1.3.0 .
# 运行：docker run -d --name lightcat -p 7547:7547 -v ./data:/data lightcat:v1.3.0
#       （面板与 CWMP 默认共用 7547；生产部署建议看 docker-compose.yml 与 deploy/DOCKER.md）
#
# 设计约束：
#   - 单静态二进制（CGO_ENABLED=0，modernc.org/sqlite 纯 Go），运行时只需要 alpine 基座；
#   - 数据只有一个目录 /data（SQLite 库 + 固件文件），备份 = 拷走这个目录；
#   - 非 root 运行（uid/gid 10001）；K8s 挂卷权限处理见 deploy/k8s/statefulset.yaml。

# ---- 构建段 ----
# 固定在构建机架构上跑（Apple Silicon / amd64 CI 都一样），Go 交叉编译出目标架构，
# 不需要 qemu 模拟，多架构构建快且稳。
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
# 模块代理：默认走 goproxy.cn（国内秒达，海外也可用），proxy.golang.org 兜底。
# 海外/内网自建代理可覆盖：docker build --build-arg GOPROXY=https://proxy.golang.org,direct .
ARG GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct
ENV GOPROXY=$GOPROXY
# 依赖层单独缓存：只有 go.mod / go.sum 变了才重新下载
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/lightcat ./cmd/lightcat

# ---- 运行段 ----
FROM alpine:3.22
# ca-certificates：ontweb 采集 / 告警 webhook 出站 HTTPS 要用；
# tzdata：TZ=Asia/Shanghai 让日志是本地时间（Go 认 TZ 环境变量需要时区库）。
# 官方 CDN 慢/不通时换国内镜像源：--build-arg APK_MIRROR=mirrors.aliyun.com
ARG APK_MIRROR=
RUN if [ -n "$APK_MIRROR" ]; then \
        sed -i "s|dl-cdn.alpinelinux.org|$APK_MIRROR|g" /etc/apk/repositories; \
    fi \
    && apk add --no-cache ca-certificates tzdata \
    && addgroup -g 10001 lightcat \
    && adduser -D -H -u 10001 -G lightcat lightcat \
    && mkdir -p /data/files \
    && chown -R 10001:10001 /data
COPY --from=build /out/lightcat /usr/local/bin/lightcat

LABEL org.opencontainers.image.title="LightCat" \
      org.opencontainers.image.description="LightCat 光猫管家 —— 家用光猫/FTTR 的 TR-069 ACS 与监控面板" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later" \
      org.opencontainers.image.source="https://github.com/zy84338719/lightcat"

USER lightcat
WORKDIR /data
# 镜像内置的默认配置：全部可用同名环境变量覆盖（完整清单见 deploy/DOCKER.md）
ENV ACS_DB=/data/lightcat.db \
    ACS_FILES_DIR=/data/files
# 7547 = CWMP（设备侧；不设 ACS_WEB_LISTEN 时面板也在这个端口）；8080 = 面板（双端口部署时）
EXPOSE 7547 8080

# 健康检查打专用端点 /healthz（会真 ping 一次 SQLite），不用面板首页：
# 首页未登录时靠「303 跳登录页也算健康」这种巧合来判断，语义不对；
# 而打 CWMP 路径在双端口部署下是 GET / → 405，容器会被一直判 unhealthy。
# /healthz 在 ACS 与面板两个端口都有，所以单端口/双端口都不用改端口号。
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${LIGHTCAT_HEALTH_PORT:-7547}/healthz" || exit 1

ENTRYPOINT ["/usr/local/bin/lightcat"]
