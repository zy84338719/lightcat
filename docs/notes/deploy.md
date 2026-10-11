# 安装包与部署脚本（实现笔记）

面向维护者：为什么发布包长这样、脚本里哪些地方是刻意的。

## 打包形态

`scripts/build-release.sh vX.Y.Z` 产出 `dist/` 下每个架构**两个包名**加一份校验和：

```
lightcat-X.Y.Z-linux-amd64.tar.gz     ← 正名（v1.3.0 起）
acs-X.Y.Z-linux-amd64.tar.gz          ← 兼容名副本（内容相同）
lightcat-X.Y.Z-linux-arm64.tar.gz
acs-X.Y.Z-linux-arm64.tar.gz
SHA256SUMS                            ← 四个包都列
```

为什么要兼容名：≤v1.2.x 的存量安装里，`update.sh` 按 `acs-<版本>-linux-<架构>.tar.gz`
拼下载 URL、按 `-name acs` 在解包目录里找二进制。只发新名会把这些老安装的
`sudo ./update.sh` 当场打断，所以两个名字都发（发布时四个 tar.gz 都传上 release）。

包里就是「一个二进制（两个名字）+ 三个脚本 + systemd 单元模板」：

```
lightcat-X.Y.Z-linux-amd64/
├── lightcat            静态二进制（CGO_ENABLED=0，modernc.org/sqlite 纯 Go）
├── acs                 同一 inode 的兼容硬链接（老 update.sh 按这个名字找）
├── install.sh          安装 / 原地升级
├── update.sh           从 GitHub 升级（校验、备份、回滚）
├── uninstall.sh        卸载（默认留数据）
├── lightcat.service    systemd 单元模板，install.sh 会填占位符（装成 acs.service）
├── README.md           部署说明（= deploy/README.md）
├── LICENSE             AGPL-3.0
└── VERSION
```

编译用 `-trimpath -ldflags "-s -w"`：路径不进二进制，体积也小（amd64 包约 6.1 MB）。

## 落点与约定

| 项目 | 路径 |
|---|---|
| 主程序 | `/usr/local/bin/lightcat` |
| 数据（数据库、升级备份、pkg）| `/var/lib/lightcat`（0750，属主 `acs`）|
| 配置 | `/etc/default/lightcat`（0600，systemd EnvironmentFile）|
| 单元 | `/etc/systemd/system/acs.service`（**服务名保持 `acs`**：老安装 systemctl 习惯不断档）|
| 运行用户 | `acs`（系统用户、nologin）|

上表是 v1.3.0 起的全新安装落点。≤v1.2.x 的老落点是 `/usr/local/bin/acs`、
`/var/lib/acs`、`/etc/default/acs`（库文件名 `acs.db`）——三个脚本都会**自动接管老落点**：
`install.sh` 发现新位置没数据而 `/var/lib/acs/acs.db` 存在时沿用老目录/老配置/老库名；
`update.sh` 换二进制写的是**单元 ExecStart 实际引用的路径**（不按名字猜，新老布局都升得动），
备份与健康检查读的也是老落点；`uninstall.sh` 两种二进制名都清、`--purge` 找得到老数据。
数据不搬家、服务不更名，老机器升级零迁移。

## 刻意的几处

1. **`install.sh` 重复运行 = 原地升级**：已存在的 `/etc/default/lightcat` 默认不覆盖（要覆盖加
   `--force`），数据库当然也不动。这样「下载新包 → 解压 → 跑 install.sh」就是升级路径，
   `update.sh` 是在此之上多了「自己下载 + 校验 + 回滚」。
2. **版本靠 `VERSION` 文件，不靠 `lightcat -version`**：二进制里没有版本常量，也不想为一个展示字段
   引入构建期 ldflags 依赖。`install.sh` 把包里的 `VERSION` 落到数据目录，`update.sh` 用它判断
   「已是最新」和给备份命名。以后真加了 `-version`，这两处一起改。
3. **`update.sh` 必须先停服务再换二进制**：Linux 上覆盖一个正在运行的可执行文件会 `ETXTBSY`。
   换完起服务做健康检查，不健康就把备份装回去再起一次（备份在 `/var/lib/lightcat/backups/`，
   默认留最近 3 个，`--keep` 可调）。数据库与配置全程不动。
4. **升级来源两条路**：默认走 GitHub API（curl 拉 JSON，压成一行后用 grep 抠字段——不依赖 jq，
   小机器上不用额外装东西）；`--file` 走本地包，内网 / 离线现场用。包用 release 里的
   `SHA256SUMS` 校验，校验不过直接拒绝安装。
5. **systemd 加固**：`ProtectSystem=strict` + `ReadWritePaths=<数据目录>`、`NoNewPrivileges`、
   `PrivateTmp`。**数据库挪到别处时，必须同步把新目录加进 `ReadWritePaths`**，否则服务起不来
   （会在安装脚本的健康检查里直接暴露出来，不会静默）。
6. **小于 1024 的端口**（比如 `:80`）需要 `AmbientCapabilities=CAP_NET_BIND_SERVICE`，
   单元里留了注释行，去掉注释即可。
7. **健康检查判据**：面板端口返回 2xx/3xx/401/403 都算正常（开了面板鉴权就是 401）。
   端口从 `/etc/default/lightcat` 里的 `ACS_WEB_LISTEN`、没有取 `ACS_LISTEN`。

## 发布前试跑抓到的几个坑（都在脚本里，二进制没动）

1. **别用正则去刮 GitHub API 里的资产地址**。第一版 `update.sh` 是把 API JSON 压成一行后
   用 `grep -oE '"name": "xxx".*"browser_download_url": "..."'` 抠下载地址，结果 `[^]]*`
   是贪婪的，跨过 asset 对象一路匹配到了**最后一个**资产的 URL——于是「下载 tar.gz」实际下到
   的是 190 字节的 `SHA256SUMS`。
   **是 SHA256 校验闸把它拦下来的**（哈希对不上直接拒装），这个坑值得留着当反面教材。
   现在的做法：release 资产 URL 形状固定，直接拼 `/releases/download/<tag>/<文件名>`，
   API 只用来问「最新 tag 是哪个」。（内网镜像可用环境变量 `ACS_DL_BASE` 换前缀。）
2. **管道 + `set -e` 会咬人**。`grep -E "^ACS_X=" "$ENV_FILE" | ...` 在配置文件不存在时
   grep 退出码 2，配上 `set -o pipefail` 直接中断整个脚本（症状：升级做到一半静悄悄退出）。
   现在这类「读一读、读不到就算了」的取值函数统一 `|| true`，清理备份的 `ls` 也一样。
3. **解包加 `--no-same-owner`**。包里带的属主信息在容器 / 用户命名空间里没法还原，
   `tar -xzf` 会报错退出；反正下一步是 `install -m 0755`，属主不重要。
4. **回滚要连版本号一起回滚**。否则数据目录里记着新版本、跑的却是旧二进制，
   下次 `update.sh` 的「已是最新」判断就错了。

## 这套脚本是怎么验的

- 打包：`scripts/build-release.sh v1.0.0`，检查两个包的内容与权限（755 保持得住，Linux 上 tar
  本来就带权限，不像 Windows 那样丢可执行位）。
- `install.sh`：① `--no-service` + 自定义目录（普通用户即可）→ 二进制、配置、VERSION 落位正确；
  ② 完整 systemd 分支用 `unshare -rm`（假 root）+ 一个假的 `systemctl` 驱动跑通：生成单元 →
  `daemon-reload`/`enable`/`restart` → 面板返回 401（鉴权生效）→ 健康检查通过。
- `update.sh`：`--file` 本地包升级 → 备份、`VERSION`、`/var/lib/lightcat/pkg` 刷新都对；
  坏包 / 包不存在 / release 还不存在三种失败都给明确错误与退出码 1；健康检查与回滚分支同样在假
  systemctl 下覆盖过。`--dry-run` 全程只打印。
- `uninstall.sh`：默认保留数据与配置；`--purge` 连数据、配置、服务用户一起删。
- 升级的「起不来自动回滚」分支：用「假 systemctl 不真的起服务」制造健康检查失败，
  确认旧二进制与 `VERSION` 都回退了、退出码非 0。
- **从真 GitHub release 升真版本**：把数据目录的 `VERSION` 改成 0.9.0，
  用发布包里的 `update.sh` 走完整网络流程（查最新版 → 下载 → SHA256 → 换二进制 → 健康检查）。

**还没在真机上跑过 `sudo ./install.sh`**（开发环境没有免密 sudo）：真 root + 真 systemd 的
那一遍需要在有 sudo 的机器或干净容器里过一遍，重点看 `useradd`、单元加载、`ProtectSystem` 下能否
正常写库。

## 发新版流程（备忘）

**推 tag 后 `release.yml` 会自动做掉第 3~4 步**（多架构镜像推 ghcr.io +
`build-release.sh` 产物传 GitHub Release，说明取 `docs/releases/<tag>.md`）。
手动发布或 CI 失败时才走下面的完整流程：

1. 改 `deploy/` 里的脚本 / 代码后，跑一遍完整验收（`go test ./...`、`scripts/verify-s1.sh`、
   `scripts/verify-interop.sh`）。
2. 提交并推 `main`；把 README 与 `deploy/README.md` 里 `VERSION=1.0.x` 的示例改成新版本号。
3. `scripts/build-release.sh vX.Y.Z` 产包（每次只把本次产出的包写进 `SHA256SUMS`，
   同名旧包会先删掉，免得混进去），`git tag -a vX.Y.Z` 并推 tag（**先提交再打包**，
   二进制里会带构建时的 VCS 信息）。
4. 上传五个资产（`lightcat-*` 与 `acs-*` 兼容名共四个 tar.gz + `SHA256SUMS`）到该 tag 的
   release —— 兼容名别漏，≤v1.2.x 存量安装的 `update.sh` 靠它升级；release 说明放
   `docs/releases/vX.Y.Z.md`。
5. 收尾验证：拿新包在一个临时目录里 `install.sh --no-service`，再把数据目录的 `VERSION`
   改成上一版，用包里的 `update.sh` 从真 GitHub 升一次，确认能换上新二进制且版本号对得上。

## 容器链（Dockerfile / compose / K8s）：哪些地方是刻意的

1. **构建段 `--platform=$BUILDPLATFORM` + Go 交叉编译**：多架构构建不靠 qemu 模拟
   （慢且容易在边界情况翻车），Go 本来就交叉编译得动；`TARGETOS/TARGETARCH` 是 buildx
   注入的标准 ARG。运行段 alpine 由 buildx 按目标架构取对应基座。
2. **运行段选 alpine 而不是 scratch/distroless**：留一个 shell 给 `docker exec` 排障，
   顺手拿到 ca-certificates（ontweb / webhook 出站 HTTPS）与 tzdata（`TZ` 生效，Go 认
   命名时区需要时区库）。代价 ~8MB，对这个量级的镜像无所谓。
3. **非 root（uid/gid 10001）+ 只写 /data**：K8s 清单配了只读根文件系统与 drop ALL。
   `local-path`/`hostPath` 不支持 fsGroup，所以 StatefulSet 带了个 root 的
   `fix-data-perms` initContainer 兜底（restricted PSA 的命名空间要删掉它，改用支持
   fsGroup 的存储）。
4. **健康检查/探针永远打面板端口**：CWMP 处理器对非 POST 一律 405（`handler.go`），
   GET / 探 CWMP 专用端口必误报。单端口部署 7547 即面板；双端口用
   `LIGHTCAT_HEALTH_PORT=8080`（compose 已内置），K8s 探针直接指 panel 端口。
   面板未登录时 / 是 303 —— K8s httpGet 把 2xx/3xx 都算成功，正好。
5. **StatefulSet + replicas 恒 1**：SQLite 单写者，没有水平扩展余地；单副本滚动更新
   天然先停旧再起新，不会双实例抢 RWO 卷。这条约束写在 K8S.md 开头，别让人手滑调大。
6. **compose 的 `ACS_WEB_PASS` 用 `${LIGHTCAT_PASS:?}`**：不给密码直接起不来 ——
   面板鉴权是「首启种一次」的模型，空密码等于把无鉴权面板开进局域网，宁可第一次就拦住。
7. **manifests 校验放 CI（kubeconform -strict）**：本地 `kubectl --dry-run=client` 也要连
   集群拉 OpenAPI，离线环境验不了；CI 常驻严格校验才能拦住字段拼错。
8. **国内网络**：Docker Hub 直连不通时配 registry-mirrors（daocloud / 1ms.run 实测可用）。
   一个实测的坑：系统代理若在**仅 VPN 路由可达**的网段（如 192.0.2.x），Docker Desktop
   的 VM 与 backend 进程都可能路由不到它（`no route to host` / 超时），此时镜像源是唯一
   稳的路；细节与解法记在 deploy/DOCKER.md「国内网络」。
