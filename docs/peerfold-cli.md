# peerfold 命令行

`peerfold` 是一个把多台机器上的目录聚合成一棵树的程序。它只有一条强制规则：
**连接方向恒定** —— server 监听，client 主动拨号出去。因此 client 可以躲在 NAT
后面，server 需要能被所有人访问。

本文件描述 v1 的全部命令行接口。

```
usage: peerfold <command> [args]

commands:
  server  run the aggregation server
  client  join a server and share local directories
  ls      list the aggregated tree
  cat     print a file from the aggregated tree
  put     write stdin to a file in the aggregated tree
  mkdir   create a directory in the aggregated tree
  rm      remove a file or directory in the aggregated tree
  mv      rename within one client's tree
```

## 快速上手

三台终端，同一个 `--server`：

```sh
# 1. 服务端
peerfold server --listen 127.0.0.1:7000

# 2. 供货方：把本机两个目录共享出去
peerfold client --server 127.0.0.1:7000 --name win \
  --share Documents=/home/me/Documents \
  --share Photos=/home/me/Pictures

# 3. 查看聚合后的树
peerfold ls
```

`ls` 输出：

```
d	win
d	win/Documents
f	win/Documents/notes.txt
d	win/Photos
```

第一个字段是类型：`d` 目录、`f` 文件、`?` 本版本不认识的类型（如实报出，不猜）。

## 配置文件

`server` 与 `client` 共用一个 JSON 配置文件，默认位置：

- Linux: `$XDG_CONFIG_HOME/peerfold/config.json`（通常是 `~/.config/peerfold/config.json`）
- macOS: `~/Library/Application Support/peerfold/config.json`
- Windows: `%AppData%\peerfold\config.json`

用 `--config PATH` 覆盖。`ls` / `cat` / `put` / `mkdir` / `rm` / `mv` 从这里读取
要连接的 `server` 地址，所以**必须先跑过一次 `peerfold client`**，它们才知道去哪。

```json
{
  "listen": ":7000",
  "server": "127.0.0.1:7000",
  "client_name": "win",
  "shares": { "Documents": "/home/me/Documents" },
  "aggregate": false
}
```

`Load`/`Save` 是严格互逆的：不填默认值、不做归一化。省略的键保持零值。

## server

```
usage: peerfold server [flags]
```

运行聚合服务端：所有 client 都拨号到它，它持有全局索引。**监听直到被中断**
（Ctrl-C）。没有任何 client 时它也可以先起来。

| 标志 | 说明 |
| --- | --- |
| `--listen ADDR` | 监听地址。默认取配置里的 `listen`，再退回 `:7000`。只影响本次运行，不写回配置。 |
| `--config PATH` | 配置文件路径。 |

` server` 不写配置文件：client 的运行要留下"下次仍然共享这些目录"，而 server
没有任何需要累积的东西。

`--listen` 与 client 的 `--server` 不匹配是配置错误，但服务端检测不到 —— 它只
知道自己绑在哪。

## client

```
usage: peerfold client [flags]
```

拨号到 server 并共享本机目录。**标志会合并进配置文件，且在任何连接尝试之前写盘**，
所以一个恰好没起来的 server 不会连带丢掉你刚声明的共享。稍后重跑同一个命令即可
用同样的设置连上。

| 标志 | 说明 |
| --- | --- |
| `--server ADDR` | 要拨号的 server（`host:port`）。 |
| `--share NAME=PATH` | 共享一个目录。**可重复**。 |
| `--name NAME` | 本机在树里的名字。默认取本机主机名。 |
| `--aggregate` | 请求成为聚合客户端。 |
| `--config PATH` | 配置文件路径。 |

未给出的标志不会擦掉配置里已有的值 —— 传 `--server` 不会清空 `shares`。

**共享名不能包含路径分隔符**（`/` 或 `\`）。一个共享名是一个目录，它出现在
client 这一层下面，例如上面的 `win/Documents`。

## ls

```
usage: peerfold ls [PATH]
```

列出聚合树。`PATH` 的三种形态：

| `PATH` | 含义 |
| --- | --- |
| 省略 | 已连接的 client 名字 |
| `win` | 该 client 的共享（共享名） |
| `win/Documents` | 该 client 某一个共享里的目录 |

**列出的路径是 client 相对的**：`ls win/Documents` 打印 `Documents/notes.txt`，
不是 `win/Documents/notes.txt`。唯一完全限定的是**推送事件**里的路径。

## cat

```
usage: peerfold cat PATH
```

把一个文件原样写到 stdout。`PATH` 形如 `win/Documents/notes.txt`。

二进制安全：不换行、不转码、不做行尾处理。

## put

```
usage: peerfold put [--exclusive] PATH
```

把 **stdin** 写到 `PATH` 所在机器上，覆盖原有内容。

| 标志 | 说明 |
| --- | --- |
| `--exclusive` | `PATH` 已存在就失败，而不是覆盖。 |

`--exclusive` 是给"绝不能覆盖一个我并不知道的文件"的调用方的。先检查再写**不是**
同一件事：这里检查和写入是一步。

```sh
echo hello | peerfold put win/Documents/notes.txt
peerfold put --exclusive win/Documents/only-once.txt < payload.bin
```

写入是**整文件回写**：内容先落到目标同目录的临时文件，`Sync` 之后再 `Rename`
覆盖过去。所以 `put` 返回成功时字节已经落在供货方的磁盘上，而不是写进了内存队列。
代价是改一个字节也要传整个文件。

## mkdir

```
usage: peerfold mkdir PATH
```

在拥有该路径的机器上建目录。**父目录不会被创建**：路径写错就失败，而不是替
你搭出一棵没人指名的树。

已存在则失败。

## rm

```
usage: peerfold rm PATH
```

删掉一个文件，或一个**空**目录。

非空目录会被拒绝：删除不是递归的，所以一个写错的路径不能把一整棵树带走。

## mv

```
usage: peerfold mv OLD NEW
```

把 `OLD` 改名为 `NEW`。**两端必须指出同一个 client、同一个共享**，因为改名是
一个文件系统里的一步，它从来不是"复制 + 删除"。

```sh
peerfold mv win/Documents/draft.txt win/Documents/final.txt
```

跨 client 的改名由 **viewer 端**拒绝（此时它还知道两端的 client 名）；跨共享的
改名由 **拥有该路径的 client** 拒绝（此时只有它知道自己的共享表）。server 不做
这件事 —— 它拿到的是剥掉 client 名之后的路径，看不出区别。

## v1 已知限制

以下是 v1 **有意不做**的事，以及做了但边界明确的事。

**没有 `watch` 子命令。** `viewer` 包提供 `Watch()` 返回推送通道，但它没有对应的
命令行入口。v1 的命令就是上面八个。

**没有挂载。** 没有 FUSE / WinFsp 层，`peerfold` 不是文件系统。它是一个命令行
程序：想看就看一眼，想写就写一次。

**没有鉴权、没有加密。** 能连到 server 端口的人就是 server。不要把 `--listen`
暴露到不受信任的网络上。

**没有分页 / 断点续传。** 读和写都以整文件为单位流式传输，但不是随机访问的
文件系统接口。`cat` 无法从中间开始。

**索引是最终一致的，不是事务性的。** 索引反映的是供货方通知的目录状态；一个
正在写入的文件可能先以旧大小出现。变化的传播走文件系统通知，有微小延迟。

**删除的传播依赖通知。** 文件和目录的增删改都会重新扫描并推送给观察者，但
在极端情况下（通知被合并、事件在传输中丢掉）索引要等到下一次扫描才追上。

**路径长度受宿主机限制。** 在 Windows 上默认最大路径 260 字符；长路径需要
宿主机启用相应策略，否则相关测试会跳过。

**`ls` 的类型字母只有三个。** `d`、`f`，以及 `?`（本版本不认识的类型）。v1
的协议目前只有目录和文件两种节点。

**一个共享就是共享名这一层。** 共享名不能包含分隔符，共享也不嵌套 ——
`win/Documents` 是一个共享，`win/Documents/sub` 是它里面的目录。

**server 起不来时会立刻报错，不会重试。** 端口被占用在命令返回前就报出来，
不会先打印"正在运行"再失败。

**client 连不上时会带回非零退出码，但共享已经写进配置了。** 这是有意的：
把声明的共享留在盘上，重跑即可。调用方若需要"连上才算数"，必须自己看退出码。