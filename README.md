# peerfold

一个联邦文件系统：把多台机器上的本地目录聚合成一棵树，挂载成本地文件夹使用。

```
┌──────────┐      ┌──────────┐
│  server  │◄────►│ client A │──► 共享目录 A
│ (聚合索引)│      └──────────┘
│          │      ┌──────────┐
│          │◄────►│ client B │──► 共享目录 B
└──────────┘      └──────────┘
      │
      ▼
┌──────────┐
│   mount  │──► 挂载点（像普通文件夹一样读写）
└──────────┘
```

## Windows 运行条件

| 条件 | 要求 | 获取方式 |
|---|---|---|
| 操作系统 | Windows 10/11 (x64) | — |
| Go | ≥ 1.27 | [go.dev/dl](https://go.dev/dl/) |
| WinFsp | ≥ 2.0 | [winfsp.dev](https://winfsp.dev/)（挂载必需） |
| 管理员权限 | 挂载时需要 | 以管理员身份运行终端 |

> **WinFsp** 是 Windows 上的 FUSE 兼容层，安装后系统就能把用户态文件系统挂载成普通目录。不安装 WinFsp 可以运行 server/client，但无法挂载。

## 构建

```powershell
git clone https://github.com/<your-name>/peerfold.git
cd peerfold
go build -o peerfold.exe ./cmd/peerfold
```

## 运行

在**四个终端**（或四台机器）上分别运行：

### 1. 启动 server

```powershell
.\peerfold.exe server --listen 127.0.0.1:7000
```

### 2. 启动 client（每台共享机器一个）

```powershell
.\peerfold.exe client --server 127.0.0.1:7000 --name alice --share docs=C:\Users\alice\Documents
```

同一台机器跑多个 client 时用不同的 `--name`：

```powershell
.\peerfold.exe client --server 127.0.0.1:7000 --name bob --share docs=C:\Users\bob\Documents
```

### 3. 挂载聚合树

```powershell
.\peerfold.exe mount --server 127.0.0.1:7000 --mount C:\Users\you\peerfold
```

> 挂载点目录**不能预先存在**（WinFsp 会自动创建）。

### 4. 使用

挂载完成后，`C:\Users\you\peerfold` 下就是联邦文件系统：

```
C:\Users\you\peerfold\
  alice\docs\...    ← 来自 client alice 的共享
  bob\docs\...      ← 来自 client bob 的共享
```

像普通文件夹一样读、写、创建、重命名、删除即可。写入会自动同步回对应 client 的真实磁盘。

## CLI 命令

| 命令 | 说明 |
|---|---|
| `peerfold server` | 启动聚合服务器 |
| `peerfold client` | 加入服务器并共享本地目录 |
| `peerfold mount` | 把联邦树挂载成本地目录 |
| `peerfold ls [PATH]` | 列出聚合树 |
| `peerfold cat PATH` | 打印一个文件 |
| `peerfold put PATH` | 写入 stdin 到文件 |
| `peerfold mkdir PATH` | 创建目录 |
| `peerfold rm PATH` | 删除文件或目录 |
| `peerfold mv OLD NEW` | 重命名 |

## 配置与日志

配置和日志都在用户主目录下，每个组件一个文件夹：

```
~/peerfold-server/
  config.json          ← server 配置
  log.txt              ← server 日志

~/peerfold-client-<name>/
  config.json          ← client 配置
  log.txt              ← client 日志（每次读/写/推送都有记录）
```

日志格式：`时间戳 [info/debug/error] 内容`，记录每次 list / read / write / mkdir / rename / remove / 事件推送。

## 当前限制

- **无认证 / 无加密**——仅适合可信局域网，勿暴露公网
- **整文件写回**——每次保存全文件上传，大文件编辑会慢
- **Linux 挂载未验证**——核心逻辑跨平台，但真实 Linux FUSE 挂载未测试

## 测试

```powershell
# 单元测试
go test ./... -count=1

# 真实挂载 e2e（需要 WinFsp）
$env:PEERFOLD_E2E_MOUNT = '1'
go test ./internal/mount -run '^TestE2ERealMount$' -count=1 -v
```
