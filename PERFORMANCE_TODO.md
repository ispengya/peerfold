# 性能优化待办

更新于 2026-10-22。当前口径：**单文件最大 10 MB**，更大的文件暂不处理。
更完整的设计推导在本地 `docs/performance-design.md`（该目录被 `.gitignore` 排除，不随仓库发布）。

## 已完成

### 增量索引与事件

- 目录缓存增加 `name -> node` 索引，命中后 lookup 为 O(1)
- 目录 TTL 独立为 30 秒；List 增加 singleflight；一次 miss 只记一条日志
- server 索引增量维护 `parent -> children`，`Children` 不再全表扫描
- 客户端事件合并：50 ms / 512 路径一批，按路径保留最终状态并保持变化顺序
- 新目录只 Walk 一次，同时注册 watch 和上报内容
- server 每个 watcher 一个有序、有界发送队列，广播不再乱序
- viewer 事件通道 64 -> 1024；mount 按批失效目录缓存并阻止已删除目录下的迟到子项
- 初始树上传窗口 4（原为逐批 stop-and-wait）

### 写入正确性

- 空文件 create、部分覆盖保留旧字节、truncate(0) 不预读、整文件覆盖不预读
- `WriteLimit` 默认 8 MiB -> 16 MiB，支持 10 MB 文件；10 MiB 回归测试已加入

### 本轮（第一、第二梯队）

- `--exclude` 暴露到 CLI 与配置：node_modules / .git / dist 不扫描、不 watch、不索引
- fsync 语义拆分：普通 flush/close 只保证原子 rename，显式 `Fsync` 才持久化；
  写请求携带 `durability`，缺省值仍是 durable，兼容旧 viewer
- 读/写/List/Mkdir/Remove/Rename 全部走辅助 viewer 连接池（hello `watch=false`），
  主连接只保留 watch；元数据操作不再排在事件 pump 的 20 ms 读窗口后面
- 读路径去掉全局 `fetchMu`，按 (path, generation, unit, chunk) singleflight；
  顺序读预读 4-8 块；读块按实测 RTT 在 256 KiB-1 MiB 间调整
- 跨 open 持久读缓存：按路径失效，close 不再丢弃；事件/本地写/rename/reconnect 失效
- 目录缓存增量更新：子项事件直接 patch 父 listing，不再整体失效
- server 读答案收到 head 后逐块转发
- 读路径日志降噪：只记录慢请求和错误
- 连接池在 reconnect 换 caller 时关闭旧连接；watch 停止后到达的事件不再应用

**关于“批量元数据写”**：FUSE 的 mkdir/create/close 是逐个同步调用，构建工具会等待
每个 close 的结果，因此真正的跨文件 batch-put 只能靠 write-behind（提前返回）或
协议级多文件请求，而前者会削弱“close 成功即已保存”的承诺。本轮用连接池让并发的
元数据/写请求真正并行，并去掉每个文件的 fsync；跨文件 batch 与 write-behind 仍列在
下面的后续项中。

## 第一梯队（已完成）

1. `--exclude` CLI/配置
2. fsync 语义拆分
3. 并发元数据与写请求（连接池）；跨文件 batch 仍待定
4. 读并发、预读与连接池

## 第二梯队（已完成）

5. 跨 open 持久读缓存
6. 目录缓存增量更新
7. server 读答案流式转发
8. 自适应读块与日志降噪

## 第三梯队（仅记录，暂不实施）

1. **Windows 大小写不敏感路径规范化**
   - Explorer 会请求 `/HANZHIPENG`、`/HANZHIPENG/DOCS`，当前会产生重复失败 RTT
   - 精确查找失败后按 `EqualFold` 匹配目录项，并把 canonical path 用于后续操作
   - 只在大小写不敏感平台启用

2. **WinFsp 缓存选项对齐**
   - 评估 `FileInfoTimeout` 等选项与 `AttrTimeout` / `EntryTimeout` / `ListingTimeout` 的关系

3. **fsnotify 溢出与 inotify 上限**
   - `addTree` 的 watch 失败目前被静默吞掉；记录并提示 `max_user_watches`
   - fsnotify overflow 后没有重扫，目录可能陈旧直到 listing TTL
   - 增加“溢出 -> 重新 Scan-and-diff 该子树”

4. **Windows 实机冒烟与 fsync 实测**
   - Windows server + client + mount
   - 空文件 create、10 MB 保存、批量创建/删除、rename 覆盖
   - 区分杀毒/文件锁造成的延迟，再决定默认 durability 策略
   - 当前测试套件只在 Linux 运行；Windows 仅做交叉编译验证

5. **事件表 / tombstone 增长**
   - `Mount.events` 对删除路径保留 tombstone；长时间构建会累积
   - 评估按目录代际或 TTL 清理，同时保持删除不被陈旧 listing 复活

6. **server 索引内存**
   - 子节点索引让每个节点多一份 path 引用；超大分享需要评估内存
   - 可选：children 只存 base name，或分片锁

7. **超大目录 listing**
   - 2 万条目的 List JSON 会达到 MB 级；目前整块解码
   - 评估流式解码、分页或按需字段

8. **孤儿节点清理**
   - 父目录已删除后，批次边界外的迟到子事件仍可能在 server 索引留下孤儿
   - 评估 Apply 时校验父路径，或在 Remove 后记录代际

9. **客户端自动重连 / 心跳**
   - client 断线后需要人工重启；server/client 尚无心跳，网络分区只能等 TCP 超时

10. **初始上传调优**
    - 当前窗口固定为 4；评估 8、按字节限流和并行 Scan
    - 上传期间 watcher 仍需等 tree_done 后才广播 ready

11. **压缩 / QUIC**
    - 能力协商后再做；收益依赖网络与文件类型

12. **超过 16 MiB 的文件（暂缓）**
    - 磁盘 spool 去掉整文件内存峰值
    - dirty extent + base version 增量写回，冲突时回退整文件
    - `write_limit` 配置化；补 truncate 的上限检查（当前 truncate 可绕过限制）

## 基准与验收

当前仓库还没有可重复运行的 benchmark。下一轮优先补 fixture，再对每项优化做对比：

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
