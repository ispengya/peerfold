# 性能优化待办

更新于 2026-10-21。当前口径：**单文件最大 10 MB**，更大的文件暂不处理。
更完整的设计推导在本地 `docs/performance-design.md`（该目录被 `.gitignore` 排除，不随仓库发布）。

## 本轮已完成（供后续对照）

- 目录缓存增加 `name -> node` 索引，命中后 lookup 为 O(1)
- 目录 TTL 独立为 30 秒；List 增加 singleflight；一次 miss 只记一条日志
- server 索引增量维护 `parent -> children`，`Children` 不再全表扫描
- 客户端事件合并：50 ms / 512 路径一批，按路径保留最终状态并保持变化顺序
- 新目录只 Walk 一次，同时注册 watch 和上报内容
- server 每个 watcher 一个有序、有界发送队列，广播不再乱序
- viewer 事件通道 64 -> 1024；mount 按批失效目录缓存并阻止已删除目录下的迟到子项
- 初始树上传窗口 4（原为逐批 stop-and-wait）
- 写入正确性：空文件 create、部分覆盖保留旧字节、truncate(0) 不预读、整文件覆盖不预读
- `WriteLimit` 默认 8 MiB -> 16 MiB，支持 10 MB 文件；10 MiB 回归测试已加入

## P1：读取与大文件

1. **跨 open 持久读缓存**
   - 键改为 `(path, size+mtime, chunk)`，close 不丢弃
   - 事件/版本变化时按路径失效；保持全局 LRU 上限
   - 验收：二次打开前 16 MiB 接近本地读取速度

2. **顺序预读 + 辅助 viewer 连接池**
   - 顺序访问检测，后台预取后续 4-8 块
   - 2-4 条辅助 viewer 连接；hello 增加 `watch=false`，不注册为 watcher
   - `fetchMu` 改为按 chunk 的 singleflight
   - 验收：50 ms RTT 下 64 MiB 顺序读达到 30 MB/s 以上

3. **server 读答案流式转发**
   - 现在 `router.complete` 先收齐整个答案再交给 requester
   - 改为收到 head 后逐块转发，降低首字节延迟和 server 峰值内存

4. **自适应读块**
   - 随机/小文件用 256 KiB，顺序读用 1 MiB
   - 依赖前两项完成后的真实 RTT/吞吐数据

## P1：写入

1. **fsync 语义拆分（node build 收益最大）**
   - 当前每次 Put 在 owner 上做文件 `Sync` + 目录 `Sync`
   - 普通 close/flush 不应强制持久化；只有显式 Fsync 需要
   - 需要可选 `durable` 字段和旧 peer 兼容策略；在 Windows 上实测
   - 验收：2000 个小文件保存耗时显著下降

2. **小文件批量写 / write-behind**
   - 单线程顺序构建仍是每个文件一个 RTT
   - 评估 batch-put（一次请求多个小文件）或受限 write-behind
   - 同时考虑批量 mkdir，避免 node_modules 目录树逐目录一个 RTT

3. **超过 16 MiB 的文件（暂缓）**
   - 磁盘 spool 去掉整文件内存峰值
   - dirty extent + base version 增量写回，冲突时回退整文件
   - `write_limit` 配置化

4. **Windows fsync 实测**
   - 区分杀毒/文件锁造成的延迟，再决定默认 durable 策略

## P1：Windows / 跨平台

1. **大小写不敏感路径规范化**
   - Explorer 会请求 `/HANZHIPENG`、`/HANZHIPENG/DOCS`，当前会产生重复失败 RTT
   - 精确查找失败后按 `EqualFold` 匹配目录项，并把 canonical path 用于后续操作
   - 只在大小写不敏感平台启用

2. **`--exclude` 暴露到 CLI**
   - `client.Shares` 已支持 exclude，但命令行和配置尚未暴露
   - 用于 node_modules / .git / dist，避免不必要的索引和事件

3. **WinFsp 缓存选项对齐**
   - 评估 `FileInfoTimeout` 等选项与 `AttrTimeout` / `EntryTimeout` / `ListingTimeout` 的关系

4. **fsnotify 溢出与 inotify 上限**
   - `addTree` 的 watch 失败目前被静默吞掉；记录并提示 `max_user_watches`
   - fsnotify overflow 后没有重扫，目录可能陈旧直到 30 秒 TTL
   - 后续增加“溢出 -> 重新 Scan-and-diff 该子树”

5. **Windows 实机冒烟**
   - Windows server + client + mount
   - 空文件 create、10 MB 保存、批量创建/删除、rename 覆盖
   - 当前测试套件只在 Linux 运行；Windows 仅做了交叉编译验证

## P2：规模与健壮性

1. **事件表 / tombstone 增长**
   - `Mount.events` 对删除路径保留 tombstone；长时间构建会累积
   - 评估按目录代际或 TTL 清理，同时保持删除不被陈旧 listing 复活

2. **server 索引内存**
   - 子节点索引让每个节点多一份 path 引用；超大分享需要评估内存
   - 可选：children 只存 base name，或分片锁

3. **超大目录 listing**
   - 2 万条目的 List JSON 会达到 MB 级；目前整块解码
   - 评估流式解码、分页或按需字段

4. **孤儿节点清理**
   - 父目录已删除后，批次边界外的迟到子事件仍可能在 server 索引留下孤儿
   - 评估 Apply 时校验父路径，或在 Remove 后记录代际

5. **客户端自动重连 / 心跳**
   - client 断线后需要人工重启；server/client 尚无心跳，网络分区只能等 TCP 超时

6. **初始上传调优**
   - 当前窗口固定为 4；评估 8、按字节限流和并行 Scan
   - 上传期间 watcher 仍需等 tree_done 后才广播 ready

7. **压缩 / 自适应块 / QUIC**
   - 能力协商后再做；收益依赖网络与文件类型

## 基准与验收

建议固定 fixture，后续每项优化都补一次对比：

- 1 GiB 单文件顺序读（50 ms RTT）
- 100 MiB 编辑保存（当前范围外，仅作长期参考）
- 2 万文件 / 2 千目录的合成 node_modules
- 2 千个小文件的构建写入
- 一次创建/删除 2 万文件的事件风暴

指标：首字节、总耗时、峰值 RSS、List RPC 次数、事件批次数、序号断档次数、fsync 次数。

## 已知的既有测试失败

以下问题在本轮之前就存在，与本轮改动无关：

- `internal/mount`：`TestErrnosMatchCgofuse`（平台 errno 差异）
- `internal/mount`：`TestPrepareMountPointRefusesANonEmptyDirectory`（错误文案断言）
- `internal/viewer`：`TestEmitAndCloseCannotMeetOnAClosedChannel` 在 `-race` 下报告测试自身对 `v.events` 的未同步读取

Windows 交叉编译验证命令：

```bash
GOOS=windows CGO_ENABLED=0 go build ./...
```

Linux 测试需要 FUSE 构建标签：

```bash
GOFLAGS=-tags=fuse3 go test ./...
```
