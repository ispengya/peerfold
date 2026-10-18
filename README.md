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

## Windows 环境准备

### 1. 安装 Go

下载并安装 Go 1.27 或更高版本：

```
https://go.dev/dl/
```

安装完成后验证：

```powershell
go version
# 应输出 go1.27.x windows/amd64
```

### 2. 安装 WinFsp

WinFsp（Windows File System Proxy）是 Windows 上的 FUSE 兼容层，挂载功能依赖它。**不安装 WinFsp 可以运行 server/client，但无法挂载。**

**下载地址**：https://winfsp.dev/rel/

选择 **WinFsp 2.x Installer（.msi）** 下载并安装。安装过程中：
- 勾选 **Developer**（如果需要开发 FUSE 文件系统，可不勾）
- 默认安装路径即可（`C:\Program Files (x86)\WinFsp`）

安装完成后验证：

```powershell
# 服务注册成功即可，无需手动启动
Get-Service WinFsp.Launcher
# Status 应为 Running
```

如果服务没启动：

```powershell
Start-Service WinFsp.Launcher
```

### 3. 管理员权限

挂载文件系统需要管理员权限。后续所有 `peerfold mount` 命令都要**以管理员身份运行终端**。

右键 PowerShell / Windows Terminal → **以管理员身份运行**。

## 获取源码

```powershell
git clone https://github.com/ispengya/peerfold.git
cd peerfold
```

## 构建

### 方式一：一键配置（推荐）

以**管理员身份**运行 PowerShell：

```powershell
.\setup.ps1
```

自动完成：检查/安装 Go → 检查/安装 WinFsp → 构建 binary → 创建配置目录 → 生成启动脚本。

如果只想构建不想装依赖：

```powershell
.\setup.ps1 -Quick
```

配置完成后直接启动：

```powershell
.\setup.ps1 -Start
```

### 方式二：手动构建

```powershell
go build -o peerfold.exe ./cmd/peerfold
```

验证构建成功：

```powershell
.\peerfold.exe
# 应输出命令列表（server / client / mount / ls / cat / put / mkdir / rm / mv）
```

## 运行

### 一键启动（推荐）

以管理员身份运行：

```powershell
.\start-all.ps1
```

自动启动 server + alice + bob + mount，并创建测试文件。停止：

```powershell
.\stop-all.ps1
```

### 手动启动（四个终端）

需要**四个终端窗口**（或分布在四台机器上）。建议先准备好共享测试目录：

```powershell
mkdir C:\peerfold-demo\alice, C:\peerfold-demo\bob | Out-Null
Set-Content C:\peerfold-demo\alice\hello.txt "hello from alice"
Set-Content C:\peerfold-demo\bob\hello.txt "hello from bob"
```

### 终端 1 — 启动 server

```powershell
cd D:\github\peerfold
.\peerfold.exe server --listen 127.0.0.1:7000
```

看到 `listening on 127.0.0.1:7000` 即启动成功。此终端保持运行。

### 终端 2 — 启动 client alice

```powershell
cd D:\github\peerfold
.\peerfold.exe client --server 127.0.0.1:7000 --name alice --share docs=C:\peerfold-demo\alice
```

看到 `joined 127.0.0.1:7000 as alice (client)` 即连接成功。

### 终端 3 — 启动 client bob

```powershell
cd D:\github\peerfold
.\peerfold.exe client --server 127.0.0.1:7000 --name bob --share docs=C:\peerfold-demo\bob
```

> 同一台机器跑多个 client 时，`--name` **必须不同**（server 按名字隔离索引）。

### 终端 4 — 挂载（以管理员身份）

```powershell
cd D:\github\peerfold
.\peerfold.exe mount --server 127.0.0.1:7000 --mount C:\Users\你\peerfold
```

看到 `The service peerfold has been started.` 即挂载成功。此终端保持运行。

> **挂载点目录不能预先存在**。WinFsp 会自动创建它，进程退出时自动删除。如果上次运行残留了目录，先删掉再挂载：`Remove-Item C:\Users\你\peerfold -Force`

### 使用

挂载完成后，打开 `C:\Users\你\peerfold`，能看到两棵子树：

```
C:\Users\你\peerfold\
  alice\docs\hello.txt     ← 来自 client alice
  bob\docs\hello.txt       ← 来自 client bob
```

像普通文件夹一样读、写、创建、重命名、删除即可。写入会自动同步回对应 client 的真实磁盘。

### 验证同步

在 `C:\peerfold-demo\alice` 下直接创建一个文件（绕过挂载），挂载点里应该几秒内能看到：

```powershell
Set-Content C:\peerfold-demo\alice\new-file.txt "synced from owner disk"
Start-Sleep -Seconds 2
Get-Content C:\Users\你\peerfold\alice\docs\new-file.txt
# 应输出 synced from owner disk
```

## CLI 命令

| 命令 | 说明 | 示例 |
|---|---|---|
| `peerfold server` | 启动聚合服务器 | `peerfold server --listen 127.0.0.1:7000` |
| `peerfold client` | 加入服务器并共享目录 | `peerfold client --server 127.0.0.1:7000 --name alice --share docs=C:\data` |
| `peerfold mount` | 挂载联邦树 | `peerfold mount --server 127.0.0.1:7000 --mount C:\mnt` |
| `peerfold ls` | 列出聚合树 | `peerfold ls` |
| `peerfold cat` | 打印文件内容 | `peerfold cat alice/docs/hello.txt` |
| `peerfold put` | 写入文件 | `echo hi \| peerfold put alice/docs/new.txt` |
| `peerfold mkdir` | 创建目录 | `peerfold mkdir alice/docs/newdir` |
| `peerfold rm` | 删除 | `peerfold rm alice/docs/old.txt` |
| `peerfold mv` | 重命名 | `peerfold mv alice/docs/a.txt alice/docs/b.txt` |

> `ls` / `cat` 等命令需要用 `--server` 或配置文件指定 server 地址。

## 配置与日志

配置和日志在用户主目录下，每个组件一个文件夹：

```
C:\Users\你\
  peerfold-server\
    config.json          server 配置（listen 地址）
    log.txt              server 日志
  peerfold-client-alice\
    config.json          alice 的配置（server 地址、共享目录）
    log.txt              alice 的日志
  peerfold-client-bob\
    config.json          bob 的配置
    log.txt              bob 的日志
```

日志格式 `时间戳 [info] 内容`，记录每次 list / read / write / mkdir / rename / remove / 文件变化推送。查看日志：

```powershell
Get-Content C:\Users\你\peerfold-server\log.txt -Tail 20
Get-Content C:\Users\你\peerfold-client-alice\log.txt -Tail 20
```

## 测试

```powershell
# 单元测试（不需要 WinFsp）
go test ./... -count=1

# 真实挂载 e2e（需要 WinFsp + 管理员权限）
$env:PEERFOLD_E2E_MOUNT = '1'
go test ./internal/mount -run '^TestE2ERealMount$' -count=1 -v
```

## 当前限制

- **无认证 / 无加密** —— 仅适合可信局域网，勿暴露公网
- **整文件写回** —— 每次保存全文件上传，大文件编辑会慢
- **Linux 挂载未验证** —— 核心逻辑跨平台，但真实 Linux FUSE 挂载未测试

## License

MIT
