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

**Windows 与 Linux 可互相混部**：server 在一边、client 在另一边、mount 两边都能挂。协议走 TCP，路径在 wire 上统一用 `/`。

---

## 安装

从源码构建：先 clone，再编译。

```sh
git clone https://github.com/ispengya/peerfold.git
cd peerfold

./setup-server.sh          # Linux
.\setup-server.ps1         # Windows

./setup-client.sh          # Linux
.\setup-client.ps1         # Windows
```

脚本会装依赖（Go + FUSE/WinFsp）、编译、写 PATH，最后直接进入交互式启动：已有配置就直接读取，没有就当场询问，然后选择前台或后台运行。需要 Go 1.27+；发行版的 `golang` 太旧时，先从 https://go.dev/dl/ 安装官方工具链。

Linux 手工编译要带 fuse3 标签（脚本已处理）：

```sh
# Debian/Ubuntu
sudo apt install libfuse3-dev gcc
# RHEL / OpenCloudOS 系
sudo dnf install fuse3-devel gcc

go build -tags=fuse3 -o peerfold ./cmd/peerfold
```

---

## 使用

三个角色都是**不带参数就是交互式 setup**，带参数就直接跑：

```sh
peerfold server    # 聚合服务端
peerfold client    # 共享本地目录
peerfold mount     # 挂载聚合树
```

带参数示例：

```sh
# 服务端
peerfold server --listen 0.0.0.0:7000

# 共享端（可多个 --share）
peerfold client --server 1.2.3.4:7000 --name alice \
  --share docs=/home/alice/docs \
  --share photos=/home/alice/photos

# 挂载端
peerfold mount --server 1.2.3.4:7000 --mount ~/peerfold
```

### 常用命令

```sh
peerfold status
peerfold stop server
peerfold stop client alice
peerfold stop all
peerfold restart server
peerfold ls / cat / put / mkdir / rm / mv
```

### 开机自启

交互式选 Background 时可以启用自启：

- **Windows**：写入 HKCU `...\CurrentVersion\Run`
- **桌面 Linux**：写入 `~/.config/autostart/peerfold-*.desktop`（XDG）
- **无桌面 Linux**：用 systemd user unit

```ini
# ~/.config/systemd/user/peerfold-server.service
[Unit]
Description=peerfold server
After=network.target

[Service]
ExecStart=%h/.local/bin/peerfold server --daemon --listen 0.0.0.0:7000
Restart=on-failure

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now peerfold-server.service
```

### 清理

```sh
./cleanup.sh          # Linux（从源码目录）
.\cleanup.ps1         # Windows
```

删配置、日志、自启项、演示目录、构建产物。不动 FUSE/WinFsp 和系统 PATH。

---

## 跨平台混部注意

| 事项 | 说明 |
|---|---|
| 路径 | wire 上统一 `/`；本地路径用各平台的分隔符 |
| 主机名 | client 默认名：Windows 用 `COMPUTERNAME`，Linux 用 `hostname` |
| 挂载点 | Windows 由 WinFsp 创建目录；Linux 要求目录**已存在且为空** |
| 挂载依赖 | Windows 要 WinFsp；Linux 要 fuse3（`sudo apt install fuse3`） |
| 下线语义 | client 断开 → 挂载里对应子树消失；最后一个 owner 下线 → 挂载点释放 |

## 日志

```
~/peerfold-server/log.txt
~/peerfold-client-<名字>/log.txt
```

挂载端有自己的配置目录（server 地址、挂载点、缓存参数）：

```
~/peerfold-mount/config.json
```

Windows 上是 `C:\Users\你\peerfold-server\log.txt` 这种形式。

日志记录每次 list / read / write / mkdir / rename / remove / 文件变化推送。

## License

MIT
