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

## Windows 一键使用

> 以**管理员身份**运行 PowerShell 或 CMD 均可。
> 有 `.ps1` 和 `.bat` 两个版本，效果一样。

### 1. 启动 server（服务端）

```powershell
.\setup-server.ps1
```

或 CMD：
```cmd
setup-server.bat
```

自动安装 Go + WinFsp → 构建 → 启动 server。已有的依赖会跳过。

### 2. 启动 client（共享端）

在需要共享文件的机器上（同一台也行）：

```powershell
.\setup-client.ps1
```

或 CMD：
```cmd
setup-client.bat
```

自动安装 Go + WinFsp → 构建 → **输出启动命令**，复制粘贴运行即可。

也可以直接指定名字和共享目录：

```powershell
.\setup-client.ps1 -Name alice -ShareDir C:\data\documents
```

### 3. 挂载（查看端）

```powershell
.\peerfold.exe mount --server 127.0.0.1:7000 --mount C:\Users\你\peerfold
```

挂载点打开后就能像普通文件夹一样读写。

### 4. 清除所有环境

```powershell
.\cleanup.ps1
```

或 CMD：
```cmd
cleanup.bat
```

删除所有配置、日志、演示目录、构建产物，并卸载 WinFsp，不留痕迹。

## 日志

每个组件的日志在用户主目录下的独立文件夹里：

```
C:\Users\你\peerfold-server\log.txt
C:\Users\你\peerfold-client-<名字>\log.txt
```

日志记录每次 list / read / write / mkdir / rename / remove / 文件变化推送。

## License

MIT
