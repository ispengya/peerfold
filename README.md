<p align="center">
  <img src="./assets/readme/hero.svg" width="100%" alt="peerfold：server 保存全局索引，client 共享本机目录，mount 把多台机器的目录挂成一个普通文件夹">
</p>

<p align="center">
  <strong>把多台机器上的目录，挂成一个普通文件夹。</strong><br>
  文件不搬家；Linux 与 Windows 可以混布。
</p>

---

## 为什么用它

文件散在两三台机器上时，SCP、rsync、网盘都会留下副本和同步状态。peerfold 换一种做法：

- **一个挂载点看全部**：每台机器把自己的目录共享出来，挂载端看到的是一棵完整的树。
- **读写像本地目录**：`ls`、`cat`、`put`、`mkdir`、`rm`、`mv` 都是普通文件和目录操作。
- **文件不搬家**：文件始终留在原来的机器上，server 只保存索引、转发请求。
- **跨平台混布**：server、client、mount 可以任意放在 Linux 或 Windows 上。

## 它是怎么工作的

<p align="center">
  <img src="./assets/readme/workflow.svg" width="100%" alt="peerfold 工作方式：server 保存全局索引，client 共享本机目录，mount 连上 server 后得到一棵挂载树">
</p>

- **server** — 聚合服务端，持有全局索引，不保存文件内容。
- **client** — 把本机的目录按名字共享给 server，可以同时共享多个目录。
- **mount** — 连上 server，把当前所有 client 的共享目录拼成一棵树。

协议走 TCP，路径在 wire 上统一用 `/`；本地路径使用各平台自己的分隔符。

## 快速开始

需要 **Go 1.27+**。Linux 还需要 FUSE3，Windows 需要 WinFsp；setup 脚本会尝试自动装好依赖、编译、写 PATH。发行版的 `golang` 太旧时，先从 https://go.dev/dl/ 安装官方工具链。

### 装好并启动

在作为 server 的机器上：

```sh
git clone https://github.com/ispengya/peerfold.git
cd peerfold

./setup-server.sh          # Linux
.\setup-server.ps1         # Windows
```

`setup-server` 最后会直接进入交互式启动：已有配置就直接读取，没有就当场询问，然后选择前台或后台运行。

server 默认监听 `0.0.0.0:7000`，这样其他机器上的 client/mount 才能连上来。协议没有鉴权，只在本机使用时应显式改成 `--listen 127.0.0.1:7000`。

在每台要共享目录的机器上：

```sh
./setup-client.sh          # Linux
.\setup-client.ps1         # Windows
```

`setup-client` 只负责构建和安装命令，完成后提示你手动运行 `peerfold client`——这样重跑脚本来更新二进制时，不会顺带把 client 拉起来。装完手动启动：

```sh
peerfold client
```

在要挂载的机器上（装过任意一个 setup 脚本即可）：

```sh
peerfold mount
```

第一次运行 mount 会询问 server 和挂载点，之后从 `~/peerfold-mount/config.json` 读取。client 和 mount 各自独立启动：一个只负责共享本地目录，一个只负责挂载聚合树。

### 直接带参数启动

三个角色都是**不带参数进入交互式 setup，带参数就直接跑**。脚本化部署可以这样写：

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

Linux 的挂载点必须**已存在且为空**；Windows 上由 WinFsp 创建目录。

<details>
<summary>手工编译（不用 setup 脚本）</summary>

Linux 编译要带 fuse3 标签（脚本已处理）：

```sh
# Debian/Ubuntu
sudo apt install libfuse3-dev gcc
# RHEL / OpenCloudOS 系
sudo dnf install fuse3-devel gcc

go build -tags=fuse3 -o peerfold ./cmd/peerfold
```

</details>

## 日常使用

| 命令 | 作用 |
| --- | --- |
| `peerfold status` | 查看 server / client / mount 的运行状态 |
| `peerfold stop server` | 停止服务端 |
| `peerfold stop client alice` | 停止指定 client |
| `peerfold stop mount` | 停止挂载（Linux 先落盘再卸载） |
| `peerfold stop all` | 全部停止 |
| `peerfold restart server` | 后台重启 |
| `peerfold ls` / `cat` / `put` / `mkdir` / `rm` / `mv` | 直接操作聚合树 |

`peerfold mount` 运行期间会记录自己的 PID 和实际使用的 server / 挂载点，所以从另一个终端也能用 `peerfold status` 看到它；`peerfold stop mount` 会先落盘再卸载（Linux 通过 SIGTERM 触发正常退出，Windows 上是强制结束进程）。

### status 能看到整个联邦

`peerfold status` 会连一次配置里的 server，列出**所有连到这台 server 的 mount 和 client**，包括其他机器上的：

```text
Remote mounts via 124.221.247.88:7000:
  WINBOX  C:\Users\me\peerfold  from 117.154.103.159:22739  up 3m12s

Clients via 124.221.247.88:7000:
  alice  (aggregate)  from 10.0.0.7:52344  up 8m5s
```

mount 在握手时声明主机名和挂载点，client 握手时声明名称，server 记录连接来源和时间；所以 A 机器上的 mount/client 在 B 机器的 status 里也可见。

旧版 server 不认识这些查询，status 会提示更新并重启 server，而不是假装没有远程连接。要让远程信息可见，server 和对应的 mount/client 都需要更新到包含该功能的版本。

## 跨平台混部注意

| 事项 | 说明 |
| --- | --- |
| 路径 | wire 上统一 `/`；本地路径用各平台的分隔符 |
| 主机名 | client 默认名：Windows 用 `COMPUTERNAME`，Linux 用 `hostname` |
| 挂载点 | Windows 由 WinFsp 创建目录；Linux 要求目录**已存在且为空** |
| 挂载依赖 | Windows 要 WinFsp；Linux 要 fuse3（`sudo apt install fuse3`） |
| 下线语义 | client 断开 → 挂载里对应子树消失；最后一个 owner 下线 → 挂载点释放 |
| 远程连接 | mount/client 在握手时向 server 登记；任何能连到该 server 的机器都能在 `status` 里看到 |

## 常驻运行与开机自启

交互式选 **Background** 时可以启用自启：

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

## 日志

```text
~/peerfold-server/log.txt
~/peerfold-client-<名字>/log.txt
```

挂载端有自己的配置目录（server 地址、挂载点、缓存参数）：

```text
~/peerfold-mount/config.json
```

`chunk_size`（或 `--chunk`）控制每次向 owner 读取的字节数，默认 512 KiB，最大 1 MiB。高延迟链路可以调大以减少往返；小文件为主时可以调小以降低首字节延迟。`buffer_limit` / `buffer_total` 控制读缓存的单文件/总量上限。

Windows 上是 `C:\Users\你\peerfold-server\log.txt` 这种形式。日志记录每次 list / read / write / mkdir / rename / remove / 文件变化推送，并带有耗时和字节数（例如 `mount: read ... took=... bytes=...`、`server: read ... took=...`、`client: read ... took=...`），可以直接对比慢在网络请求、server 转发还是 owner 读盘。

## 清理

cleanup 脚本会**先停掉正在运行的前台/后台进程**（mount 会正常卸载），再删除配置、日志、自启项、演示目录和构建产物。不动 FUSE/WinFsp 和系统 PATH。

```sh
./cleanup.sh          # Linux（从源码目录）
.\cleanup.ps1         # Windows
```

## License

MIT
