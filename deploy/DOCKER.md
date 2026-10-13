# Docker 部署

单镜像、单容器、一个数据目录。镜像里是一个静态二进制（非 root 运行），
数据只有 `/data`（SQLite 库 + 上传的固件），备份 = 拷走这个目录。

## 快速开始

```bash
# 方式一：docker compose（推荐，配置都写好了）
LIGHTCAT_PASS='你的强密码' docker compose up -d

# 方式二：裸 docker run（单端口模式：面板与 CWMP 共用 7547）
docker run -d --name lightcat \
  -p 7547:7547 \
  -v ./data:/data \
  -e ACS_WEB_USER=admin -e ACS_WEB_PASS='你的强密码' \
  ghcr.io/zy84338719/lightcat:1.4.1
```

> **镜像标签没有 `v` 前缀**：发布流水线推的是 `1.4.1` / `1.4` / `latest`，
> 不是 `v1.4.1`。写 `v1.4.1` 会拉到 404 —— 冒号后的部分才是 tag，`ghcr.io/.../lightcat`
> 后面那个 `v` 是仓库名的一部分。

启动后：

| 入口 | 地址 |
|---|---|
| Web 面板 | compose：`http://<本机IP>:8080/`；单端口：`http://<本机IP>:7547/` |
| CWMP（填进光猫的 ACS URL） | `http://<本机IP>:7547/acs` |

compose 默认**双端口**（7547 只跑设备协议、8080 只开面板），职责分开、暴露面更小；
想单端口就把 `ACS_WEB_LISTEN` 留空并删掉 8080 映射。

## 配置

全部配置走 `ACS_*` 环境变量（与命令行 flag 一一对应，**flag 优先**：环境变量先给默认值，
命令行再覆盖），常用的：

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `ACS_LISTEN` | `:7547` | CWMP 监听（设备侧） |
| `ACS_WEB_LISTEN` | 空=同端口 | 面板独立监听 |
| `ACS_DB` | `/data/lightcat.db`（镜像内置） | 数据库路径 |
| `ACS_FILES_DIR` | `/data/files`（镜像内置） | 固件文件目录 |
| `ACS_WEB_USER` / `ACS_WEB_PASS` | 空 | 面板账号，**只在首次启动（空库）种一次**；之后改密码走面板「设置」页 |
| `ACS_WEB_AUTH` | 空 | 设成 `off` 关掉面板鉴权（救急用，见下） |
| `ACS_API_TOKEN` | 空 | `/api` 与 `/metrics` 的 Bearer token，**配了就必须校验**（见「安全备注」） |
| `ACS_URL` | 空 | ACS 对外 URL；**固件远程升级必须配**（光猫按它来拉文件），要填光猫可达的地址 |
| `ACS_TLS_CERT` / `ACS_TLS_KEY` | 空 | 成对配置后两个监听都切 HTTPS（证书挂进容器，见下） |
| `TZ` | `Asia/Shanghai`（compose） | 日志时区（镜像已带 tzdata） |

`docker-compose.yml` 在这些之上还包了一层更短的变量名（给 `.env` 或命令行用）：

| compose 变量 | 映射到 | 默认 | 说明 |
|---|---|---|---|
| `LIGHTCAT_PASS` | `ACS_WEB_PASS` | **必填** | 没设会直接报错退出，防止误开无鉴权面板 |
| `LIGHTCAT_USER` | `ACS_WEB_USER` | `admin` | |
| `LIGHTCAT_API_TOKEN` | `ACS_API_TOKEN` | 空 | |
| `LIGHTCAT_ACS_URL` | `ACS_URL` | 空 | 固件升级要用，建议配上 |
| `LIGHTCAT_HEALTH_PORT` | 仅镜像内 HEALTHCHECK | `7547` | **双端口必须设成面板端口**，compose 已代劳 |
| `TZ` | `TZ` | `Asia/Shanghai` | |

`LIGHTCAT_PASS` 用的 `:?` 校验对**所有** compose 子命令生效，所以 `down`/`logs` 也要带上：

```bash
LIGHTCAT_PASS=x docker compose down
```

完整清单：`docker run --rm ghcr.io/zy84338719/lightcat:1.4.1 -help`，或 README「配置」一节。

面板密码忘记时的救急：加 `-e ACS_WEB_AUTH=off` 重建容器（只关鉴权，不动数据），
进面板改完密码再去掉。

## 数据与备份

```bash
docker compose stop                       # 或 docker stop lightcat
cp -r ./data ./data-backup-$(date +%F)    # 库 + 固件一起备走
docker compose start
```

恢复 = 把目录拷回去再启动。数据库迁移在启动时自动执行，升级不丢数据。

## 升级

```bash
# 用仓库里的 compose（默认就是本地构建）：
git pull && docker compose build && docker compose up -d

# 想用预构建镜像：把 docker-compose.yml 里的 build: . 换成
#   image: ghcr.io/zy84338719/lightcat:1.4.1
# 之后就是 docker compose pull && docker compose up -d
```

注意：**容器里不要用 `deploy/update.sh`**——那是二进制/systemd 部署的自升级脚本，
容器的升级语义就是「换镜像、重建容器」，数据在卷里不受影响。

## 健康检查

镜像自带 HEALTHCHECK（打面板端口的 `/`；未登录时 303 跳登录页也算健康）。
**双端口部署必须把 `LIGHTCAT_HEALTH_PORT` 设成面板端口**（compose 已代劳）：
CWMP 专用端口对 GET 一律 405（协议只收 POST），探活会误报。

## TLS

证书挂进容器再指环境变量（不建议把证书打进镜像）：

```yaml
    volumes:
      - ./data:/data
      - ./certs:/certs:ro
    environment:
      ACS_TLS_CERT: /certs/fullchain.pem
      ACS_TLS_KEY: /certs/privkey.pem
```

或者容器只跑 HTTP，TLS 交给前面的反代（Nginx/Caddy/Traefik）终结——
光猫侧 ACS URL 填反代的 https 地址即可（CWMP 就是普通 HTTP POST，反代无压力）。

## 国内网络（构建拉不动时照这个来）

镜像构建有两类外联，分别处理：

**1. 基础镜像（golang / alpine，走 Docker Hub）——配 registry mirror：**

Docker Desktop → Settings → Docker Engine：

```json
{ "registry-mirrors": ["https://docker.m.daocloud.io", "https://docker.1ms.run"] }
```

**2. 构建步骤内的下载（go 模块 / apk 包）——Dockerfile 已内置国内可达的默认值：**

- Go 模块默认走 `goproxy.cn`（`proxy.golang.org` 兜底），海外可用
  `--build-arg GOPROXY=https://proxy.golang.org,direct` 覆盖；
- Alpine 包源默认官方 CDN，慢/不通时 `--build-arg APK_MIRROR=mirrors.aliyun.com`。

**一个实测的深坑（macOS + 局域网代理）**：系统代理若在仅 VPN/特定网卡可达的网段
（例 `192.0.2.10:7890`），Docker 的两条路都可能断——VM 里没有那条路由（pull/构建报
`no route to host` 或超时）；更隐蔽的是 macOS 的「本地网络」隐私权限会拦
`com.docker.backend` 进程直连局域网 IP（shell 里 `curl` 通、Docker 自己不通）。
解法：Docker Desktop → Settings → Resources → Proxies 改 Manual，指向一个
**主机回环上的转发器**（回环不受「本地网络」权限限制），例如：

```bash
# 127.0.0.1:17890 → 真代理（python3 即可，socat 同理）
python3 - <<'EOF' &
import asyncio
async def pipe(r,w):
    while d:=await r.read(65536): w.write(d); await w.drain()
    w.close()
async def h(cr,cw):
    ur,uw=await asyncio.open_connection('192.0.2.10',7890)
    await asyncio.gather(pipe(cr,uw),pipe(ur,cw))
async def m():
    async with await asyncio.start_server(h,'127.0.0.1',17890): await asyncio.sleep(-1)
asyncio.run(m())
EOF
# 然后 Manual proxy 填 http://127.0.0.1:17890，
# Bypass 里加上镜像源域名（docker.m.daocloud.io,docker.1ms.run,…）让 pull 走直连
```

或者干脆给 Docker Desktop 勾上「本地网络」权限（系统设置 → 隐私与安全性 → 本地网络），
让 manual proxy 直接填局域网代理地址。

## 安全备注

- 容器以非 root（uid/gid 10001）运行，只有 `/data` 可写；
- 面板密码只种一次、PBKDF2 哈希入库，环境变量里的是明文种子，启动后可 `docker inspect`
  看到——介意就用 `ACS_WEB_AUTH=off` + 反代鉴权，或起完容器后在面板改密码并清掉环境变量；
- **配了 `ACS_API_TOKEN` 就必须校验**：访问 `/api` 与 `/metrics` 时，携带正确的
  `Authorization: Bearer <token>` 或带面板登录态，二者其一即可；否则回 401 JSON。
  token 用常量时间比较。这一条与面板账号是否启用**无关**——v1.4.0 之前这里有个洞：
  只配了 token 而没启用面板账号时，`/api/devices`（含设备序列号、来源 IP、全部参数）
  对能访问该端口的人全网可读，而 token 形同虚设。v1.4.0 起「配了 token 就校验」，
  启动时若检测到配了 token 但未启用面板账号，会在日志里打一条 WARN 提醒你
  **面板页面本身仍然公开**——想让它也受保护，得配 `ACS_WEB_USER` / `ACS_WEB_PASS`；
- 不配 token 时：`/api` 与 `/metrics` 跟随面板自身的鉴权——面板开了账号就要登录态，
  没开就是公开（等同于部署时就没打算设防）。
