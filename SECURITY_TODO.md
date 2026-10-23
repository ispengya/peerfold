# 安全设计待办

更新于 2026-10-23。当前代码处于 **P0 + P1**：输入边界收敛、TLS 1.3、
每主体 token 认证。

**安全范围：局域网。** 能拿到登录码并完成登记的设备视为可信。P2 细粒度
授权、P3 资源边界和端到端加密在局域网范围内**有意不做**，只有服务需要
暴露到公网或接入不完全可信的设备时才重新评估。server 默认只接受 TLS
且要求 token；未登记的 peer 无法连接。

## 威胁模型

**要防的**：同网段的其他设备、误暴露的公网端口、被动抓包、冒名顶替的
client/viewer、share 内的符号链接把访问带出分享目录。

**不防的**：server 主机被控制（它本来就在转发所有内容）、本机用户、share 内
运行的恶意本地程序、局域网内已登记设备之间的越权（它们处于同一信任域，
不按 client/share 隔离读写）。server 能看到所有文件名和转发的文件内容；
端到端加密（只保证 owner 与 viewer 之间可见）不在当前范围，它会与聚合索引
冲突。

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

- server 首次启动在 `peerfold-server/` 下生成自签 ECDSA P-256 证书：
  `tls.crt` 0644、`tls.key` 0600，有效期 10 年
- server 用 `tls.Listen` 包住 listener；最低 TLS 1.3
- 登录码里带证书的 SHA-256 指纹；client/mount 用 `VerifyConnection`
  逐字节比对，不依赖系统 CA，也不做静默 TOFU
- 指纹不匹配直接失败，并给出期望/实际指纹
- 连接池共享 `ClientSessionCache`，后续读连接尽量走 TLS 会话恢复
- 证书轮换：删除证书对并重新分发登录码；没有自动轮换

### 登录码与每主体 token

- server 首次启动生成 32 字节 enrollment secret（`enroll.key`，0600），启动
  时打印：
  `login code: PF1.<证书指纹>.<enrollment secret>`
- 登录码跨重启稳定；删除 `enroll.key` 并重启即可轮换，已签发的 per-name
  token 不受影响
- 首次 `peerfold client`/`mount` 只需要粘贴这一个登录码；名字、server 地址
  和角色仍由本机设置提供
- 客户端在 TLS 握手后把 enrollment secret 放在 hello 里；server 校验后：
  - 若该名字已有 token 且匹配 → 正常登录
  - 若名字没有 token，或同名同角色重新登记 → 生成 32 字节 per-name token，
    server 只存 SHA-256，并通过 hello_ok 返回一次
  - 同名但角色不一致 → 拒绝
- client/mount 收到 issued token 后立即写回配置（0600），后续启动、重连都
  使用该 token，不再需要登录码
- 采用 **TLS 内的 bearer token**：登录码或 per-name token 在 hello 控制帧中
  发送，控制帧从不压缩；TLS 提供机密性与重放保护，server 只保存哈希
- token 文件与 client/mount 配置均为 0600；Windows 下依赖用户目录 ACL
- 认证要求 TLS：`NewWithOptions` 在只给 token 文件不给 TLS 时拒绝启动

### 启动流程

- 旧 peer 默认被 TLS server 拒绝；`peerfold server --insecure` 是显式明文
  逃生口，此时客户端没有登录码也能连接
- 已存在的旧配置在下次交互启动时会提示补登录码；直接回车可以继续显式明文
  模式（仅适用于 `--insecure` server）
- 后台/自启进程不提问：登录码在交互设置阶段写入配置，首次连接后替换成
  per-name token
- server 交互启动会打印登录码；客户端/mount 的登录码解析失败会明确报错，
  不会静默降级

测试：`TestServerCertificateIsStableAcrossLoads`、
`TestClientRejectsTheWrongPinnedCertificate`、
`TestClientTLSConfigRefusesAPlaintextServer`、
`TestTokensAreHashedAndBoundToNameAndRole`、
`TestLoginCodeRoundTripAndValidation`、
`TestSecureServerAcceptsOnlyTheTokenBoundToTheNameAndRole`、
`TestSecureServerRefusesAPlaintextHandshake`、
`TestEnrollmentIssuesATokenAndThenUsesIt`、
`TestEnrollmentDoesNotChangeARecordedRole`、`TestE2ESecureFlow`、
`TestInteractiveClientSavesLoginCode`、
`TestInteractiveMountRepairsAnIncompleteLoginCode`、
`TestInteractiveClientUpgradesAnExistingConfigWithALoginCode`、
`TestCmdServerPrintsALoginCode`

### P1 残余风险

- 登录码是登记凭据，不能贴到公开日志/聊天；泄露后删除 `enroll.key` 并重启
  即可轮换（已签发 token 仍有效，需要同时清理 `tokens.json` 里对应项）
- 任何拿到登录码的人都可以登记一个新名字；同名同角色重新登记还会替换旧
  token，让原设备下线。局域网内登录码只交给可信设备，接受这一点
- 不要对不可信的人分发登录码；登记即进入同一信任域
- `--write` 已经记录在 token 记录里，但 P2 尚未在请求入口执行读写授权；
  目前所有已认证 viewer 仍可写
- 没有 token 过期、单独撤销命令或连接频率限制；P3 补齐
- server 本机的 `ls/cat/status` 需要一份带 per-name token 的配置；否则它们
  只能连明文 server。自动为本机创建 viewer 凭据留给后续
- TLS 只保护传输；server 主机仍能看到所有文件名和转发内容

## 暂不实现（局域网范围外）

以下是公网/多租户场景才需要的控制。当前范围是局域网，已登记设备属于同一
信任域，因此有意不做；只有服务要暴露到公网或接入访客设备时才重新评估。

### P2 授权（暂不做）

局域网内登记即信任，不按 client/share 隔离读写。如果以后确实要接入访客或
半可信设备，只做最小改动：viewer 默认只读，mount 登记时显式 `--write`。

- token 记录 `{name, role, caps}`；viewer 默认只读，mount 的 token 显式
  授予写
- server 在 List/Read/Write 入口按发起者能力与目标 client 检查
- 后续可收紧到 client/share 白名单；status 视图可限制给管理 token

### P3 资源边界（暂不做）

除非出现实际资源耗尽或公网暴露，否则不做。

- 未认证连接数上限，握手超时 30s → 5s
- 每 token 连接数、索引节点数、单批节点数、协议级单文件大小与配额
- 每连接并发流上限；畸形事件日志限流
- 事件表/tombstone 增长与超大 listing 见 `PERFORMANCE_TODO.md`

### 端到端加密（暂不做）

TLS 只保护传输，server 仍能看到文件名和转发内容。局域网内 server 是自己
控制的，接受这一点；E2E 与聚合索引冲突，不在当前范围。

## 运维约束

- 默认监听 `0.0.0.0` 是为了局域网可达；不要做端口映射或在公网主机上直接
  暴露，否则登录码的暴露面会变成互联网
- 登录码只发给可信设备，不要贴到公开聊天、issue 或日志
- 设备退役或登录码泄露：删除 `enroll.key` 并重启 server 轮换登录码，同时
  清理 `tokens.json` 里不再需要的记录
- 撤销单台设备：从 `tokens.json` 删除该记录，新连接立即被拒；已建立的连接
  需要断开。没有撤销命令
- 证书轮换：删除证书对，重新分发登录码；没有自动轮换

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
