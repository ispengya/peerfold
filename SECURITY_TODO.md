# 安全设计待办

更新于 2026-10-22。当前代码处于 **P0 + P1**：输入边界收敛、TLS 1.3、
每主体 token 认证。**P2 授权与 P3 资源边界尚未实现**。
server 默认只接受 TLS 且要求 token；未登记的 peer 无法连接。

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
- 在显式 `--insecure` 模式下，未经认证的 peer 仍可读、写、抢占名字；
  P0 只保证它出不了自己的命名空间和 share
- 事件日志可能被大量畸形事件刷屏（每批一行），限流留给 P3

## 已完成：P1 传输与身份

### TLS 1.3

- server 首次启动（或首次运行 `peerfold invite`）在 `peerfold-server/` 下生成
  自签 ECDSA P-256 证书：`tls.crt` 0644、`tls.key` 0600，有效期 10 年
- server 用 `tls.Listen` 包住 listener；最低 TLS 1.3
- client/mount 固定证书指纹：invite 携带 SHA-256 指纹，连接用
  `VerifyConnection` 逐字节比对；不依赖系统 CA，也不做静默 TOFU
- 指纹不匹配直接失败，并给出期望/实际指纹
- 连接池共享 `ClientSessionCache`，后续读连接尽量走 TLS 会话恢复
- 证书轮换：删除证书对并重新给所有机器发 invite；没有自动轮换，避免
  静默断开已登记机器

### 每主体 token

- `peerfold invite --name <name> --role client|viewer [--write] [--server host:port]`
  生成 32 字节随机 token，server 端 `tokens.json` 只存 SHA-256；重复同名
  invite 会轮换旧 token
- token 与名字、角色绑定；server 在注册前校验，未知名字、错误 token、角色
  不匹配返回同一句 “authentication failed”
- 采用 **TLS 内的 bearer token**：token 在 hello 控制帧中发送，控制帧从不
  压缩；TLS 提供机密性与重放保护，server 只保存哈希。没有做 HMAC 挑战响应，
  因为那需要 server 持有 token 明文或引入 PAKE，收益不及复杂度
- token 文件与 client/mount 配置均为 0600；Windows 下依赖用户目录 ACL
- 认证要求 TLS：`NewWithOptions` 在只给 token 文件不给 TLS 时拒绝启动

### invite 与启动流程

- `peerfold invite` 输出一条 `pf://host:port?fp=…&name=…&role=…&token=…`
- 首次 `peerfold client`/`mount` 粘贴 invite，一次保存地址、指纹、principal
  和 token；之后正常启动不再提问
- 已存在的旧配置在下次交互启动时会提示粘贴 invite 完成升级；直接回车可以
  继续显式明文模式（仅适用于 `--insecure` server）
- 客户端升级时 invite 的名字必须与已有配置的 client name 一致，否则拒绝，
  避免把凭据保存到另一个名字的配置里
- 旧 peer 默认被 TLS server 拒绝；`peerfold server --insecure` 是显式明文
  逃生口，此时客户端没有 invite 配置也能连接
- 后台/自启进程不提问：凭据在交互设置阶段写入配置
- server 交互启动会打印指纹和 `peerfold invite` 提示；客户端/mount 的 invite
  解析失败会明确报错，不会静默降级

测试：`TestServerCertificateIsStableAcrossLoads`、
`TestClientRejectsTheWrongPinnedCertificate`、
`TestClientTLSConfigRefusesAPlaintextServer`、
`TestTokensAreHashedAndBoundToNameAndRole`、`TestInviteRoundTripAndValidation`、
`TestSecureServerAcceptsOnlyTheTokenBoundToTheNameAndRole`、
`TestSecureServerRefusesAPlaintextHandshake`、
`TestE2ESecureFlow`、`TestInteractiveClientSavesInvite`、
`TestInviteCommandProducesAWorkingViewerCredential`、
`TestInteractiveMountRepairsAnIncompleteInvite`

### P1 残余风险

- invite 里的 token 是凭据，不能贴到公开日志/聊天；需要泄露时重新生成同名
  invite 即可轮换
- `--write` 已经记录在 token 记录里，但 P2 尚未在请求入口执行读写授权；
  目前所有已认证 viewer 仍可写
- 没有 token 过期、单独撤销命令或连接频率限制；P3 补齐
- server 本机的 `ls/cat/status` 需要一份带 invite 的配置；否则它们只能连
  明文 server。自动为本机创建 viewer 凭据留给后续
- TLS 只保护传输；server 主机仍能看到所有文件名和转发内容

## 下一步（未实现）

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
