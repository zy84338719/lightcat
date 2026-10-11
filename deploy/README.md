# LightCat 光猫管家 —— 安装包

单二进制、零外部中间件的 TR-069/CWMP ACS（管理光猫 / FTTR 主机）。
项目与源码：https://github.com/zy84338719/lightcat
（源自 https://github.com/hakureiyuyuko/go-acs ，AGPL-3.0，见 README 出处声明）

本包是**二进制 / systemd** 的安装方式；Docker 与 Kubernetes 部署见仓库里的
`deploy/DOCKER.md` 和 `deploy/k8s/K8S.md`（不随本包分发）。

## 包里有什么

| 文件 | 说明 |
|---|---|
| `lightcat` | 主程序（Linux 静态二进制，无动态库依赖）|
| `acs` | 同一份二进制的兼容硬链接（老版本升级脚本按这个名字找）|
| `install.sh` | 安装（装 systemd 服务，可重复运行 = 原地升级）|
| `update.sh` | 升级（从 GitHub 拉新版，校验、备份、失败自动回滚）|
| `uninstall.sh` | 卸载（默认保留数据，`--purge` 连数据一起删）|
| `lightcat.service` | systemd 单元模板（`install.sh` 会按参数填好，装成 `acs.service`）|
| `VERSION` | 版本号 |

## 安装

```bash
VERSION=1.3.0                                  # 换成你下载的那个版本（≤1.2.x 的包名是 acs-*）
tar xzf lightcat-$VERSION-linux-amd64.tar.gz
cd lightcat-$VERSION-linux-amd64
sudo ./install.sh
```

装完会打印面板地址与要填进光猫的 ACS URL。常用选项：

```bash
sudo ./install.sh --web-listen :8080                 # 面板单独一个端口
sudo ./install.sh --web-user admin --web-pass 'xxx'  # 顺便设面板账号密码
sudo ./install.sh --dry-run                          # 先看它要做什么，不动系统
sudo ./install.sh --help                             # 全部选项
```

默认落点：

| 项目 | 路径 |
|---|---|
| 主程序 | `/usr/local/bin/lightcat` |
| 数据目录（数据库、升级备份）| `/var/lib/lightcat` |
| 配置文件 | `/etc/default/lightcat` |
| systemd 单元 | `/etc/systemd/system/acs.service`（服务名保持 `acs`，升级不断档）|
| 运行用户 | `acs`（系统用户，不可登录）|

从 ≤v1.2.x 的老落点（`/usr/local/bin/acs`、`/var/lib/acs`、`/etc/default/acs`）升级时，
脚本会**自动沿用老位置**（数据不搬家、服务不更名）；`update.sh` 换二进制写的是
当前单元实际引用的路径，两种布局都升得动。

## 给光猫配 ACS URL

- CWMP 端点：`http://<本机IP>:7547/acs`
- 面板：`http://<本机IP>:7547/`
- 光猫上只允许填根路径的设备也支持：`http://<本机IP>:7547/`

面板用 `--web-listen` 挪到独立端口后，CWMP 那个端口**任何路径都受理**，不用担心运营商定制固件的路径写法。

## 升级

```bash
sudo ./update.sh                # 拉最新版
sudo ./update.sh --check        # 只看有没有新版本
sudo ./update.sh --tag v1.2.3   # 升到指定版本
sudo ./update.sh --file lightcat-$VERSION-linux-amd64.tar.gz   # 内网/离线，用本地包
```

升级流程：下载 → 校验 SHA256 → 停服务 → 旧二进制备份到 `/var/lib/lightcat/backups/`
（默认留最近 3 个）→ 换新 → 起服务 → 健康检查，**起不来自动回滚**。
数据库与配置不动。

> 也可以直接用新版安装包里的 `install.sh` 原地升级：它保留 `/etc/default/lightcat` 与数据库。

## 卸载

```bash
sudo ./uninstall.sh          # 只拆服务与主程序，保留数据库、配置、用户
sudo ./uninstall.sh --purge  # 连数据库、配置、服务用户一起删
```

## 端口

| 端口 | 用途 |
|---|---|
| `7547` | CWMP（TR-069），光猫连进来的口 |
| 面板端口 | 默认与 7547 同一个；`--web-listen` 可分开 |

要监听 1024 以下的端口（如 `:80`），把 `lightcat.service` 里那两行
`AmbientCapabilities` / `CapabilityBoundingSet` 的注释去掉再重装。

## 配置

配置全在 `/etc/default/lightcat`（环境变量），改完 `sudo systemctl restart acs`。
变量名与命令行参数一一对应，完整列表见 README 的「配置」一节。

面板的监听地址改动**重启生效**；面板登录账号密码保存后**立即生效**（其中密码以 PBKDF2 散列存在数据库里）。
面板走**独立登录页 + 会话 cookie**（不是 HTTP Basic），顶栏有「退出」；忘了密码用 `ACS_WEB_AUTH=off` 起一次即可。

## 两个提醒

- 单元里开了 `ProtectSystem=strict`，服务只允许写数据目录；把数据库挪到别处时，记得把它加进 `ReadWritePaths`。
- 默认部署走**明文 HTTP**（登录密码也是明文传输），别把端口直接暴露到公网：放内网，
  或配 `-tls-cert` / `-tls-key`（`ACS_TLS_CERT` / `ACS_TLS_KEY`）原生开 HTTPS ——
  配了之后 ACS 与面板两个监听一起切 TLS（光猫那侧支不支持 HTTPS 要看固件，普遍不支持时
  可以面板单独走 `-web-listen` + TLS 前置，或继续用 nginx 做来源限制）。走 HTTPS 时登录 cookie 自动带 Secure。
