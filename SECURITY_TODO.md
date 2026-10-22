# 安全设计待办

更新于 2026-10-22。当前代码仍处于 P0：**输入边界收敛**。
TLS、身份认证与授权尚未实现；在此之前，server 不应暴露到不受信任的网络。

## 威胁模型

**要防的**：同网段的其他设备、误暴露的公网端口、被动抓包、冒名顶替的
client/viewer、share 内的符号链接把访问带出分享目录。

**不防的**：server 主机被控制（它本来就在转发所有内容）、本机用户、share 内
运行的恶意本地程序。server 能看到所有文件名和转发的文件内容；端到端加密
（只保证 owner 与 viewer 之间可见）尚未设计，它会与聚合索引冲突。

## 已完成：P0 输入与文件系统边界

P0 不改线格式，修的是“未受信字符串变成名字或磁盘路径后越出自己的命名空间”。

### 名字与路径

- `node.ValidName`：client/share 名字必须是单个路径组件；hello（client、
  viewer、mount）与 `Registry.Register` 都校验，拒绝 `/`、`\`、`.`、`..`
- `node.ValidRelPath`：share-relative 路径的形状定义，索引、scanner、
  server 共用一套；禁止绝对路径、反斜杠、空组件、`.` 与 `..`
- tree batch 中任一非法路径或未知 kind 会使整批被拒绝（不部分应用）
- event 中非法路径或未知 kind 使整批被丢弃且不广播；空路径仍表示该 client
  自己的 share 根
- 原因：索引原本会静默丢弃坏路径，但广播发送的是未过滤节点；`path.Dir`
  会先 Clean，因此 `evil/../victim/x` 可能落到另一个 client 的缓存目录里

测试：`TestValidName`、`TestValidRelPath`、
`TestRegistryRejectsNamesThatAreNotOnePathComponent`、
`TestServerRejectsNamesThatAreNotOnePathComponent`、
`TestServerRefusesABatchWithAnInvalidPath`、
`TestServerDropsEventsWithInvalidPaths`

### 符号链接收敛

- `Shares` 建立时记录每个 root 的 `EvalSymlinks` 规范路径
- `FS.LocalPath` 对目标父目录做符号链接解析，并要求结果仍在规范 root 下；
  最终组件不解析，保证 create/rename/remove 作用于分享内的名字本身
- `FS.localPathFollow` 供 Read/Stat 使用：跟随最终链接，但目标也必须留在
  share 内
- 分享内指向分享内的链接继续可用；指向 share 外的读写删一律拒绝
- 临时文件不再使用可预测的 `.name.peerfold.tmp`：随机名 + `O_EXCL`，
  避免本机预置符号链接和并发写共用临时文件

测试：`TestReadFollowsALinkOnlyWhenItsTargetIsInsideTheShare`、
`TestOperationsThroughAnEscapingDirectoryLinkAreRefused`、
`TestOperationsThroughAnInsideDirectoryLinkAreAllowed`、
`TestWriteToALinkNameReplacesTheLinkNotItsTarget`、
`TestWriteDoesNotFollowAPredictableTempSymlink`、
`TestConcurrentWritesToTheSameNameDoNotMix`、
`TestE2EAnEscapingSymlinkIsRefusedOnEveryOperation`

### mount 侧兜底

- event 路径必须是清洗过的绝对 FUSE 路径；含 `..`、重复分隔符或指向 mount
  根的 event 被丢弃并记一行日志
- 即使 server 被替换成不校验的旧版本，也不会通过事件污染其他 client 的
  listing
- 测试：`TestEventsCannotPatchAListingThroughDotDot`

### P0 残余风险

- `EvalSymlinks` 检查与真正 open 之间仍有 TOCTOU：本机可写者可以在窗口内
  换掉目录。彻底闭合需要 Linux `openat2(RESOLVE_BENEATH)` 与 Windows
  句柄式遍历
- 硬链接和 bind mount 不受符号链接检查约束（bind mount 属于部署配置）
- 未经认证的 peer 仍可读、写、抢占名字；P0 只保证它出不了自己的命名空间
  和 share
- 事件日志可能被大量畸形事件刷屏（每批一行），限流留给 P3

## 下一步（未实现）

### P1 传输与身份

- TLS 1.3，server 首次启动生成长期自签 ECDSA 证书，私钥 0600
- client 显式确认并固定证书指纹；指纹变化报错，不做静默 TOFU
- 每主体一个 32 字节 token，server 只存 SHA-256；TLS 内做 nonce +
  HMAC-SHA256 挑战响应；名字以 token 记录为准，消灭冒充与抢占
- token 文件与 client 凭据 0600；Windows 说明 ACL 行为
- mount 的连接池共享 `ClientSessionCache`，避免每条连接都付完整握手
- 兼容：旧 peer 默认拒绝，保留显式 `--insecure` 逃生口

### P2 授权

- token 记录 `{name, role, caps}`；viewer 默认只读，mount 的 token 显式
  授予写
- server 在 List/Read/Write 入口按发起者能力与目标 client 检查
- 后续可收紧到 client/share 白名单；status 视图可限制给管理 token

### P3 资源边界

- 未认证连接数上限，握手超时 30s → 5s
- 每 token 连接数、索引节点数、单批节点数、协议级单文件大小与配额
- 每连接并发流上限；畸形事件日志限流
- 事件表/tombstone 增长与超大 listing 见 `PERFORMANCE_TODO.md`

## 验证

```bash
GOFLAGS=-tags=fuse3 go test ./...
GOFLAGS=-tags=fuse3 go test -race ./...
GOFLAGS=-tags=fuse3 go vet ./...
GOOS=windows CGO_ENABLED=0 go build ./...
GOFLAGS=-tags=fuse3 go build ./...
```

2026-10-22 P0 落地时上述全部通过，并做过一次真实 server/client/mount
挂载冒烟：读写正常、写入后目录无重复项、share 外链接读取被拒。
