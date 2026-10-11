# Kubernetes 部署

清单在 `deploy/k8s/`，纯 YAML + kustomize，不引入 Helm（与项目零依赖的取向一致）。

## 先读我：单副本是硬约束

数据层是**单文件 SQLite（单写者）**，所以 `replicas` 只能是 1，也没有 HPA。
StatefulSet 单副本的滚动更新 = 先停旧 Pod 再起新 Pod（不会双实例抢同一块 RWO 卷），
代价是升级窗口内 CWMP 短暂不可用（分钟级以内）——光猫按自己的重试周期再连上来，
不丢数据。要真水平扩展得先换存储引擎（见 `docs/requirements.md` 的边界说明）。

## 前置

- 一个可用的 StorageClass（k3s 默认 `local-path` 即可）；
- 镜像：CI 会把多架构镜像推到 `ghcr.io/<owner>/lightcat`；也可以本地
  `docker build -t lightcat:v1.3.0 .` 后导入（k3s：`k3s ctr images import`，
  或推给自己的私有 registry）。

## 安装

```bash
kubectl create namespace lightcat

# 1) 面板凭据（Secret 不进 Git；示例见 secret.example.yaml）
kubectl -n lightcat create secret generic lightcat-secret \
  --from-literal=ACS_WEB_USER=admin \
  --from-literal=ACS_WEB_PASS='你的强密码' \
  --from-literal=ACS_API_TOKEN="$(openssl rand -hex 16)"

# 2) 按需改 configmap.yaml（重点：ACS_URL 要填光猫能访问到的地址，固件升级依赖它）

# 3) 部署
kubectl apply -k deploy/k8s/ -n lightcat

# 4) 看状态
kubectl -n lightcat get pods,svc,pvc
kubectl -n lightcat logs statefulset/lightcat -f
```

用镜像仓库时先覆盖镜像地址：

```bash
cd deploy/k8s && kustomize edit set image lightcat=ghcr.io/<owner>/lightcat:v1.3.0
# 没有 kustomize 二进制就用 kubectl：kubectl -n lightcat set image …（或直接改 YAML）
```

## 网络暴露

| Service | 默认类型 | 用途 |
|---|---|---|
| `lightcat-cwmp` (7547) | LoadBalancer | 光猫的 ACS URL：`http://<外部IP>:7547/acs`。没有 LB 实现就改 NodePort |
| `lightcat-panel` (8080) | ClusterIP | 面板/API/metrics，只进集群内网 |

面板访问三选一：

```bash
kubectl -n lightcat port-forward svc/lightcat-panel 8080:8080   # 临时：http://127.0.0.1:8080/
```

或把 `lightcat-panel` 改 NodePort；或启用 `ingress.yaml`（改 host、配好 IngressClass
与证书后加入 kustomization 的 resources）。TLS 在 Ingress 终结时容器内**不要**再配
`ACS_TLS_*`；运营商固件强制 ACS URL 走 https 时，让 CWMP 也走 Ingress/反代即可
（SOAP 就是普通 POST）。

## 存储与权限

- PVC `data-lightcat-0`（volumeClaimTemplates，默认 2Gi，存库 + 固件，按需调大）；
- Pod 以非 root（10001）运行、根文件系统只读，只有 `/data` 与 `/tmp` 可写；
- **权限兜底**：`local-path`/`hostPath` 这类不支持 `fsGroup` 的存储，靠清单里的
  `fix-data-perms` initContainer（以 root chown /data）解决；云盘/Longhorn 等支持
  fsGroup 的 CSI 会自动调属主，此时 initContainer 可删；
- 命名空间若启用了 `restricted` Pod Security 标准（禁止 root 容器），initContainer
  会被拒——删掉它并确保存储支持 fsGroup。

## 升级

```bash
cd deploy/k8s && kustomize edit set image lightcat=ghcr.io/<owner>/lightcat:v1.3.1
kubectl apply -k deploy/k8s/ -n lightcat
kubectl -n lightcat rollout status statefulset/lightcat
```

数据库迁移随启动自动执行。回滚 = 把镜像 tag 改回去再 apply（注意：迁移只前进
不回退，跨了 schema 变更的版本回滚前先用备份恢复 `/data`）。

## 备份与恢复

```bash
# 备份：停写后拷 /data（短暂停 Pod 保证库文件一致）
kubectl -n lightcat scale statefulset/lightcat --replicas=0
kubectl -n lightcat run lightcat-backup --rm -it --restart=Never \
  --image=alpine --overrides='{"spec":{"containers":[{"name":"lightcat-backup","image":"alpine","command":["sh","-c","tar czf - -C /data . > /backup/lightcat-$(date +%F).tar.gz"],"stdin":true,"tty":true,"volumeMounts":[{"name":"data","mountPath":"/data"},{"name":"backup","mountPath":"/backup"}]}],"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"data-lightcat-0"}},{"name":"backup","hostPath":{"path":"/tmp"}}]}}'
kubectl -n lightcat scale statefulset/lightcat --replicas=1
```

嫌上面绕的话，最朴素的方式同样成立：`kubectl cp lightcat/lightcat-0:/data ./backup-dir`
（Pod 在跑时拷 SQLite 有小概率拷到中间态，重要备份还是先缩到 0）。
恢复 = 把 tar 解回 PVC 再起 Pod。

## 可观测

- `/metrics`（Prometheus 文本格式）挂在 panel Service 上，抓取带
  `Authorization: Bearer <ACS_API_TOKEN>`；
- 日志走 stdout（`ACS_LOG_JSON=true` 切 JSON，方便集群日志栈解析）；
- 探针：startup/readiness/liveness 都打面板端口 `/`（未登录 303 也算健康）。
  **不要**把探针指到 CWMP 端口——它对 GET 一律 405。

## 排障速查

| 症状 | 原因 → 处理 |
|---|---|
| Pod `CreateContainerConfigError` | `lightcat-secret` 没建 → 按上文第 1 步创建 |
| Pod `Pending`，Events 有 `unbound PVC` | StorageClass 没配好 → `kubectl get sc`，在 volumeClaimTemplates 里指名 |
| 日志 `permission denied` on /data | 存储不支持 fsGroup 且删了 initContainer → 把 `fix-data-perms` 加回来 |
| `ImagePullBackOff` | 镜像名/tag 错，或私有仓库没配 imagePullSecret |
| 光猫连不上 | ACS URL 是否用了**光猫可达**的地址（LB/NodePort 外部 IP）；`kubectl logs` 里有没有「收到 Inform」 |
| 固件升级设备拉不到文件 | `ACS_URL` 没配或配了集群内地址 → 改成外部可达 URL |
