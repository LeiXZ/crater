# CephFS quota-agent

quota-agent 是 storage-server 镜像的一种受限运行模式。它只注册
`/internal/storage/*` 配额接口，并通过带有 CephFS `p` 权限的专用静态 PVC
读写目录用量与配额。它不依赖 Rook toolbox，也不会给普通业务 Pod 使用的
CSI 身份增加 `p` 权限。

quota-agent 只接收由后端登录密钥派生出的内部令牌，不挂载 backend 或 storage-server
配置文件。因此它无法利用该令牌签发用户登录凭据，也不会获得数据库、LDAP 或镜像仓库配置。

## 存储类型与功能开关

目录用量与配额管理依赖 CephFS 的 `ceph.dir.rbytes` 和
`ceph.quota.max_bytes` 扩展属性，因此不支持 NFS、本地盘或其他 CSI 后端。
这些存储仍可正常用于文件浏览、上传和下载，只是不显示存储配额管理页面。

默认 Helm 配置关闭该功能：

```yaml
backendConfig:
  storage:
    quota:
      enabled: false
      rookNamespace: rook-ceph
      cephFSCSIDriver: ""
      toolboxLabelSelector: app=rook-ceph-tools
      cephFSName: cephfs
```

普通 NFS 部署应保持此配置。CephFS 部署可以显式设为 `true`；启用
`quotaAgent.enabled` 时，Chart 会自动打开该总开关并将 Provider 指向
quota-agent。即使开关被打开，后端仍会校验 PV 是否为 Rook CephFS CSI，并仅在
能力探测成功后向前端展示对应入口。

## 前提

- 存储由 Rook CephFS CSI 提供，现有共享 PVC 已绑定。
- 集群安装了 `CephClient` CRD，且 Rook Operator 正常运行。
- 执行初始化脚本的管理员可以创建 `CephClient`、Secret、PV 和 PVC。
- quota-agent 使用的 storage-server 镜像包含 `quota-agent` 运行模式。

## 创建专用存储身份

初始化脚本会读取现有 PVC 对应 PV 的 `subvolumePath`，创建仅限该路径的
CephX 客户端，并建立指向同一 CephFS 子卷的静态 PV/PVC。脚本不会输出密钥，
也不会修改 `client.csi-cephfs-node`。按照 Rook 的共享子卷要求，脚本会把原始 PV
的回收策略改为 `Retain`，防止静态 PV 仍在使用时误删底层子卷。

```bash
APP_NAMESPACE=crater-workspace \
ROOK_NAMESPACE=rook-ceph \
SOURCE_PVC=crater-rw-storage \
bash backend/hack/bootstrap-cephfs-quota-agent.sh
```

默认创建以下资源：

- `rook-ceph/CephClient/crater-quota`
- `rook-ceph/Secret/crater-quota-csi`
- `PersistentVolume/crater-quota-storage-pv`
- `crater-workspace/PersistentVolumeClaim/crater-quota-storage`

## 启动 quota-agent

在 Crater 的 Helm values 中启用组件：

```yaml
quotaAgent:
  enabled: true
  existingClaim: crater-quota-storage
```

`quota-agent` 与普通 storage-server 复用 `images.storage` 镜像；Chart 仅通过
`CRATER_STORAGE_MODE=quota-agent` 切换为受限配额模式。Rook 不在 `rook-ceph`
命名空间或使用自定义 CSI Driver/Toolbox 标签时，应覆盖上面的集群参数。

然后执行正常的 Helm 升级。Chart 会创建：

- `Deployment/crater-quota-agent`
- `Service/crater-quota-agent`

同时，Chart 会把后端配额 Provider 设置为 `storageServer`，内部地址设置为
`http://crater-quota-agent.<job-namespace>.svc:7320`。

## 不推送镜像的开发测试

开发阶段可以把本地代码交叉编译成 Linux 单文件二进制，再复制到挂载专用
CephFS PVC 的临时 Pod。该方式不需要提交代码、构建镜像或访问镜像仓库。

先完成前面的专用存储身份初始化，然后运行：

```bash
KUBECONFIG=backend/kubeconfig \
bash backend/hack/run-cephfs-quota-agent-dev.sh
```

脚本使用仓库内 `backend/.gocache`，编译 Linux/amd64 storage-server，创建临时
`crater-quota-agent-dev` Pod 和只包含内部鉴权密钥的 Secret，并将二进制复制到 Pod
中以 `quota-agent` 模式运行。Pod 只把集群已有的 storage-server 镜像作为基础运行
环境，实际执行的是刚复制进去的本地二进制。脚本不会把数据库、LDAP 等本地配置
复制到集群。

另开终端把临时 Agent 转发到本地后端配置使用的 `7330` 端口：

```bash
kubectl --kubeconfig backend/kubeconfig \
  -n crater-workspace port-forward pod/crater-quota-agent-dev 7330:7320
```

测试结束后删除临时 Pod 和鉴权 Secret：

```bash
KUBECONFIG=backend/kubeconfig \
bash backend/hack/run-cephfs-quota-agent-dev.sh --cleanup
```

专用 CephClient、静态 PV/PVC 会保留，可供后续开发测试或正式 quota-agent 使用。

## 验证

```bash
kubectl -n crater-workspace get pvc crater-quota-storage
kubectl -n crater-workspace rollout status deployment/crater-quota-agent
kubectl -n crater-workspace logs deployment/crater-quota-agent
```

日志中的启动信息应包含 `mode=quota-agent`。后端能力接口应返回
`usage_readable=true`、`quota_readable=true` 和 `quota_writable=true`，此时前端才会
显示配额修改控件。

后端不会定时扫描用户目录。管理员在存储管理页面点击“刷新用量”后，后端才会依次读取
每个用户目录的 `ceph.dir.rbytes` 并更新缓存。页面会显示每条用量的刷新时间；首次刷新完成前，
用户仍会正常列出，用量显示为“等待用量刷新”。配额功能关闭时不会显示刷新入口，也不会访问
NFS 或其他非 CephFS 存储。

静态 PV 的回收策略是 `Retain`。卸载时先删除 quota-agent 和静态 PVC/PV；不要删除
原始共享 PVC，否则可能触发原始 CephFS 子卷的回收流程。
