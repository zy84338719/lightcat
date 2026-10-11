# LightCat 光猫管家

**简体中文** | [English](README-en.md)

> LightCat 是**独立维护的开源项目**，源自 [hakureiyuyuko/go-acs](https://github.com/hakureiyuyuko/go-acs)（AGPL-3.0）：
> 继承其完整的 TR-069/CWMP 实现，在其上持续扩展光猫监控与运维能力（ontweb Web 适配通道、
> 光功率趋势与阈值告警、预设、固件远程升级、Docker/Kubernetes 部署等）。
> 本仓库继续以 AGPL-3.0 开源，并保留对上游的署名。

单二进制、零外部中间件的 TR-069/CWMP ACS（Go + SQLite），用来管一批光猫 / FTTR 主机：
**纳管 → 看信息 → 改配置 → 诊断 → 子设备与终端管理**。

![CI](https://github.com/zy84338719/lightcat/actions/workflows/ci.yml/badge.svg)
![Release](https://github.com/zy84338719/lightcat/actions/workflows/release.yml/badge.svg)
![Go](docs/images/badges/go.svg)
![License](docs/images/badges/license.svg)

## 特点

- **单文件部署**：一个静态二进制 + 一个 SQLite 文件，没有 Redis / MySQL / 消息队列
- **只用标准协议**：Inform、GetParameterValues / Names、SetParameterValues、GetRPCMethods、Reboot、
  IPPingDiagnostics、Connection Request（含 HTTP Digest）；不依赖任何厂商私有服务
- **已真机验证机型：华为 V271-20**（联通版，固件 V5R023C10S326）—— **不需要运营商超管**，
  光猫的普通账号走 Web 适配通道（ontweb）即可完整纳管：设备信息、无线与 FTTR 子设备、
  在线终端、发光功率 / 温度 / 电压 / 偏流时序（详见下文「Web 适配通道」一节）；
  将来拿到超管、把 ACS URL 指过来，同一台设备自动合并为 TR-069 管理
- **先探测再显示**：能力探测决定界面出现哪些区块（WAN、FTTR 子设备…），探测不到就整块不显示，不摆空壳
- **如实呈现设备行为**：能改不能读的参数、异步生效的无线参数、设备没上报的字段（显示 `N/A`），不编数字
- **面板中英双语**：右上角一键切换（`?lang=` + cookie + 浏览器语言协商），漏翻有测试卡着
- **离线设备不乱给操作**：详情页里需要设备配合的按钮（重新获取、唤醒、重启、诊断）在离线时变灰，
  删除设备这类本地操作照旧可用
- **在线状态按设备自己的周期判**：超过`上报周期×2`没上报，先主动发 Connection Request 探三次，
  探不通才判离线（设备断电时 TR-069 不会通知 ACS，只能这样确认）
- **可运维**：任务队列持久化、一键唤醒设备、重启 / 删除设备、面板独立登录页（会话 cookie、可退出）、
  任务与上报记录的保留上限
- **改完监听端口不用回命令行**：设置页里改完地址，点「立即重启服务」就换进程生效
  （没变的端口把 socket 交给新进程，设备上报不断；新端口起不来当场回滚）
- **厂商私有参数有映射表**：各家光猫的光功率/温度等参数名、位置、单位都不一样（还有按光模块
  寄存器编码的原始值），这些映射存在库里（`param_aliases`）—— 支持一台新光猫就是加几行数据
  （`lightcat alias add`），没登记的机型按叶子名启发式显示，不会空着也不编数字
- **光功率趋势图**：收光 / 发光 / 光模块温度按采集时序入库（TR-069 与 Web 适配两条通道都写），
  详情页服务端直出 SVG 曲线，零前端依赖；读不到的项宁缺毋编（Web 通道没有收光就不画收光）
- **阈值告警**：收光过低 / 光模块过温 / 光口异常 / 设备离线自动记录，面板「告警」页可查；
  配一个 webhook 地址就推 JSON（接 Telegram / 企业微信 / Home Assistant 由你中转，不内置 SDK）；
  同设备同类告警有冷却窗口，持续越限不轰炸
- **预设（Preset）**：「什么事件 / 什么条件下，自动对设备做哪串操作」全是数据 —— 面板上建，
  设备上报命中时动作在**同一次会话**里按序下发（纳管即配好，不用等下一轮周期）
- **固件远程升级**：面板上传固件 → 给设备下发 Download → 设备自己来拉（内置文件服务）→
  TransferComplete 回报成败，任务历史全程可见；支持 DelaySeconds 错峰
- **HTTPS**：配 `-tls-cert` / `-tls-key` 后 ACS 与面板一起切 TLS（运营商固件要求 https 的场景）
- **可观测与自动化**：`/metrics` 输出 Prometheus 文本格式；`/api/*` 与 `/metrics` 支持
  Bearer API token（`-api-token`），脚本与抓取器不用面板登录态
- **运维分组与报表**：设备标签（位置 / 用途 / 批次，概览页按标签筛选，API/CSV 同款过滤）；
  概览页「收光最弱」榜单（每台最近收光读数从低到高）；设备清单与光功率历史导出
  CSV（UTF-8 带 BOM，Excel 直接打开不乱码）
- **CloudWeGo Hertz 引擎**：HTTP 层跑在 Hertz（netpoll）上，`/api/*` 由 hz 按 IDL
  （`idl/lightcat_api.proto`）生成路由与 biz 三层；TR-069 协议内核保持零第三方依赖，
  经 415 项端到端与 GenieACS 互通验证
- **三种部署形态**：二进制 + systemd（一条命令安装/升级/回滚）、Docker（compose 一条命令起，
  多架构镜像、非 root、自带健康检查）、Kubernetes（StatefulSet + PVC + kustomize，
  探针/安全上下文配齐）；数据永远只有一个目录，备份 = 拷走它

## 截图

> 数据来自仓库自带的 CPE 模拟器，不含任何真实设备信息。

概览：统计卡 + 收光最弱榜 + 设备列表（在线状态、序列号、软件版本、标签、最后上报时间、已采集参数条数、
无线终端数，有设备上报光功率时会多出「收光 / 发光」两列；可按在线 / 离线 / 标签筛选，右上角可开 5 秒
自动刷新）+ 无线概况 + 导出 CSV

![概览](docs/images/overview.png)

基本信息（含光模块读数：收光 / 发光 / 光模块温度 / 电压 / 偏流）、WAN 连接、操作（唤醒 / 重启 / 删除）、备注；右上角同样有 5 秒自动刷新开关（与列表页共享）

![设备详情](docs/images/device.png)

设置：ACS 与面板各自的监听地址、面板登录账号密码（端口改动重启生效，账号密码立即生效）；
改完地址可以点「立即重启服务」，不用再去命令行

![设置](docs/images/settings.png)

## 安装（Release 包）

从 [Releases](https://github.com/hakureiyuyuko/go-acs/releases) 下对应架构的包，解压后一条命令装成 systemd 服务：

```bash
VERSION=1.3.0                                   # 换成你下载的那个版本（≤1.2.x 的包名是 acs-*）
tar xzf lightcat-$VERSION-linux-amd64.tar.gz
cd lightcat-$VERSION-linux-amd64
sudo ./install.sh                                                # 默认 CWMP 与面板都走 :7547
sudo ./install.sh --web-listen :8080 --web-user admin --web-pass '改成你自己的'
```

装完会打印面板地址与要填进光猫的 ACS URL（`http://<本机IP>:7547/acs`）。后续：

```bash
sudo ./update.sh         # 升级：校验 SHA256 → 停服务 → 备份 → 换新 → 健康检查，起不来自动回滚
sudo ./uninstall.sh      # 卸载：默认保留数据库与配置，--purge 连数据一起删
```

默认落点：主程序 `/usr/local/bin/lightcat`、数据 `/var/lib/lightcat`、配置 `/etc/default/lightcat`、
服务 `acs`。包内 `README.md` 有完整的选项与目录说明。

## 快速开始（源码）

```bash
go build -o lightcat ./cmd/lightcat        # Go 1.27+；CGO_ENABLED=0 可得到静态二进制
./lightcat -listen :9090 -db lightcat.db   # 面板 http://<IP>:9090/ ，CWMP http://<IP>:9090/acs
```

打发布包：

```bash
scripts/build-release.sh v1.2.3   # 产物在 dist/：amd64 + arm64 的 tar.gz 与 SHA256SUMS
```

设备侧的 ACS URL 填 `http://<IP>:9090/acs`；真机里也见过配成根路径 `/` 的，所以两者都收。
面板挪到独立端口（`-web-listen :8080`）时，CWMP 那侧**任何路径都受理**，不用担心运营商定制设备的路径写法。

没有设备也能玩：仓库自带模拟器（含 FTTR 子设备、能改不能读、异步诊断等开关；
收到 Download 会**真的去拉文件**并按协议另起会话回报 TransferComplete，固件升级全流程可离线验证）。

```bash
go build -o cpesim ./test/cpesim
./cpesim -acs http://127.0.0.1:9090/acs -serial DEMO0123 -fttr 3 -fttr-optical
```

## 容器部署（Docker / Kubernetes）

```bash
# Docker：一条命令起服务（面板 :8080，CWMP :7547，数据落 ./data）
LIGHTCAT_PASS='你的强密码' docker compose up -d
```

Kubernetes 用 `deploy/k8s/`（StatefulSet + PVC；**单副本是硬约束**——SQLite 单写者，
升级时先停旧再起新，不双实例抢库）：

```bash
kubectl create namespace lightcat
kubectl -n lightcat create secret generic lightcat-secret \
  --from-literal=ACS_WEB_USER=admin --from-literal=ACS_WEB_PASS='你的强密码'
kubectl apply -k deploy/k8s/ -n lightcat
```

镜像多架构（amd64 / arm64），非 root 运行；无论哪种部署，数据永远只有一个目录
（SQLite 库 + 固件文件），备份 = 拷走它。配置全部走 `ACS_*` 环境变量（与 flag 一一
对应）。详见 [deploy/DOCKER.md](deploy/DOCKER.md) 与 [deploy/k8s/K8S.md](deploy/k8s/K8S.md)。

## Web 适配通道（ontweb）—— 没有运营商超管也能纳管

TR-069 的前提是把光猫的 ACS URL 指到本 ACS，而那个设置页通常只有运营商超级管理员能进。
`ontweb` 是一条补充通道：用光猫 Web 界面的**普通账号**，直接读它的 AJAX 参数接口
（`getajax.cgi`，本质是 HTTP 上的 TR-181 查询），把采集结果按标准 TR-098 参数形状
写进与主程序**同一个库** —— 设备列表 / 详情 / 光功率 / FTTR 子设备 / 终端树，面板一行不改全部复用。

```bash
go build -o lightcat ./cmd/lightcat
ONT_USER=user ONT_PASSWORD='光猫密码' lightcat ontweb -db lightcat.db -url http://192.168.1.1
```

**真机验证（2026-10-11）**：联通版 Huawei V271-20（固件 V5R023C10S326）普通账号跑通全链路 ——
配方自动命中、399 条参数入库、64 台终端（含 3 台 FTTR 子光猫及其下 35 台终端）、光模块
发光 / 温度 / 电压 / 偏流读数齐全（**收光对普通账号不开放，如实存 NULL**，趋势图只画发光线）；
CLI 一次性采集与服务端内建轮询（`-ontweb-url` 等配置）两条路径都验过。

- **采什么**：设备信息（型号 / 序列号 / 软硬件版本 / CPU 内存占用）、光口读数
  （发光功率 / 温度 / 电压 / 偏流，寄存器原始值入库，自动登记光口映射表的换算规则）、
  在线终端（IP / MAC / 接入端口 / 流量 / 在线状态）
- **FTTR 建模**：ONU 口上的子光猫（如 K251e）识别为卡片，其下终端进各自的关联终端表；
  有线口与主猫自己 WiFi 的终端只入参数库 —— 面板的终端树回答的是「谁连在哪个 WLAN 上」，不硬造
- **身份可合并**：三元组取真序列号（OUI = SN 前 6 位）。将来拿到超管、把 TR069 的 ACS URL
  指过来时，同一台设备自动合并，以真实 Inform 数据为准
- **如实标记**：参数 `source=web`、事件 `6 WEBPOLL`，与 TR-069 采的数据一眼可分
- **光功率趋势**：每次采集记一条时序（保留 30 天），设备页画内联 SVG 曲线，零 JS
- **只读标注**：Web 通道接入的设备在面板上明确标注只读，重启 / 唤醒等需设备配合的
  按钮自动禁用，等真 TR-069 接入（同序列号合并）后自动恢复

限制也说清楚：这条通道**只读**（改配置 / 重启 / 诊断仍要走真 TR-069）；面板的收光一列
显示 `-`（普通账号页面不暴露上行收光）；终端主机名取决于光猫页面自己解析到多少。
登录握手与页面结构按华为 V271-20（V5R023 固件）实测；页面路径、握手、解析正则都收在
`internal/ontweb/profile.go` 的**机型配方**里 —— 与 `param_aliases` 一样，支持一台新机型是
加一份配方（数据），不是改架构。解析不出来的页面会如实报「配方不符 + 检测到的型号/固件」，不静默。

## 配置

命令行参数与环境变量一一对应（环境变量名 = `ACS_` + 参数名大写、连字符换下划线）。

| 参数 | 环境变量 | 默认 | 说明 |
| --- | --- | --- | --- |
| `-listen` | `ACS_LISTEN` | `:7547` | CWMP 监听地址 |
| `-web-listen` | `ACS_WEB_LISTEN` | 空 | 面板监听地址；留空 = 与 CWMP 同端口 |
| `-path` | `ACS_PATH` | `/acs` | CWMP 端点路径（同端口时生效） |
| `-db` | `ACS_DB` | `lightcat.db` | SQLite 文件路径 |
| `-user` / `-password` | `ACS_USER` / `ACS_PASSWORD` | 空 | 设备侧 HTTP 认证（CPE 基本认证）|
| `-web-user` / `-web-pass` | `ACS_WEB_USER` / `ACS_WEB_PASS` | 空 | 面板登录账号密码（首次启动种入，之后以设置页为准）|
| `-max-params-per-request` | `ACS_MAX_PARAMS_PER_REQUEST` | `200` | 单次 GetParameterValues 带多少个参数名 |
| `-task-history-limit` | `ACS_TASK_HISTORY_LIMIT` | `500` | 每台设备保留多少条任务记录（`0` = 不限）|
| `-inform-history-limit` | `ACS_INFORM_HISTORY_LIMIT` | `500` | 每台设备保留多少条上报记录（`0` = 不限）|
| `-offline-after` | `ACS_OFFLINE_AFTER` | `10m` | 多久没上报算离线（设备**没**上报周期信息时的兜底）|
| `-offline-probe` | `ACS_OFFLINE_PROBE` | `true` | 判离线前先主动探测；`false` = 退回纯超时 |
| `-offline-probe-factor` | `ACS_OFFLINE_PROBE_FACTOR` | `2` | 超过 `上报周期 × 这个倍数` 没上报就开始探测 |
| `-offline-probe-attempts` | `ACS_OFFLINE_PROBE_ATTEMPTS` | `3` | 最多探测几次，都没回音才判离线 |
| `-offline-probe-interval` | `ACS_OFFLINE_PROBE_INTERVAL` | `15s` | 两次探测之间的间隔 |
| `-offline-probe-grace` | `ACS_OFFLINE_PROBE_GRACE` | `30s` | 最后一次探测后再等多久才判离线 |
| `-offline-probe-max` | `ACS_OFFLINE_PROBE_MAX` | `0` | `周期×倍数` 的上限（`0` = 不设；设备上报的周期很大时用得上）|
| `-offline-check-interval` | `ACS_OFFLINE_CHECK_INTERVAL` | `30s` | 后台多久巡检一次在线状态 |
| `-auto-fetch-wifi` | `ACS_AUTO_FETCH_WIFI` | `true` | 纳管时自动采集无线概况与主机列表 |
| `-auto-refresh-wifi` | `ACS_AUTO_REFRESH_WIFI` | `10m` | 无线 / 终端概况的自动刷新间隔（面板上的「采集」时间跟着动；`0` = 只在首次纳管时采一次）|
| `-probe-capabilities` | `ACS_PROBE_CAPABILITIES` | `true` | 纳管时做一次能力探测（决定界面区块）|
| `-log-level` | `ACS_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `-log-soap` | `ACS_LOG_SOAP` | `false` | 打印原始 SOAP 报文（排障用）|
| `-acs-url` | `ACS_URL` | 空 | ACS 对**设备**的可达地址；下发固件时文件 URL 用它拼（不配则按面板访问地址兜底并提示核实）|
| `-optical-history-days` | `ACS_OPTICAL_HISTORY_DAYS` | `30` | 光功率历史（趋势图）保留天数（`0` = 不限）|
| `-alert-webhook` | `ACS_ALERT_WEBHOOK` | 空 | 告警推送地址（POST JSON；空 = 只记录不推送）|
| `-alert-rx-low` | `ACS_ALERT_RX_LOW` | `-27` | 收光低于此值告警，dBm（`0` = 关闭该规则）|
| `-alert-temp-high` | `ACS_ALERT_TEMP_HIGH` | `70` | 光模块温度高于此值告警，℃（`0` = 关闭）|
| `-alert-offline` | `ACS_ALERT_OFFLINE` | `true` | 设备判定离线时告警 |
| `-alert-cooldown` | `ACS_ALERT_COOLDOWN` | `6h` | 同设备同类告警的冷却窗口 |
| `-files-dir` | `ACS_FILES_DIR` | `files` | 固件/配置文件库目录 |
| `-tls-cert` / `-tls-key` | `ACS_TLS_CERT` / `ACS_TLS_KEY` | 空 | HTTPS 证书/私钥（PEM，成对配置后 ACS 与面板一起切 TLS）|
| `-api-token` | `ACS_API_TOKEN` | 空 | `/api` 与 `/metrics` 的 Bearer token（**配了就必须校验**，见「部署与运维」）|

### 面板登录

面板是**独立登录页 + 会话 cookie**（不是 HTTP Basic）：未登录访问任何页面都会跳到 `/login`，
带登录态后 7 天内有效、用着会自动续期，顶栏有「退出」按钮。登录态是 HMAC 签名的 cookie
（HttpOnly + SameSite=Lax，密钥存库、重启不掉线）；**改账号密码会把所有旧登录态立刻作废**。
接口（`/api/*`）未登录时回 401 JSON，静态资源不需要登录。

忘了密码：`ACS_WEB_AUTH=off` 起一次即可进去改，或清掉库里 `web_user`/`web_pass`。

### 在线状态怎么判

设备断电时 TR-069 **不会**给 ACS 发任何通知（只有设备主动连上来时 ACS 才知道它活着），
所以在线状态是按「多久没动静」推出来的，且跟设备自己上报的周期挂钩：

```
最后一次上报 + 上报周期×2            → 开始主动探测：发 Connection Request
  探测最多 3 次（间隔 15s）          → 有一次连上：判定设备还活着，保持在线
  3 次都连不上 + 再等 30s            → 标记离线
设备没上报周期信息                   → 退回 -offline-after（默认 10 分钟）纯超时
```

界面上有三种状态：**在线** / **探测中**（超期了、正在探）/ **离线**。
设备再次上报（含被唤醒后回连）就自动恢复在线并清掉探测进度。

### 阈值告警

四类告警自动记录：收光低于 `-alert-rx-low`（默认 -27 dBm，GPON 灵敏度下限附近）、
光模块温度高于 `-alert-temp-high`（默认 70 ℃）、光口状态不是 Up（Web 适配通道上报的状态）、
设备判定离线。记录进 `alerts` 表，顶栏「告警」页可查，设备详情页也有「最近告警」块。

- **冷却窗口**（`-alert-cooldown`，默认 6h）：同设备同类告警窗口内只记一条，持续越限不轰炸。
- **webhook 推送**（`-alert-webhook`）：POST 一份 JSON（kind / message / value / threshold /
  设备序列号 / 时间），10 秒超时、失败只记日志不重试 —— 告警主体永远在库里，推送是尽力而为的旁路。
  接 Telegram / 企业微信 / Home Assistant / n8n 由你的中转服务完成，本项目不内置第三方 SDK。
- 读数缺失（NULL）的项不评估：没读到不告警，与面板「宁缺毋编」同一口径。

### 预设（Preset）

面板「预设」页管理，**不改代码不发版**：

- 触发：Inform 事件码（0 BOOTSTRAP / 1 BOOT / 2 PERIODIC…）或「首次纳管」，可再叠加参数条件
  （按参数名后缀找值，支持 eq / ne / contains / exists）；
- 动作：每行一个，`SET 参数名=值|类型` 写参数、`GET 参数名` 采参数；相邻同类行自动合并成一条 RPC；
- 命中后动作在**同一次会话**里按序下发（纳管即配好，不用等下一轮周期上报）；
- 默认「每台设备只触发一次」；编辑预设会清掉触发记录（改过的预设重新生效一次）。

### 固件远程升级

面板「固件」页：上传固件/配置文件（落盘 `-files-dir`，磁盘名是服务端生成的随机十六进制，
自动算 SHA256）→ 选设备「下发」→ ACS 给设备入队 Download → **设备自己来拉**
`/files/<随机名>`（内置文件服务，双端口部署时挂在 CWMP 侧）→ 设备传完发
TransferComplete(CommandKey) 回报成败，任务历史可见；接受后 2 小时没回报由巡检判失败。
`DelaySeconds` 支持批量错峰。文件 URL 的基地址优先用 `-acs-url`（设备视角可达的地址）。

### HTTPS 与自动化接口

- `-tls-cert` + `-tls-key`（成对）后 ACS 与面板两个监听一起切 HTTPS；证书读不了启动即报错。
- `/metrics`：Prometheus 文本格式（设备数/在线数/任务/告警等 gauge，零第三方依赖）；
  `/metrics` 与 `/api/*` 认两种凭据：面板登录 cookie 或
  `Authorization: Bearer <token>`（`-api-token` 配置，常量时间比较），未认证回 401 JSON。
  **配了 `-api-token` 就一定校验**，与是否启用面板账号无关（v1.4.0 起；
  之前只配 token 而不开面板账号时，`/api/*` 实际全网可读，token 形同虚设）。
  不配 token 时则跟随面板自身鉴权：开了账号要登录态，没开就是公开部署。
- **设置页一键对接**：面板「设置」页底部可下载两份现成的配置 ——
  **Prometheus 抓取配置**（按当前实例生成：可达地址已填好、配了 token 就带上
  Bearer 段，并进 `prometheus.yml` 即可）与 **Grafana 看板 JSON**（导入后数据源选
  自己的 Prometheus）。两者对 `/metrics` 实际暴露的十个指标是双向对齐的，
  看板不会引用不存在的指标。改过 token 记得重新下载。
- 抓取端点默认是 `127.0.0.1:<面板端口>`；Prometheus 与面板不在同一台机器时，
  把监听地址改成可被 Prometheus 访问的网卡地址。

### 设备清单页（/inventory）

概览页是运营看板（在线状态 / 收光榜 / 告警，按筛选裁剪）；**清单页**是资产盘点视角：
不筛选、不裁列，在线与离线、历史上纳管过的都在，连低频字段（来源 IP、WAN 口 IP、
首次纳管时间、上报周期）也一并列出，直到手动删除为止。两个页面刻意分工 ——
日常运维看概览，年底盘点看清单。

### 使用帮助（/help）

顶栏「帮助」入口。第一次部署最该知道的事都在这儿：这是什么、首次接入的四步
（ACS URL 怎么填、为什么不能填 `127.0.0.1`）、各页面职责、常见问题、版本与升级方式。
不必先去翻仓库文档。

### 标签、收光榜与 CSV 导出

- **设备标签**：设备详情页「标签」块（逗号分隔，最多 16 个，超长自动截断）。纯面板侧的
  运维分组（位置 / 用途 / 批次），**只存本系统、不下发设备**；概览页有标签 chips 条，
  点一下按标签筛选（URL 可分享），设备行里的标签徽章同样可点；`/api/devices?tag=`
  与 CSV 导出是同一套筛选。设备删除时标签随外键级联清掉。
- **收光最弱榜**：概览页新增一块榜单 —— 按**每台设备最近一次**收光读数从低到高取前 5
  （没有收光数据的不参与，不拿 NULL 冒充垫底）。统计卡片维持「已纳管设备 / 在线」两张
  的既定口径，告警入口一直在顶栏。
- **CSV 导出**：概览页搜索栏旁「导出 CSV」（设备清单：序列号 / 厂商型号 / 在线 / 标签 /
  备注 / 最近光功率读数，跟随当前标签筛选）；设备详情页趋势图下「导出历史 CSV」
  （光功率历史全量，`?limit=` 可调）。UTF-8 带 BOM，Excel 双击打开不乱码；
  鉴权与 `/api/*` 一致（面板登录态或 Bearer token）。

## 验收

```bash
go test ./...                   # 单元测试：协议解析 / 存储 / Web
bash scripts/verify-s1.sh       # 端到端 415 项：模拟器打真实 HTTP + SOAP，逐条断言
                              # （含固件下发/预设/告警全流程；3 项浏览器用例无 headless Chrome 时合规跳过）
bash scripts/verify-interop.sh  # 与 GenieACS 官方 JS 模拟器互通 8 项
```

## 压测

```bash
scripts/loadtest.sh                     # 阶梯 10/50/100/200/400，每个档位都用全新的库
scripts/loadtest.sh -n 200 -e 4484      # 只跑一档（-e = 每台 X_HW_APDevice 子树参数个数）
```

设备模板默认照真机华为 V271-20（TR-098、3 台 FTTR 子设备、`X_HW_APDevice` 子树 4484 个参数，
每台约 4700 个参数），全部设备同时发 `1 BOOT`，模拟**大规模断电恢复**。

本机（4 核）实测：吞吐约 **4 台设备/秒**（1.6–1.8 万参数行/秒）—— 100 台同时上电 26 秒完成，
200 台约 1 分钟、400 台约 2 分钟；数据都是全量入库，只是超过 CPE 30 秒超时的设备会在下一次
周期上报收尾。瓶颈是 SQLite 单连接串行写（**别调大连接池**，实测会丢写入）。
方法与完整数据见 `docs/notes/loadtest.md`。

## 目录

```
cmd/lightcat/            程序入口（监听、优雅退出、双端口路由）
idl/                    API 契约（hz 注解的 proto）：/api/* 的路由与请求形态以它为准
biz/                    hz 生成的三层（router 路由 / handler 薄壳）；handler 委托给 internal/web 的服务层
internal/cwmp/      协议核心：SOAP 编解码、会话、任务、诊断、Connection Request
internal/store/     SQLite：设备 / 参数 / 任务 / 上报记录（迁移走 user_version）
internal/web/       Web UI 与 JSON API（模板 + 少量原生 JS，无前端框架）
test/cpesim/        自研 CPE 模拟器（大量开关，验收靠它）
deploy/             发布包里的安装 / 升级 / 卸载脚本与 systemd 单元模板
scripts/            打包、验收、压测、参考实现拉取、开发用起停脚本
docs/               需求文档、发布说明与实现笔记
```

## 更多

- `docs/requirements.md` —— 需求与实现进度
- `docs/notes/deploy.md` —— 安装包与部署脚本的实现笔记（systemd 加固、升级回滚、怎么验的）
- `docs/notes/i18n.md` —— 面板多语言方案（为什么用中文字面量当 key、怎么加一种语言）
- `docs/notes/loadtest.md` —— **压测记录**：集体上电的并发上限、瓶颈（SQLite 单连接）与实测数据
- `docs/notes/implementation-notes.md` —— **真机踩坑与实测记录**：协议边界（单次 GetParameterValues 上限、
  写回类型大小写、诊断要最后置 `Requested`）、设备怪癖（能改不能读、异步生效、身份键被元数据改写）、
  运维坑（挂载掉了不能乱删、任务别卡在 running）……修法与验证都在里面

仓库里的真机样本与文档**均已脱敏**（序列号、MAC、SSID、内网地址、终端名换成示例值）；
运行时数据库 `data/` 与日志不进仓库。

## 许可证

[AGPL-3.0](LICENSE)。可以自由使用、修改、分发（含商用）；但把**修改后**的版本作为网络服务提供给别人用时，
需要把对应源码以同样许可开放。

## 致谢

本项目由 **Deepseek V4.1 Flash** 辅助开发。

<img src="docs/images/thanks.png" alt="小肥鱼太棒了！" width="300">
