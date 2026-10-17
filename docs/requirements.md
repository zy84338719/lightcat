# LightCat 光猫管家 服务器 — 需求文档

- 文档版本：v0.1（草案，待评审）
- 编写日期：2026-09-28
- 实现语言：Go
- 状态：**需求梳理阶段**，未开始编码

> **实现进度（2026-10-12 更新）**
> P0/P1 全部落地：协议核心、任务队列、Connection Request、Web 面板、REST API（Bearer token 鉴权）。
> 本轮新增：**FR-10 预设**（面板数据驱动，同会话下发）、**FR-11 固件 Download 闭环**
> （上传 → 下发 → 设备自拉 → TransferComplete 结案）、**FR-14 `/metrics`**（Prometheus 文本格式）、
> HTTPS（`-tls-cert/-tls-key`）、光功率趋势历史（收光/发光/温度，含 Web 适配通道）、
> 阈值告警（收光过低/过温/光口异常/离线，webhook 推送）、**FR-16 面板可用性**
> （`/inventory` 资产盘点、`/help` 使用帮助、机器端点鉴权口径收紧）。
> 工程规范对齐 oekit（errors.Wrap 错误链、async.GoSafe 协程安全）已完成第一轮，
> 剩余差距立项见 **§13 工程规范对齐 oekit：差距清单**（OEK-1~11）。
> 各条目的实现口径见对应 FR 小节。
> 验收：`go test ./...` 全绿 + `scripts/verify-s1.sh` **415 / 0**（另 3 项浏览器用例在无
> headless Chrome 的机器上合规跳过）、`verify-interop.sh` 8 / 0。
> 新增三节端到端：固件下发（上传 → Download → 模拟器自拉文件 → TransferComplete 结案）、
> 预设命中（同会话下发 + 每台一次）、阈值告警。
>
> **实现进度（2026-09-28 更新）**
> S1「纳管 + 查看设备信息」已实现并通过真实验收（42/42 断言），代码在仓库根目录。
> 验收方式：`scripts/verify-s1.sh`（端到端）与 `scripts/verify-interop.sh`（与 GenieACS 官方 CPE 模拟器的互通性）。
> **已接真机**：华为 OptiXstar HN8145X6N（FTTR 光猫）纳管成功，取回 14 个参数。
> 实现笔记与实测发现见 [notes/implementation-notes.md](notes/implementation-notes.md)。
> 实现中修正了本文档三处想法：① 取设备信息改为**下发显式参数名**而不是子树路径
> （有的实现不支持子树查询，见笔记 §2）；② ACS 侧错误码用 8xxx，9xxx 是 CPE 发给我们才用的；
> ③ **ACS URL 很可能配成根路径 `/` 而不是 `/acs`**（真机就是这么配的），两个路径都要接。

---

## 1. 项目概述

### 1.1 一句话定位

用 Go 实现一个**单二进制、嵌入式存储、零外部中间件**的轻量 TR-069/CWMP ACS（Auto Configuration Server），
足以接管一批 CPE（光猫 / 路由器 / 网关），完成「注册纳管 → 参数下发 → 采集 → 远程升级 → 重启」的闭环。

### 1.2 目标

| 编号 | 目标 | 说明 |
| --- | --- | --- |
| G1 | 协议正确 | 能正确收发 CWMP SOAP，兼容 TR-069 Amendment 1~6 的命名空间与常见厂商实现 |
| G2 | 能真正管住设备 | Inform / Get-Set / Connection Request / 任务队列 / 固件下载 全部可用 |
| G3 | 轻量 | 单个 Go 二进制 + 一个 SQLite 文件即可跑起来；无 Node/Java/Python/Redis/PG 强制依赖 |
| G4 | 可观测 | 有结构化日志 + 每台设备的会话/任务/参数历史可查 |
| G5 | 可移植 | 只在标准 Linux 上依赖 Go 与文件系统；不绑定具体机器事实（见 §2.4 原则） |

### 1.3 非目标（明确不做，避免范围膨胀）

- 不做多租户 / 计费 / 运营级 SaaS；
- 不做 TR-369 (USP) / MQTT / WebSocket 南向协议；
- 不做完整 TR-098/TR-181 数据模型字典（只存与管实际出现过的节点，见 §6.2）；
- 不做 STUN/XMPP NAT 穿透（Annex G），第一期只支持「CPE 有可达 ConnectionRequestURL」的场景；
- 不做前端构建链（不引入 npm/vite），界面用服务端模板 + 原生 JS 或 htmx；
- 不做告警/工单/报表等上层业务系统。

---

## 2. 术语与协议基础

### 2.1 术语

| 术语 | 含义 |
| --- | --- |
| ACS | Auto Configuration Server，本项目的服务端 |
| CPE | Customer Premises Equipment，被管的设备（光猫/路由） |
| CWMP | CPE WAN Management Protocol，即 TR-069 的协议名 |
| RPC | 远程过程调用，SOAP 信封里的一次方法调用 |
| Inform | CPE → ACS 的上报，一切会话的起点 |
| Connection Request | ACS 主动「踢」CPE 回连的 HTTP GET |
| Data Model | 参数树，TR-098 根为 `InternetGatewayDevice.`，TR-181 根为 `Device.` |
| 会话 Session | 一次 CPE 与 ACS 的完整报文交换过程，由 CPE 发起、CPE 结束 |

### 2.2 协议交互模型（关键，必须实现正确）

CWMP 是一条**长轮询式的、CPE 单向发起的** SOAP over HTTP 通道，**每个 HTTP POST 只承载一个 RPC**：

```
  CPE                                             ACS
   |  POST  Inform                              |
   | -----------------------------------------> |
   |  200   InformResponse (MaxEnvelopes=1)     |
   | <----------------------------------------- |
   |  POST  (空信封：我还有事，你说吧)            |
   | -----------------------------------------> |
   |  200   GetParameterValues (ACS 下发的任务)  |
   | <----------------------------------------- |
   |  POST  GetParameterValuesResponse          |
   | -----------------------------------------> |
   |  200   (空 body)                           |
   | <----------------------------------------- |
   |  POST  (空信封)                            |
   | -----------------------------------------> |
   |  204   No Content (会话结束)                |
   | <----------------------------------------- |
```

必须遵守的规则：

1. **CPE 永远是发起方**；ACS 不能主动建连接，只能靠 Connection Request 让 CPE 回连（带回事件码 `6 CONNECTION REQUEST`）。
2. **同一时刻一台 CPE 只允许一个会话**；ACS 侧必须对同一设备串行化（见 FR-6 / NFR-2）。
3. 一个 SOAP Envelope 的 `Header` 中 **`cwmp:ID` 必须回填同一个值**（`mustUnderstand="1"`）。
4. ACS 响应中的 `MaxEnvelopes` 应回 `1`（表示「我一次只处理一个请求」）。
5. 命名空间需与 CPE 声明的版本一致（`urn:dslforum-org:cwmp-1-0` ~ `cwmp-1-4`），**不要写死 1-0**。
6. ACS 无任务可下发时，对该次空 POST 返回 `204 No Content`（部分实现用 200 + 空 body，需兼容）。

### 2.3 Inform 事件码

| 事件码 | 含义 | 附带内容 | 备注 |
| --- | --- | --- | --- |
| `0 BOOTSTRAP` | 首次连上本 ACS 或 ACS URL 变更 | DeviceId 全量 | 纳管入口，需下发基础配置 |
| `1 BOOT` | 每次上电启动 | — | |
| `2 PERIODIC` | 周期上报 | — | 由 `PeriodicInformInterval` 控制 |
| `3 SCHEDULED` | ScheduleInform 触发 | CommandKey | |
| `4 VALUE CHANGE` | 被通知参数发生变化 | **仅带变化的 ParameterList** | |
| `5 KICKED` | 被 ACS 断开后跟随 | — | |
| `6 CONNECTION REQUEST` | 收到 ACS 连接请求后回连 | — | Connection Request 成功的标志 |
| `7 TRANSFER COMPLETE` | 下载/上传完成 | CommandKey | 与 Download 任务关联 |
| `8 DIAGNOSTICS COMPLETE` | 诊断完成 | CommandKey | 第二期 |
| `M Reboot` | 重启之后 | — | |
| `X CT-*` | 厂商自定义 | 视厂商而定 | 只记录、不报错 |

> **需求约束 R-INF-1**：不得假设 Inform 一定携带全量 ParameterList。`0 BOOTSTRAP`/`1 BOOT` 可能只带部分节点，`4 VALUE CHANGE` 只带变化节点。ACS 必须能按需补齐。

### 2.4 实现原则（重要）

沿用本项目一贯风格，**不为某一台机器/某一型号设备写死事实**：

- 数据模型根（`InternetGatewayDevice.` / `Device.`）、认证方式、周期、参数类型，一律**运行时探测 + 配置项可覆盖**；
- 探测失败时**优雅降级**（例如未知参数类型先按 string 存并在日志标注），不得 panic、不得中断会话；
- 文档中出现的任何性能/容量数字必须标注为「实例测量值，非项目常量」；
- 厂商私有节点差异只在「适配层」处理，不污染核心会话逻辑。

---

## 3. 需求范围与优先级（MoSCoW）

| 优先级 | 含义 | 内容摘要 |
| --- | --- | --- |
| **P0 必须有** | 缺了就不算 ACS | HTTP/SOAP 端点、Inform 处理、会话状态机、Get/SetParameterValues、GetParameterNames、任务队列、SQLite 持久化、结构化日志 |
| **P1 应该有** | 生产可用 | Connection Request、Add/DeleteObject、参数属性（Get/SetParameterAttributes）、预设(Preset)、固件 Download + TransferComplete、最小 Web 界面、REST API、基础认证 |
| **P2 可以有** | 锦上添花 | Upload、ScheduleInform、FactoryReset、Reboot 策略控制、HoldRequests 长轮询、STUN、mTLS、PostgreSQL 后端、Prometheus 指标 |
| **不做** | — | USP/MQTT、多租户、报表、告警、NAT 穿透(第一期) |

---

## 4. 功能需求

### FR-1 CWMP 端点与传输层

- `POST /acs`（路径可配置）接收 SOAP；必须支持 `text/xml; charset=utf-8` 与 `Content-Type: application/soap+xml`。
- 必须容忍 CPE 常见的脏报文：多余空白、`Transfer-Encoding: chunked`、无 `Content-Length`、gzip、非标准 `charset`、未知 SOAP 命名空间前缀。
- 支持 HTTP 与 HTTPS（证书路径配置化，未配置则只监听 HTTP）。
- 单请求体默认上限（可配置，默认 4 MiB）防 DoS。
- **R-FR1-1**：解析失败时返回合法 SOAP Fault，而不是 500 裸错误，便于定位。

### FR-2 认证与安全

| 编号 | 需求 | 优先级 |
| --- | --- | --- |
| FR-2.1 | CPE → ACS 认证：HTTP Digest（首选）/ Basic（兼容）；账号可来自配置或数据库 | P1 |
| FR-2.2 | 可选 mTLS（Pre-Shared Certificate，TR-069 Annex C） | P2 |
| FR-2.3 | 密码**不得明文入库**（bcrypt/scrypt 或 AES-GCM 加密存储） | P1 |
| FR-2.4 | Connection Request 使用 `InternetGatewayDevice.ManagementServer`（或 `Device.ManagementServer`）下的 `ConnectionRequestUsername/Password`，HTTP Digest 认证 | P1 |
| FR-2.5 | 日志中屏蔽密码、Authorization 头、敏感参数值 | P1 |
| FR-2.6 | 可选按源 IP/白名单限制 `/acs` | P2 |

### FR-3 SOAP 编解码

- 统一 Envelope 构造：`Envelope` / `Header(cwmp:ID)` / `Body`。
- 命名空间随 CPE 请求版本回填（见 §2.2 第 5 条）。
- **R-FR3-1**：编解码必须是「容忍未知元素」的：遇到不认识的子元素/厂商扩展，跳过而不是报错。
- **R-FR3-2**：`ParameterValueStruct` 的 `xsi:type` 需覆盖 `string / int / unsignedInt / boolean / dateTime / base64`。
- **R-FR3-3**：数字类型在 XML 里可能带前导零、`+` 号、科学计数法，解析需容错。
- SOAP Fault 输出需符合 CWMP 的 `cwmp:Fault`（含 `FaultCode` / `FaultString` / `SetParameterValuesFault`）。

### FR-4 CPE → ACS RPC（南向接收）

| RPC | 处理要求 | 优先级 |
| --- | --- | --- |
| `Inform` | 解析 DeviceId/Event/ParameterList/MaxEnvelopes/RetryCount/CurrentTime；落库；建会话；回 `InformResponse` | P0 |
| `TransferComplete` | 按 CommandKey 关联下载任务的成败，更新任务状态与固件版本 | P1 |
| `AutonomousTransferComplete` | 自主升级完成，按策略决定是否拉取参数 | P2 |
| `GetRPCMethods` | 返回本 ACS 支持的 RPC 名列表 | P1 |
| `RequestDownload` | 罕见，返回 `RequestDownloadResponse`（可先空实现并记录） | P2 |
| `Kicked` | 记录（TR-069 A.3.2.2，含 `OldACSURL`） | P2 |
| `Fault` | 记录为设备侧错误，不视为传输失败 | P0 |

### FR-5 ACS → CPE RPC（北向下发）

| RPC | 关键点 | 优先级 |
| --- | --- | --- |
| `GetParameterValues` | 参数名数组；可含单参数与对象路径（`.` 结尾） | P0 |
| `SetParameterValues` | `ParameterKey` 必须原样回传；失败项逐条落库 | P0 |
| `GetParameterNames` | `NextLevelOnly` 语义；返回 `writable` 标记 | P0 |
| `AddObject` | 返回新 `InstanceNumber`，需回填本地模型；失败回滚 | P1 |
| `DeleteObject` | 对象名需以实例号 + `.` 结尾 | P1 |
| `GetParameterAttributes` | `Notification` / `AccessList` | P1 |
| `SetParameterAttributes` | 同上，写权限校验 | P1 |
| `Download` | FileType / URL / Username / Password / FileSize / DelaySeconds / SuccessURL / FailureURL / CommandKey | P1 |
| `Upload` | CPE 上传到 ACS 端 URL（需文件接收端点） | P2 |
| `Reboot` | CommandKey 关联；需策略控制（避免误重启） | P1 |
| 〃 | **已实现**：详情页红按钮 + 二次确认；同一设备不重复下发；设备回 RebootResponse 即完成 | ✅ |
| `FactoryReset` | 高危，默认关闭，需显式白名单 | P2 |
| `ScheduleInform` | DelaySeconds / CommandKey | P2 |

> **R-FR5-1**：所有下发 RPC 都必须带 `CommandKey`（可配置是否生成），以便和后续 Inform/TransferComplete 关联。
> **R-FR5-2**：CPE 返回 Fault 时要区分「重试有意义」（9005 Retry request / 8005）与「重试无意义」（9003 Invalid arguments 等），决定任务是否重试。

### FR-6 会话状态机与并发

- 会话由 CPE 的 POST 驱动，ACS 侧维护内存态（可选落库做审计）。
- **R-FR6-1**：同一设备（键：OUI+ProductClass+SerialNumber）的会话必须**串行**；并发到达时排队或直接拒绝（返回 503/空响应），不得交叉下发任务导致报文错配。
- **R-FR6-2**：会话有超时（默认 60s，可配置），超时释放锁并清理上下文。
- **R-FR6-3**：`cwmp:ID` 与设备绑定，用于校验「这个响应是回应我哪次下发」。
- 支持 `HoldRequests`（CPE 声明支持时可长轮询，减少空 POST 往返）—— P2。
- 会话结束（204）后必须归还设备锁、落盘会话记录。

### FR-7 设备与参数存储

- 设备身份唯一键：`(OUI, ProductClass, SerialNumber)`；`0 BOOTSTRAP` 时自动注册新设备。
- 参数以「路径 + 值 + 类型 + 可写 + 属性」形式存储，支持按前缀批量查询（对象树）。
- **R-FR7-1**：ACS 需要区分「ACS 想下发的期望值」与「CPE 实际上报值」吗？→ 第一期只存实际值 + 待下发任务，不做 last-known-good 影子模型（记入 §12 开放问题）。
- 提供参数历史（可选，按需开启，避免无限增长）。

### FR-8 任务队列

- 每设备一条 FIFO 待办队列，任务类型见 FR-5。
- 状态机：`pending → running → done | failed | expired`。
- 支持：延迟执行（`not_before`）、过期时间、重试次数与退避、去重（同设备同类同参任务合并）。
- 设备上线（Inform）时取出到期任务依次下发，一次会话内可连续下发多条。
- 提供 REST/CLI 手工入队能力（运维刚需）。

### FR-9 Connection Request（主动唤醒）

- 从设备参数读取 `ConnectionRequestURL`（可能带 `?auth=...` 查询串，需原样保留）。
- 用设备侧的 `ConnectionRequestUsername/Password` 发 HTTP GET，完成 Digest 握手。
- 结果分类：`成功 / 401 认证失败 / 连接超时 / 无 URL / 被 NAT 挡（URL 为私网且不可达）`，分类落库并在 UI 展示。
- 成功后 CPE 会在数秒内以 `6 CONNECTION REQUEST` 发起会话，此时任务才会真正下发。
- 超时可配置（默认 10s），失败不阻塞调用方（异步）。

### FR-10 预设 / 策略（Preset）

- 触发条件（至少支持）：
  - 事件码（如 `0 BOOTSTRAP`、`1 BOOT`、`2 PERIODIC`、`7 TRANSFER COMPLETE`）；
  - 首次纳管（新设备）；
  - 参数值变化（`4 VALUE CHANGE` 命中某个参数）。
- 动作序列：`SetParameterValues` / `GetParameterValues` / `AddObject` / `Download` 等，按序入队。
- 支持条件表达式（对设备已有参数做判断，如「型号 = X 且 软件版本 < Y」）——可先做简单比较。
- 预设与动作需可在不改代码的前提下增删（配置或界面）。

> **已实现**（2026-10-10）：`presets` / `preset_fires` 两张表 + 面板「预设」页增删启停，
> 不改代码不发版。触发 = 事件码（任意 Inform 事件）/ 首次纳管，可叠加参数条件
> （名字后缀匹配 + eq/ne/contains/exists 简单比较，不做表达式引擎）。动作 = SET/GET 两种
> （文本行定义，相邻同类合并成一条 RPC），Inform 命中后在**同一次会话**里按序下发。
> 默认每台设备只触发一次；编辑预设清触发记录（改过的重新生效）。AddObject/Download
> 不进预设动作（高危操作留在界面人工确认），未知动作类型跳过并留痕。

### FR-11 固件/文件管理

- 内置静态文件服务，供 CPE 直接 `Download`（CPE 是**自己拉取** URL，不经 ACS 代理）。
- 文件元数据：名称、类型（Firmware/Config/Web）、大小、SHA-256、适用型号、版本号。
- 下发 `Download` 时生成 `CommandKey`，等待 `TransferComplete` 回执关联成败。
- 支持 `DelaySeconds`（错峰，避免批量升级打垮链路）。
- 校验：CPE 上报的版本与目标版本比对，避免重复升级。

> **已实现**（2026-10-10）：面板「固件」页上传（落盘 `-files-dir`，磁盘名 = 服务端随机
> 十六进制，用户文件名只做展示元数据；SHA-256/大小入库）→ 选设备下发 Download
> （FileType 标准值、DelaySeconds 错峰、CommandKey 关联）→ 内置文件服务 `/files/<随机名>`
> 供 CPE 拉取（双端口部署挂在 CWMP 侧；不做面板鉴权，靠随机名 + 库内登记 + 精确匹配）→
> TransferComplete 按 CommandKey 结案（成功/失败码进任务历史）；接受后 2 小时未回报由巡检判失败。
> 未做：目标版本与设备当前版本自动比对防重复升级（文件元数据里有 version 字段，界面展示，
> 下发前人工确认）。

### FR-12 Web 界面（最小）

服务端模板渲染，无构建步骤。至少包含：

1. 设备列表（在线态、型号、软件版本、最后 Inform 时间、IP、事件码），支持搜索/过滤；
2. 设备详情：参数树浏览 + 单参数编辑下发 + 任务历史 + 会话/Inform 历史；
3. 任务中心：待办/失败任务、重试、手工入队；
4. 预设管理：查看/启停；
5. 日志/事件流；
6. Connection Request 一键触发。

> **已实现**（2026-09-28）：设备列表 / 设备详情（含 WAN、FTTR 子设备与终端、无线编辑、网络诊断 Ping、
> 备注、参数、任务历史、上报记录）/ 一键重启 / 删除设备 / **设置页**（ACS 监听、面板监听、面板账号密码）。
> 面板鉴权用 HTTP Basic + PBKDF2 散列；账号密码保存后立即生效，监听地址重启生效。

### FR-13 REST API

- 供自动化/脚本调用：设备 CRUD、参数读写、任务入队、预设管理、统计。
- 鉴权：Token（配置生成）或 Basic；默认只监听本机或需显式开启。
- 响应 JSON，风格与 Web UI 共用同一套内部服务层。

> **已实现**（2026-10-10 补齐鉴权）：`/api/devices`、`/api/devices/{id}`、`/api/alerts`、
> `/api/presets` 与若干 POST 动作端点，与 Web UI 共用 store 层。鉴权双通道：面板会话 cookie
> 或 `Authorization: Bearer <token>`（`-api-token` 配置，常量时间比较）；未认证回 401 JSON
> （不跳登录页）。设备 CRUD 里的删除已有（面板同源），创建走设备自己上报，不提供 POST 造假设备。

### FR-14 日志与可观测

- 结构化日志（`log/slog`，JSON 与文本两种格式可切）。
- 每条日志尽量带 `device` 维度（OUI/序列号）。
- 会话级报文记录（可选开关，注意隐私与体积）：原始 XML 可按需开启，用于排障。
- 关键计数：在线设备数、会话数、任务成功率、平均会话时长、Fault 计数。
- （P2）Prometheus `/metrics` 端点。

> **已实现**（2026-10-10 补 `/metrics`）：Prometheus 文本格式（0.0.4），零第三方依赖，
> 数字全部来自一条 `Stats()` 查询：设备/在线/探测中、参数行、任务（pending/running/failed）、
> 上报与告警存量、光功率历史行数。流水表按保留上限裁剪，如实按 gauge 暴露存量、不冒充 counter。
> 会话数/平均时长/Fault 计数暂未做进程内累计（重启归零的 counter 意义有限，先不做）。

### FR-15 设备标签与数据导出（P1）

- 设备标签：人工打的运维分组（位置 / 用途 / 批次），面板可编辑，概览页可按标签筛选；
  标签只存本系统、不下发设备；设备删除时级联清理。
- 数据导出：设备清单与光功率历史可导出 CSV（UTF-8 带 BOM，Excel 直接可用），
  鉴权与 `/api/*` 同一套（面板会话或 Bearer token）。
- 概览页「收光最弱」榜：按每台设备最近一次收光读数从低到高取前 N；没有收光数据的不参与。

> **已实现**（2026-10-11）：`device_tags` 表（迁移 #16/#17，一标签一行、联合主键去重、
> 外键级联）；详情页标签编辑（逗号分隔、≤16 个、单个 ≤24 字符，超长截断）；概览页
> chips 筛选（`/?tag=`，可与搜索 / 在线状态叠加）与行内徽章点筛；`/api/devices?tag=`
> 同款过滤；`GET /api/devices.csv` 与 `GET /api/devices/{id}/optical.csv?limit=`
> （默认 1000、上限 10000；空读数留空列，不编数）；概览页「收光最弱」榜（相关子查询
> 取每台最近一条，走 (device_id, id) 索引）。统计卡片维持「已纳管设备 / 在线」两张的既定口径。

### FR-16 面板可用性（P1）

- **设备清单页** `/inventory`：与概览页分工的资产盘点视角 —— 不筛选、不裁列，在线与离线、
  历史上纳管过的都在，低频字段（来源 IP / WAN 口 IP / 首次纳管 / 上报周期）一并列出。
- **使用帮助页** `/help`：首次接入步骤、各页面职责、常见问题、版本与升级方式。首次部署的
  运维不必去翻仓库文档 —— 面板上直接有答案。
- **机器端点鉴权口径收紧**：`/api/*` 与 `/metrics` 一旦配置了 Bearer token 就**一定校验**，
  与是否启用面板账号无关；未配置时跟随面板自身鉴权。
- **空状态与加载反馈**：空状态统一为「图标 + 标题 + 说明 + 下一步动作」，说明的是接下来
  会看到什么，而不只说「现在没有」；自动刷新前铺骨架，避免周期性白屏闪烁。
- **表格排版**：宽表在窄屏横向滚动而非压窄换行；标识符 / 时间 / 数值列不断行。

> **已实现**（2026-10-12）：`/inventory` 与 `/help` 两页；鉴权口径见 `internal/web/web.go`
> 的 `apiGuard` 与 `service.go` 的 `APIAuthOK`（两个壳必须同口径，生产 `/api/*` 走 Hertz
> 原生路由、`/metrics` 走 net/http 壳，写歪了只在生产暴露）；表格排版与空状态见
> `internal/web/static/style.css`。**这项曾有一个真实漏洞**：只配 token 而不开面板账号时，
> 两条鉴权路径都被 `Auth.Enabled()` 门控而直接放行，`/api/devices`（含序列号、来源 IP、
> 全部参数）对能访问端口的人全网可读 —— v1.4.0 已修，并有 9 种鉴权组合的断言钉住。

---

## 5. 数据模型与存储设计（草案）

选型见 §8；下表以关系型（SQLite/PG 通用）描述。

```sql
-- 设备
CREATE TABLE devices (
  id                    INTEGER PRIMARY KEY,
  oui                   TEXT NOT NULL,
  product_class         TEXT NOT NULL,
  serial_number         TEXT NOT NULL,
  manufacturer          TEXT,
  model_name            TEXT,
  data_model_root       TEXT,            -- 'InternetGatewayDevice.' | 'Device.'
  software_version      TEXT,
  hardware_version      TEXT,
  ip_address            TEXT,
  conn_request_url      TEXT,
  conn_request_username TEXT,
  conn_request_password TEXT,            -- 加密存储
  periodic_interval     INTEGER,
  last_inform_at        TIMESTAMP,
  last_boot_at          TIMESTAMP,
  last_contact_at       TIMESTAMP,
  online                BOOLEAN DEFAULT 0,
  created_at            TIMESTAMP,
  updated_at            TIMESTAMP,
  UNIQUE (oui, product_class, serial_number)
);

-- 参数（一棵扁平的树，按 name 前缀查对象）
CREATE TABLE device_params (
  device_id    INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,            -- 全路径，如 Device.WiFi.SSID.1.SSID
  value        TEXT,
  value_type   TEXT,                     -- string|int|unsignedInt|boolean|dateTime|base64
  writable     BOOLEAN,
  notification INTEGER DEFAULT 0,        -- 0 none / 1 passive / 2 active
  access_list  TEXT,
  updated_at   TIMESTAMP,
  PRIMARY KEY (device_id, name)
);

-- 任务（每设备 FIFO）
CREATE TABLE tasks (
  id           INTEGER PRIMARY KEY,
  device_id    INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  kind         TEXT NOT NULL,            -- SetParameterValues / Download / ...
  payload      TEXT,                     -- JSON
  command_key  TEXT,
  status       TEXT NOT NULL,            -- pending|running|done|failed|expired
  result       TEXT,                     -- JSON（含 FaultCode）
  retry_count  INTEGER DEFAULT 0,
  max_retries  INTEGER DEFAULT 3,
  not_before   TIMESTAMP,
  expire_at    TIMESTAMP,
  created_at   TIMESTAMP,
  started_at   TIMESTAMP,
  finished_at  TIMESTAMP
);

-- Inform / 事件流水
CREATE TABLE informs (
  id          INTEGER PRIMARY KEY,
  device_id   INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  event_codes TEXT,                      -- JSON 数组
  command_key TEXT,
  retry_count INTEGER,
  current_time TIMESTAMP,
  raw         TEXT,                      -- JSON 摘要（可选）
  created_at  TIMESTAMP
);

-- 会话审计
CREATE TABLE sessions (
  id            INTEGER PRIMARY KEY,
  device_id     INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  cwmp_id       TEXT,
  source_ip     TEXT,
  envelopes     INTEGER DEFAULT 0,
  started_at    TIMESTAMP,
  ended_at      TIMESTAMP,
  end_reason    TEXT
);

-- 文件/固件
CREATE TABLE files (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  file_type  INTEGER NOT NULL,           -- 1 Firmware / 2 Web / 3 Config
  path       TEXT NOT NULL,
  url_path   TEXT NOT NULL,
  size       INTEGER,
  sha256     TEXT,
  version    TEXT,
  model      TEXT,                       -- 适用型号（可空=通用）
  created_at TIMESTAMP
);

-- 预设
CREATE TABLE presets (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  trigger    TEXT NOT NULL,              -- JSON
  actions    TEXT NOT NULL,              -- JSON 数组
  priority   INTEGER DEFAULT 0,
  enabled    BOOLEAN DEFAULT 1,
  created_at TIMESTAMP
);

-- 日志（可选，或仅 stdout）
CREATE TABLE logs (
  id        INTEGER PRIMARY KEY,
  ts        TIMESTAMP,
  level     TEXT,
  device_id INTEGER,
  msg       TEXT,
  fields    TEXT
);
```

索引：`device_params(device_id, name)`、`tasks(device_id, status)`、`informs(device_id, created_at)`、`devices(serial_number)`。

---

## 6. 配置项（全部可覆盖，默认值合理）

以文件（TOML/YAML 二选一）+ 环境变量覆盖的方式提供：

```toml
[server]
listen       = ":7547"                     # 可被 ACS_LISTEN 覆盖
acs_url      = "http://10.0.0.1:7547/acs"  # 下发/写回 CPE 的 URL
path         = "/acs"
max_body     = "4MiB"
tls_cert     = ""                          # 为空则只跑 HTTP
tls_key      = ""

[storage]
driver = "sqlite"                          # sqlite | postgres
dsn    = "file:lightcat.db?_pragma=busy_timeout(5000)"  # PG 用连接串

[auth]
cpe_realm  = "acs"
# 账号优先从 DB 取；此项为内置/兜底账号
cpe_users  = "acs:secret"

[session]
timeout      = "60s"
max_envelopes = 1
serialize_per_device = true

[connection_request]
enabled = true
timeout = "10s"

[inform]
# 会话结束后保留参数快照的时长（历史清理）
history_retention = "720h"

[log]
level  = "info"
format = "json"                            # json | text
raw_soap = false                           # 是否记录原始报文（排障用）

[security]
log_redact = true
```

**R-CFG-1**：所有与运行环境相关的值（监听地址、ACS URL、数据库、TLS、认证凭证）必须可配置，不得硬编码到代码；
**R-CFG-2**：配置缺失时给出明确默认或明确报错，不静默使用「某台机器的事实」。

---

## 7. 非功能需求（NFR）

| 编号 | 需求 | 目标值（**待实测确认，非项目常量**） |
| --- | --- | --- |
| NFR-1 轻量 | 单二进制、无外部中间件；静态链接可跨平台交叉编译 | 二进制 < 20 MiB |
| NFR-2 并发 | 多设备并行、同设备串行；无数据竞争 | `go test -race` 全过 |
| NFR-3 容量 | 单实例纳管设备数 | 目标 1000+（待压测） |
| NFR-4 性能 | Inform 处理耗时（不含 CPE 侧网络） | P95 < 50 ms（待压测） |
| NFR-5 内存 | 1000 设备常驻内存 | < 150 MiB（待实测） |
| NFR-6 可靠性 | 进程重启后任务不丢；设备锁不残留 | 重启后 pending 任务可继续下发 |
| NFR-7 幂等 | 重复的 Inform/TransferComplete 不产生重复副作用 | — |
| NFR-8 安全 | 密码加密存储、日志脱敏、Fault 不回泄漏内部细节 | — |
| NFR-9 容错 | 脏报文/半截连接/超时不影响其他设备 | 单设备异常隔离 |
| NFR-10 可维护 | 核心协议逻辑可单测覆盖 | 关键路径单测覆盖 |
| NFR-11 可移植 | Linux 为主，代码不绑定特定宿主机 | `GOOS/GOARCH` 可交叉编译 |

---

## 8. 技术选型（建议）

| 方面 | 建议 | 理由 |
| --- | --- | --- |
| Go 版本 | 1.24+（Debian 13 仓库有 `golang-go` 1.24/1.26；或官方 tarball 1.26） | 需要 `log/slog`、`net/http` 新路由等 |
| HTTP | **CloudWeGo Hertz**（netpoll 内核；v1.3.0 及以前为 `net/http`） | 监听收养（零断点重启）、TLS、优雅退出、方法路由与 405 语义内建；API 层由 hz 按 IDL 生成 |
| XML | `encoding/xml` + 自封装容忍未知节点的编解码层 | 标准库即可，避免重型框架 |
| 存储 | **SQLite（`modernc.org/sqlite`，纯 Go，免 cgo）**；可选 `PostgreSQL`（`pgx`） | 纯 Go 才能保住「单二进制、交叉编译」 |
| 迁移 | 内嵌 SQL 迁移（自研极简 或 `pressly/goose`） | 升级不丢数据 |
| 模板/前端 | `html/template` + 原生 JS（或 htmx CDN） | 无构建链，符合轻量目标 |
| 日志 | `log/slog` | 标准库，结构化 |
| 依赖控制 | **核心协议层（internal/cwmp）零第三方依赖**；HTTP 引擎层 Hertz 为唯一框架依赖 | 协议稳定性与引擎演进分层隔离：经 415 项 e2e 与互通验证的部分不随引擎重写 |

---

## 9. 里程碑（建议迭代）

| 里程碑 | 内容 | 交付标志 |
| --- | --- | --- |
| **M0 骨架** | 项目结构、配置加载、HTTP 端点、SOAP 编解码、SQLite 迁移 | 能收发一个空 Envelope 并回 204 |
| **M1 最小闭环** | Inform + 会话状态机 + InformResponse + Get/SetParameterValues + GetParameterNames + 设备/参数落库 | 模拟 CPE 跑通「上报 → 下发设置 → 回读」 |
| **M2 可控** | 任务队列（重试/过期/去重）+ Connection Request + 设备串行锁 | 能主动唤醒设备并下发任务 |
| **M3 好用** | 预设/策略 + 最小 Web 界面 + REST API + 日志脱敏 + 结构化日志 | 不写代码也能纳管、采集、排障 |
| **M4 运维能力** | 固件 Download + TransferComplete + 文件管理 + Reboot 策略 + 指标 | 完成一次端到端远程升级 |
| **M5 加固** | mTLS、HoldRequests、PG 后端、Upload、压测与性能达标 | NFR 达标 |

---

## 10. 验收标准（可测）

1. **纳管**：全新（模拟）CPE 以 `0 BOOTSTRAP` 接入 → 设备自动登记，参数入库，AI 界面可见。
2. **下发**：对某设备设置 `...ManagementServer.PeriodicInformInterval` → CPE 侧回读值一致，`ParameterKey` 被原样回传。
3. **采集**：对对象路径 `GetParameterNames(NextLevelOnly=true)` 与 `GetParameterValues` 均返回结构正确的树。
4. **唤醒**：点「Connection Request」→ 设备以 `6 CONNECTION REQUEST` 回连并在同一会话内执行任务。
5. **任务可靠性**：断网/超时/CPE 返回 Fault 时，任务按策略重试或失败并留下可读原因；进程重启后 pending 任务继续下发。
6. **升级**：下发 `Download`(Firmware) → CPE 从内置文件服务拉取 → `TransferComplete` 回执，任务置成功且版本号更新。
7. **并发**：>= 若干台模拟 CPE 同时 Inform，无报文错配、`-race` 无告警、无锁死。
8. **脏报文**：非法 XML / 空 body / 未知 RPC / 未知命名空间 → 返回合法 Fault 或 204，服务不崩。
9. **安全**：抓取日志确认无明文密码；数据库内密码非明文。

> 验收建议用**自研 Go CPE 模拟器**（`test/cpesim`）驱动，可在 CI 里全自动跑；同时保留用真实设备/厂商工具做兼容性抽检。

---

## 11. 风险与开放问题

### 风险

| 风险 | 影响 | 缓解 |
| --- | --- | --- |
| 厂商 CWMP 实现不规范（命名空间、空 POST、MaxEnvelopes 语义、脏 XML） | 兼容性差 | 编解码层「容错优先」，保留原始报文开关用于排障 |
| CPE 的 ConnectionRequestURL 是私网地址 | 无法主动唤醒 | 第一期接受限制，文档说明；P2 再考虑 STUN |
| 固件文件大、批量升级打垮链路 | 网络拥塞 | `DelaySeconds` 错峰 + 并发上限 |
| 参数树规模大，全量刷新开销高 | 性能 | 只抓关注节点 + 增量（`4 VALUE CHANGE` 通知） |
| 无真实 CPE 验证 | 上不了生产 | 自研模拟器 + 找一台真实设备抽检 |

### 需要你拍板的开放问题

1. **存储**：只做 SQLite，还是一开始就抽象出 PostgreSQL？（影响驱动与迁移设计）
2. **规模**：预期纳管设备量级（几十 / 几百 / 几千）？决定是否需要 PG 与压测强度。
3. **目标设备**：主要管什么型号（光猫？自家路由？），支持 TR-098 还是 TR-181？有没有可拿到的真机？
4. **认证**：CPE→ACS 用 Digest/Basic 就够，还是要 mTLS？
5. **Web 界面**：是否要（P1）？还是只做 REST API + CLI？
6. **NAT 穿透**：第一期是否明确放弃 STUN 唤醒？
7. **Go 版本**：用 Debian 仓库的 1.24/1.26，还是官方最新（1.26）tarball？
8. **是否需要**「参数期望值 vs 实际值」的影子模型（做配置漂移检测）？

---

## 12. 当前环境备注

- 工作目录 `/path/to/ACS` 为空，未初始化 git。
- 本机（LMDE 7 / Debian 13）**尚未安装 Go**；`apt` 中有 `golang-go`（1.24/1.26 backports）可选。
- 编码前需先：安装 Go、`git init`、确定模块路径（如 `github.com/xxx/acs`）。

---

## 13. 工程规范对齐 oekit：差距清单（2026-10-17 审计）

> 背景：仓库已引入 `github.com/oekit/pkg`（错误链）与 `github.com/oekit/concurrency`（协程安全），
> 并按 [oekit llms.txt](https://oekit.github.io/llms.txt) 的 AI Agent 硬性规范完成第一轮对齐
> （`errors.Wrap` 全量迁移、裸 goroutine 全部换 `async.GoSafe`，见 CONTRIBUTING「代码与文档约定」）。
> 本节是对照规范逐条审计后**尚未满足**的差距，编号 OEK-*，供排期与拍板。
> 有意不采纳的条目也保留在表里并写明理由，避免后人重复评估。

### 13.1 需求列表

> 2026-10-17：OEK-1~7 已提交 GitHub 跟踪（[zy84338719/lightcat#1~#7](https://github.com/zy84338719/lightcat/issues)），
> oekit 侧短板（logx slog 后端不可注入 Handler，见 OEK-8 说明）已提交
> [oekit/logx#6](https://github.com/oekit/logx/issues/6)。下表 issue 列对应编号。

| 编号 | issue | 差距 | 规范出处 | 现状（审计证据） | 建议方案 | 优先级 | 状态 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| OEK-1 | [#1](https://github.com/zy84338719/lightcat/issues/1) | 生命周期签名不统一 | oekit AGENTS.md「生命周期」：组件 Close 统一 `Close(ctx) error` + sync.Once 幂等 | `store.Store.Close()` 是 `Close() error`，无 ctx、无幂等护栏（`internal/store/store.go:347`） | 给 Store 加 `sync.Once`；是否升级为 `Close(ctx)` 待定（调用点仅 main 一处，升级成本低但要过一遍测试） | P1 | ✅ 已完成
| OEK-2 | [#2](https://github.com/zy84338719/lightcat/issues/2) | 关停顺序存在竞态 | 同上 +「优雅关停」 | 巡检协程只受 ctx 控、无人 Wait（`cwmp.Server` 无 WaitGroup）；离线探测协程**完全不感知 ctx**（`offline.go`），main 里 `defer st.Close()` 先于它们结束执行 → 关停窗口期的探测回写打在已关闭的库上（报错丢结果，不崩进程） | 巡检与探测协程纳入统一的生命周期对象：shutdown 时先 cancel + Wait，再关 Store | P1 | ✅ 已完成
| OEK-3 | [#3](https://github.com/zy84338719/lightcat/issues/3) | 离线探测无并发上限 | oekit/concurrency gopool（协程池） | `SweepOffline` 每台待探设备起一个 goroutine，数量不受控（`offline.go:179` 一带）；批量掉线时 1000 台 = 1000 个并发 ConnectionRequest，每个最长 ConnReqTimeout（默认 10s） | 引入 gopool 池化，池大小做成配置（如 `-probe-concurrency`，默认 20）；与 NFR-3（1000+ 设备）直接相关 | P1 | ✅ 已完成
| OEK-4 | [#4](https://github.com/zy84338719/lightcat/issues/4) | 面板入队失败被静默吞掉 | oekit AGENTS.md「错误处理」：所有 error 禁止 `_` 忽略 | 三个表单处理器入队失败也重定向「成功」：`web.go` 的 `_ = s.ctrl.RequestRefresh(id)`、`_ = s.ctrl.FetchWiFi(id)`、`_ = s.ctrl.FetchSubtree(...)`；Inform 流水落库失败无日志：`handler.go:1236 _ = s.store.InsertInform(...)` | 重定向机制本就支持 `err=1` 反馈（presets 已在用），入队失败应带回反馈；InsertInform 失败至少 Warn 留痕 | P2 | ✅ 已完成
| OEK-5 | [#5](https://github.com/zy84338719/lightcat/issues/5) | 业务错误无机器可读码 | oekit AGENTS.md「错误处理」：业务错误用 BizError（BizCode/HTTPCode 双轨） | `APIError` 只有 HTTP 状态 + 文本；§2.4 修正②约定 ACS 错误码 8xxx，但 API 错误体 `{"error": "…"}` 里没有 code 字段 | `APIError` 增加 `BizCode`（8xxx 段），错误体加 `"code"`；是否以 BizError 作底层实现可在做时评估，注意不破坏 415 项 e2e 已钉死的 JSON 形状（新增字段属加法，不属改形） | P2 | ⬜ 未开始
| OEK-6 | [#6](https://github.com/zy84338719/lightcat/issues/6) | settings 表每请求读库 | oekit/pkg cache（CacheAside / SingleFlightCache） | `currentAPIToken` 与 Creds 每请求一条 SELECT（注释声明是刻意的：换「面板改完立即生效」）；SQLite `MaxOpenConns(1)` 串行化下，这条 SELECT 与所有写竞争同一连接 | 带写失效的进程内缓存：`SetSetting` 时失效对应键，「立即生效」语义不丢；**先压测拿基线再决定**（NFR-3/4 均标注待压测） | P2（压测后拍板） | ⬜ 待压测
| OEK-7 | [#7](https://github.com/zy84338719/lightcat/issues/7) | 包级可变全局指针 | oekit AGENTS.md「并发安全」：包级全局可变状态用 atomic.Pointer[T] | `biz/handler/api.Deps *web.Server` 是裸全局指针（`deps.go`），靠「先 SetDeps 再 GeneratedRegister」的装配顺序保证安全 | 改 `atomic.Pointer[web.Server]` + getter；改动小、顺手消掉一类理论上的 race | P3 | ✅ 已完成
| OEK-8 | — | 日志门面与 trace_id | oekit/logx：Ctx* 方法自动携带 trace_id；LOG_LEVEL 环境变量统一生效 | slog 直连，无 trace 抽取器；级别走 `-log-level` 旗标 | **阻塞已解除**（2026-10-17 复核）：logx **v0.4.0** 已实现 `NewSlogLogger(service, opts...)` + `WithHandler(slog.Handler)`，正是本仓当初提的 [oekit/logx#6](https://github.com/oekit/logx/issues/6)（已 COMPLETED）。go.sum 里因 `oekit/hertz` 间接锁在 v0.2.0 才看不见 | 真正的缺口只剩 **trace_id 本身**：接 `tracing/traceid` + `logx.SetTraceExtractor`，让一次设备上报的跨层日志能串起来。整包切 logx 门面（把 `*slog.Logger` 换成 `logx.Logger`）**不划算**，理由见 14.5 | P3（随观测性需求） | ⬜ 可启动 |
| OEK-9 | — | /metrics 手拼文本 | oekit/metrics（Prometheus 门面 + 弹性指标族） | 5 个 gauge 手拼文本，注释明确「零依赖、够用」 | 指标族增长（如加直方图/每设备指标）时再迁 oekit/metrics；当前迁移纯属替换不改能力 | 暂不采纳 | — 不采纳
| OEK-10 | — | 应用骨架自研 | oekit/bootstrap：App/Run 骨架 + healthz + 优雅关停 | main.go 手写信号处理、关停、句柄收养 | **不采纳**：零断点重启（listener 句柄父子进程交接、pid 交接）高度定制，bootstrap 的标准骨架覆盖不了这套流程；强行套用要 fork bootstrap | 不采纳（记录理由） | — 不采纳
| OEK-11 | — | CI 形态 | oekit 统一 CI：各仓 ci.yml 仅薄调用共享 reusable workflow | 本仓自有 ci.yml / build.yml / release.yml | **不适用**：该规则只约束 oekit 矩阵成员仓；lightcat 是 oekit 的使用方而非成员 | 不适用 | — 不适用

### 13.2 已合规、无需立项的项（审计结论，供对照）

- **错误链**：全部走 `pkg/errors.Wrap`（58 处迁移完成），同进程单一 errors 包装包，满足类型同一性红线；`APIError` 是 HTTP 边界载体、从不参与 `errors.Is/As` 判定，与 oekit errors 无类型冲突。
- **goroutine 安全**：裸 `go func` / `go method` 已清零（10 处），全部 `async.GoSafe`；ontweb 轮询保留「单轮失败跳过、循环继续」语义（外层包循环、内层包单轮）。
- **日志风格**：全仓 slog kv 结构化（`log.Info("事件", "k", v)`），与 logx 规范要求的 kv 风格同口径；logx 的 slog 后端硬编码 JSON 输出、无法注入自定义 Handler，现在切会改变用户可见的 text 日志格式——归入 OEK-8 随观测性一起动。
- **包级不可变全局**（regex 表、选项表、embed.FS、i18n 的 sync.Once 缓存）：初始化后只读，符合规范。
- **web.Options 命名**：符合「XxxOption 不许裸 Option」精神。
- **响应写侧的 `_ =` 忽略**（`w.Write`、`Encode` 等）：对端断连无处理手段，行业惯例，不算违规。

### 13.3 验收口径

- OEK-1/2：关停期间压测脚本打 Inform 流量 + `kill -TERM`，无「database is closed」类日志；`go test -race` 全绿。
- OEK-3：批量置 100 台设备离线（模拟器开关），单轮 SweepOffline 峰值并发 ≤ 配置值；巡检一轮总时长有上界。
- OEK-4：e2e 补断言——入队失败时重定向携带 `err=1`（反向验证：把注入的失败去掉断言会红）。
- OEK-5：e2e 断言错误体含 `code` 字段；既有 415 项断言全数保持绿（只加不改）。

### 13.4 第二轮落地记录（2026-10-17，OEK-1/2/3/4/7 已完成）

> 本节记录这轮实际怎么做的，重点是**与原建议方案不一致的地方**及原因，
> 避免后人按旧建议再改一遍。

**OEK-1（Store 生命周期）**：按建议只加 `sync.Once` 幂等，**没有**升级成 `Close(ctx) error`。
`*sql.DB` 没有 ctx 语义的关闭动作（ctx 在该层用于取消在飞的查询，那由 `database/sql`
自己管），照抄 ctx 参数只会得到一个恒为 nil 的假接口。已加 `Closed() bool` 与哨兵
`ErrClosed`，供后台协程自我判断「库已经关了」。幂等同时让「正常退出显式关一次 +
defer 兜底再关一次」两条路径能共存 —— 这正是 OEK-2 需要的安全网。

**OEK-2（关停顺序）**：**方案调整** —— 没用 WaitGroup 方案，改为进程级 `gopool.Pool`
（`newBackgroundPool` / `shutdownBackground`，cmd/lightcat）。理由：池的 `Close(ctx)`
自带排空语义，且 panic 兜底是内建的，不需要每个组件各写一遍 WaitGroup + recover。
关停顺序固化为 **ctx 取消 → `shutdownBackground` 排空（上限 `backgroundDrainTimeout` 5s）
→ `st.Close()`**。两处配套改造：
- ontweb 轮询原来是 `for range tick.C`，**根本不理 ctx**，SIGTERM 叫不醒它 —— 改成 `select` + ctx；
- 离线探测探测协程同样改成 ctx 感知（见 OEK-3）。

**OEK-3（探测并发）**：**没有只靠 gopool**。`gopool.Pool` 的 `MaxIdleWorkers` 不是硬上限
（超出部分「即用即走」，见其 `PoolOption` 注释），拿它当并发闸是假的。改为
buffered channel 硬闸 `Server.probeGate`（容量 `cfg.ProbeConcurrency`，新增
`-probe-concurrency` / `ACS_PROBE_CONCURRENCY`，默认 20）：
- **取闸用阻塞 + ctx**，不是「取不到就跳过」。跳过会让探测计数不前进，大批设备同时掉线时
  要多等好几个 `OfflineCheckInterval` 才凑得够 `OfflineProbeAttempts`；阻塞取槽让吞吐维持在
  闸值上限，单轮最坏时长有明确上界 `ceil(待探数/闸值) × ConnReqTimeout`。
- 另加 `startSweep`：探测变慢后一轮可能跨过下一个 tick，用互斥保证同一时刻只有一轮在飞
  （否则两轮会同时判同一台设备、把探测计数记重）。
- `SendConnectionRequestCtx` 让关停能立刻打断在飞的探测；关停期被取消的探测**不记**失败
  （否则一次正常 SIGTERM 会把全网设备推进离线判定）。

**OEK-4（静默吞错）**：按建议做。三个表单处理器改为 `redirectDeviceNotice(..., err=1)`，
`InsertInform` 失败记 Warn。

**OEK-7（全局指针）**：`atomic.Pointer[web.Server]` + `Deps()` getter，
另加 `depsOrFail(c)`：未注入时写 500 而不是 nil 解引用崩溃。

**顺带修掉的既有隐患**（不是 OEK 项，但被本轮改动踩出来）：
`web.Options.Log` 文档写「nil = 不记」，而 `Server` 上所有 `s.log.*` 调用都无判空 ——
`Register(mux, st, ctrl, Options{})` 之后走任何日志路径都会 nil 解引用崩溃。
现已在 `Register` 里兜底 `slog.Default()`。

**本轮新增回归测试**（`go test -race ./...` 全绿；`verify-s1.sh` 415/0、`verify-interop.sh` 8/0 不变）：

| 测试 | 钉住的契约 |
| --- | --- |
| `store.TestClose_Idempotent` / `_ConcurrentSafe` | 重复与并发 Close 返回同一成功结果 |
| `cwmp.TestSweepOffline_ProbeConcurrencyCapped` | 峰值并发 ≤ 闸值，且 ≥2（没被压成串行） |
| `cwmp.TestSweepOffline_CtxCancelStopsDispatch` | ctx 一取消，巡检不硬等满批次 |
| `cwmp.TestAcquireProbeSlot_ReleaseIsIdempotent` | 重复 release 不会把闸越用越宽 |
| `cwmp.TestSweepOffline_ShutdownCancelledProbeNotCounted` | 关停取消的探测不计入 ProbeCount |
| `web.TestEnqueueFailure_FeedsBackToUser` | 三处入队失败均带 `err=1` |
| `web.TestEnqueueSuccess_NoErrorFlag` | 反向验证：成功时不带 `err=1`（防「永远带 err=1」也过） |
| `main.TestShutdownBeforeStoreClose` | 排空后无后台写撞上已关闭的库 |
| `main.TestShutdownBackground_ToleratesTimeout` | 排空有上界，卡住的任务吊不住进程 |

`TestSweepOffline_ProbeConcurrencyCapped` 做过反向验证：把闸开到设备数（12）时实测峰值
确实为 12，说明「≤3」确实是闸压出来的，而不是观测本身测不出并发。


---

## 14. 第三轮：按 oekit llms.txt 全面体检（2026-10-17）

> 依据：[oekit llms.txt](https://oekit.github.io/llms.txt) 的模块地图逐条比对，
> 不只看已引入的包，而是把**尚未引入**的模块也过了一遍。
> 本节记三件事：补上的真缺口、按规范执行后的**反向修正**、以及有意不采纳的。

### 14.1 补上的真缺口

**探针端点（`oekit/hertz/health`）—— 收益最大的一项。**

审计前的状态是「健康检查没有端点」，而 deploy 目录里两处探活配置都是绕过：

| 位置 | 原来 | 问题 |
| --- | --- | --- |
| `deploy/k8s/statefulset.yaml` | startup/readiness/liveness 全打 `GET /` | `/` 是**面板首页**：渲染模板 + 查设备表 + 算统计。库一忙探针就超时，k8s 会把好端端的容器重启掉 |
| `Dockerfile` HEALTHCHECK | `wget .../${LIGHTCAT_HEALTH_PORT:-7547}/` | 默认端口就是 CWMP 端口，对 GET 一律 405；双端口部署必须手工设 `LIGHTCAT_HEALTH_PORT`，忘了就是容器永远 unhealthy |

清单里 k8s 那行注释本身就写着「未登录时 303 跳登录页，2xx/3xx 都算健康」——
这是拿「巧合」当契约。现在改为专用端点（`oekit/hertz/health`）：

- `/pingz` —— **liveness**：只答 pong，**不碰任何依赖**。库短暂不可用时若拿它做 liveness，
  k8s 会掉进「越重启越起不来」的死循环；
- `/healthz` —— **readiness**：真去 ping 一次 SQLite（`store.Ping`，新增），
  通不过就把流量摘走，不重启；
- `/buildVersion` —— 版本信息，出问题时让用户贴这一行最省事。

两个端口（ACS 与面板）**都注册**，探针配置因此不必猜端口；静态路径优先于
`NewHertzEngine` 的 `/*any` 兜底，`TestHealthEndpoints_BeatCatchAll` 守着这条优先级。
版本号经 `syncOekitVersion()` 与面板展示对齐（否则用户报障贴出来的是 `dev`）。

**OEK-5 业务错误码（`oekit/pkg/errors.BizError`）。** 8xxx 号段
（9xxx 留给 CPE→ACS 方向，见 §2.4 修正②）：`8000` 内部 / `8001` 请求不合法 /
`8003` 未认证 / `8004` 不存在 / `8005` 能力不可用 / `8006` 持久化失败。
错误体 `{"code": …, "error": …}` —— 新增 `code` 属**加法**，415 项 e2e 只做字段访问
不做整对象比对，实测 415/0 不变。

`APIError` 的底层换成 `*errors.BizError`（双轨），但**覆写了 `Error()`**：
`BizError.Error()` 自带 `biz_code:8004 ` 前缀，直接透出会污染用户可见文案。
外部仍可用 `errors.AsBizError` 统一取码。

顺带补上一条漏网路径：面板 `/api` 的 401 此前是手写 `{"error":"需要登录"}`，
不走统一 writer，漏了 `code`。是 `TestAPIError_UnauthCarriesCode` 把它揪出来的。

**ontweb JSON 韧性（`oekit/pkg/jsonrepair`）。** 真机抓取的报文我们控制不了，
厂商把 JSON 包在 markdown 代码围栏里、数组尾部多一个逗号都很常见 —— 以前整份作废。

**边界要说清楚（实测过的）**：合法的走 stdlib 快路径成本不变；
**截断的数组救不回来，仍然返回 nil** —— 宁可界面上「这次没采到」，
也不能把解析了一半的数组当完整结果显示。`TestParseHostsJSON_TruncatedStaysNil`
把这条钉住了（一开始误以为能修截断，实测不能，注释与测试都已按实测更正）。

### 14.2 按规范执行后的反向修正

| 原审计结论 | 复核后 | 理由 |
| --- | --- | --- |
| OEK-3「引入 gopool 做并发池」 | **半推翻**：闸用 channel 而非 gopool | `MaxIdleWorkers` 是「常驻上限，超出即用即走」，不是硬闸。已在 §13.4 记 |
| 探活「拿面板首页当探针」 | **推翻**：补专用端点 | 巧合当契约，且与库健康耦合 |
| `pkg` v0.7.0 | 升 **v0.9.0** | 新增 `jsonrepair`/`BizError` 都在这个版本区间 |

### 14.3 有意不采纳（复核后维持）

- **OEK-10 bootstrap 应用骨架**：llms.txt 已更新到 v0.4 的 `New(…) + b.Run(fn)` 形态，
  `fn` 参数确实比旧版的 `Run(Config{...})` 更能容纳定制流程。但本仓的零断点重启
  要在 `b.Run` **之前**完成 listener 句柄的父子交接与 pid 交接，这段逻辑挂在
  bootstrap 的生命周期之外，套用只会多一层间接。维持不采纳。
- **OEK-9 oekit/metrics**：`/metrics` 当前 5 个 gauge 手拼，能力不变、迁移不增能力。
  等指标族真要长（直方图 / 每设备指标）再说。

### 14.4 仍未采纳、值得后人有空再看

- **OEK-8 logx 门面**：复核 v0.2.0，`slogLogger` 仍硬编码 JSON 输出、
  `NewSlogLogger(service, level)` 不接受自定义 `slog.Handler`，
  且 `SetDefault` 收的是 `logx.Logger` 接口而不是 `*slog.Logger`。
  原提的 [oekit/logx#6](https://github.com/oekit/logx/issues/6) 依然成立。
- **OEK-6 settings 缓存**：文档要求「先压测拿基线再决定」，压测仍未做。

### 14.5 为什么仍然不整包切 logx 门面

复核发现 logx v0.4.0 已经支持注入自有 Handler（见 14.1 与 OEK-8 行），
所以「接 logx 会被迫换日志格式」这个理由**不再成立**。但不建议顺手把
`*slog.Logger` 全量换成 `logx.Logger`，理由：

- 本仓是**单二进制自部署的家用设备**，不是多租户微服务。logx 门面的价值主要在
  多服务统一 sink / 跨服务 trace 串联，这里都不存在；
- 全量替换要动 `internal/{cwmp,store,web,alert,ontweb,config}` 的每一个日志调用点，
  改动面极大、回归风险高，而**可观测性没有实质增加**（没有第二个服务要串联）；
- 真正缺的是 **trace_id**，那件事的正解是 `tracing/traceid` + `SetTraceExtractor`，
  可以只加一个中间件，不动现有日志调用点。

所以 OEK-8 收敛成一件事：**给一次设备上报串起 trace_id**，
且明确「不为此换日志门面」。
