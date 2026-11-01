# opencode-gc

OpenCode V2 垃圾清理工具，使用 Go 编写，命令行解析使用 [`go-arg`](https://github.com/alexflint/go-arg)，SQLite 驱动使用纯 Go 的 [`modernc.org/sqlite`](https://modernc.org/sqlite)（可 `CGO_ENABLED=0` 构建）。

清理范围包括：

- `<tmp>/opencode` 下的临时文件
- OpenCode data 目录下的旧日志、孤立 shell 输出、孤立 tool-output、无主 repos
- `opencode.db` 中的孤立记录、无会话项目、可再生成的缓存
- 可选的旧会话删除、归档会话删除、旧消息 reasoning 压缩
- 会话大小分析、按大小/消息数/时间排序
- 按 session ID 定点删除会话（可连同子会话）
- WAL checkpoint + `VACUUM` 空间回收

默认只读预览，只有加 `--apply` 才会真正删除。

## 构建

```sh
go mod download
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o opencode-gc.exe .
```

Linux/macOS：

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o opencode-gc .
```

要求 Go 1.26+（本机使用 Go 1.27）。

## 命令

```text
opencode-gc [scan] [flags]          扫描并报告（默认命令，只读）
opencode-gc clean [flags]           清理；默认预览，--apply 执行
opencode-gc sessions list [flags]   分析会话大小并按字段排序
opencode-gc sessions delete [flags] 按 session ID 定点删除会话
opencode-gc vacuum [flags]          WAL checkpoint + VACUUM
opencode-gc backup [flags]          创建一致的数据库备份
```

路径自动探测：

1. 优先执行 `opencode debug paths`，读取 data/tmp/state/db 等真实路径
2. 失败时回退到 `~/.local/share/opencode`、`~/.cache/opencode`、`os.TempDir()/opencode`
3. 可用 `--data-dir`、`--tmp-dir`、`--db` 覆盖；`--no-detect` 跳过 CLI 探测

## 常用示例

```sh
# 只读扫描，查看所有垃圾
opencode-gc scan

# 预览安全清理（默认选择 tmp、logs、shell、DB 孤立记录）
opencode-gc clean

# 清理所有非历史垃圾（含 repos、无会话项目、DB 缓存），并回收空间
opencode-gc clean --all --apply

# --all 叠加 --older-than：再额外清理旧会话、归档会话和 reasoning
opencode-gc clean --all --older-than 30d --apply

# 只清临时文件，保留 7 天内的文件
opencode-gc clean --tmp --apply

# 统一时间范围：预览 30 天前的文件、项目、旧会话、归档会话和 reasoning
opencode-gc scan --older-than 30d
opencode-gc clean --older-than 30d
opencode-gc clean --older-than 30d --apply

# 统一时间范围，但只清临时文件
opencode-gc clean --older-than 30d --tmp --apply

# 查看会话大小排行（默认按总大小降序）
opencode-gc sessions
opencode-gc sessions list --sort size --limit 10
opencode-gc sessions list --sort messages --project elona --older-than 30d

# 定点删除会话（默认只预览）
opencode-gc sessions delete ses_xxx
opencode-gc sessions delete ses_xxx --apply
opencode-gc sessions delete ses_xxx ses_yyy --apply
opencode-gc sessions delete ses_xxx --with-children --apply

# 删除 30 天未活动的会话（含其消息）
opencode-gc clean --sessions 30d --apply

# 删除归档超过 30 天的会话
opencode-gc clean --archived 30d --apply

# 删除旧消息中的 reasoning 内容，但保留会话和正文
opencode-gc clean --strip-reasoning 30d --apply

# 删除无 session 且创建超过 30 天的项目
opencode-gc clean --db-projects --project-age 30d --apply

# 删除可重新下载的 models-dev 缓存（约 11 MB）
opencode-gc clean --db-cache --apply

# 预览空间回收，然后执行
opencode-gc vacuum
opencode-gc vacuum --apply

# 备份到指定文件
opencode-gc backup --out D:\backup\opencode-20261101.db
```

## 统一时间范围

`--older-than DUR` 是全局时间范围，用来替代各分类单独的时长参数：

```sh
opencode-gc scan --older-than 30d
opencode-gc clean --older-than 30d --apply
```

规则：

- 未指定具体时长时，`--older-than` 同时作为文件年龄、项目年龄、会话年龄、归档年龄和 reasoning 年龄。
- 不带类别参数时，`--older-than` 会启用所有“时间类”清理：
  `tmp`、`logs`、`shell`、`tool-output`、`db-orphans`、`db-projects`、旧会话、归档会话、reasoning、VACUUM。
- 不使用 `--all` 时，显式类别参数会缩小范围，例如：
  - `--older-than 30d --tmp`：只清 30 天前的 tmp 文件
  - `--older-than 30d --db-projects`：只删 30 天前创建且无 session 的项目
  - `--older-than 30d --sessions 90d`：只删超过 90 天未活动的会话
- 具体时长参数优先于 `--older-than`：
  - `--age 7d`
  - `--project-age 60d`
  - `--sessions 90d`
  - `--archived 90d`
  - `--strip-reasoning 14d`
- `--all` 覆盖所有非历史垃圾；与 `--older-than` 叠加时会额外加入旧会话、归档会话和 reasoning，不会缩减范围。
- 统一模式包含 VACUUM，因此需要先停止 OpenCode 服务；否则数据库部分会被跳过并返回退出码 2。

常用组合：

```sh
# 只清理 30 天前的东西，先预览
opencode-gc clean --older-than 30d

# 统一清理并回收空间
opencode-gc clean --older-than 30d --apply

# 文件 7 天，其他时间类 30 天
opencode-gc clean --older-than 30d --age 7d --apply

# 先清理所有非历史垃圾（文件 7 天）
opencode-gc clean --all --age 7d --apply

# 再单独删除 90 天前的会话
opencode-gc clean --sessions 90d --apply
```

注意：不使用 `--all` 时，`--sessions 90d` 既是时长也是类别开关，会缩小为“只清会话”；在 `--all` 模式下它只会追加会话类别。如果要同时清文件和会话但使用不同阈值，请显式列出文件类别，或分两次执行。

## 会话分析与定点清理

### 列出会话

```sh
opencode-gc sessions
opencode-gc sessions list --sort size --limit 10
opencode-gc sessions list --sort messages
opencode-gc sessions list --project elona
opencode-gc sessions list --older-than 30d
opencode-gc sessions list --json
```

- `sessions` 不带子命令时等价于 `sessions list`。
- `--sort` 支持：`size`（默认）、`messages`、`updated`、`created`、`cost`、`tokens`。
- `--limit 20` 默认显示 20 条，`--limit 0` 显示全部。
- `--project TEXT` 按项目名、worktree、directory、session ID、slug、title 过滤。
- `--older-than DUR` 只显示超过该时间未活动的会话。
- 大小 = 消息数据 + blob + session_part + instruction_state；默认按总大小降序。
- 表格标记：
  - `*` 当前会话或受保护链
  - `!` 24h 内更新过
  - `*!` 两者都是
- `--verbose` 显示 slug/title/parent/children/tokens/cost/directory 和大小明细。
- `--json` 输出完整字段，适合脚本筛选 ID。

### 定点删除

```sh
# 预览
opencode-gc sessions delete ses_xxx
opencode-gc sessions delete ses_xxx ses_yyy

# 执行
opencode-gc sessions delete ses_xxx --apply
opencode-gc sessions delete ses_xxx ses_yyy --apply

# 连同子会话一起删除
opencode-gc sessions delete ses_xxx --with-children --apply

# 允许删除 24h 内更新过的会话（受保护会话仍不会删除）
opencode-gc sessions delete ses_xxx --force --apply

# 删除后回收空间
opencode-gc sessions delete ses_xxx --vacuum --apply

# JSON 输出
opencode-gc sessions delete ses_xxx --json
```

规则：

- 默认只预览，`--apply` 才删除。
- 当前会话及其祖先/后代会话始终受保护，`--force` 也不能删除。
- 默认拒绝删除 24h 内更新过的会话；`--force` 可绕过 24h 限制。
- 默认只删除指定的会话；如果它有子会话，子会话会保留并把 `parent_id` 置空，预览会提示数量。
- `--with-children` 会递归删除所有子会话；如果子树中包含受保护或 24h 内更新的会话，整个目标会被跳过。
- 自动清理由此产生的孤立 blob/instruction/event/job/schedule/inbox 记录。
- 写库前默认自动备份（`autobackup-*.db`，保留 3 份）；`--no-backup` 可关闭。
- OpenCode 服务运行时默认跳过数据库写入并返回退出码 2；`--allow-running` 可强制写入。
- `--vacuum` 需要服务已停止，否则会跳过并提示。
- 删除后如果仍有空闲页，会提示执行 `opencode-gc vacuum --apply`。

## 清理类别

`*` 表示 `clean` 不带类别参数时的默认选择；`clean --all` 命中所有非历史类别（含 repos、无会话项目、DB 缓存和 VACUUM），但不含旧会话、归档会话和 reasoning。

| 类别 | 位置 | 规则 | 默认 | `--all` |
|---|---|---|---|---|
| `tmp` | `<tmp>/opencode` | mtime 超过 `--age`（默认 7d）的文件 | * | 是 |
| `logs` | `<data>/log` | mtime 超过 `--age` 的日志 | * | 是 |
| `shell` | `<data>/shell` | 未被消息引用或项目已不存在的 `.out`，且超过 `--age` | * | 是 |
| `tool-output` | `<data>/tool-output` | 未被消息引用，且超过 `--age` |  | 是 |
| `repos` | `<data>/repos` | 无对应项目/会话且超过 `--age` 的目录 |  | 是 |
| `db-orphans` | `opencode.db` | 孤立 message/part/blob/instruction/event/job 等 | * | 是 |
| `db-projects` | `opencode.db` | 无 session 且创建超过 `--project-age` 的项目 |  | 是 |
| `db-sessions` | `opencode.db` | `--sessions` 超过阈值的会话 |  | 否 |
| `db-archived` | `opencode.db` | `--archived` 超过阈值的归档会话 |  | 否 |
| `db-reasoning` | `opencode.db` | `--strip-reasoning` 旧消息的 reasoning 条目 |  | 否 |
| `db-cache` | `opencode.db` | `models-dev:*` 可再生成缓存 |  | 是 |
| `db-vacuum` | `opencode.db` | 空闲页 + WAL，需要服务停止 |  | 是 |

说明：

- `--age` 支持 `7d`、`36h`、`90m`、`2w`，裸数字按天处理。
- 默认时长：文件 7d、项目 30d；会话删除、归档删除、reasoning 压缩默认关闭。
- `--older-than` 会覆盖上述默认时长；在不使用 `--all` 且没有显式类别参数时，启用所有时间类清理。
- 不使用 `--all` 时，显式类别参数会缩小范围；使用 `--all` 时是加法模式，显式 flags 只会追加类别。
- 会话删除和归档删除都有 **24h 硬下限**；reasoning 压缩有 **1h 硬下限**。
- `--all` 包含 `repos` 和 `db-cache`；如果不想清理这两类，请用显式类别参数而不是 `--all`。
- 删除会话或项目时会顺带执行孤立记录清理，即使没有显式传 `--db-orphans`；这样不会留下悬空记录。
- 自动备份可能和数据库同样大；只清理很小的缓存/孤立记录时可以用 `--no-backup`。

## 数据库垃圾的定义

结构性垃圾（安全、默认清理）：

- `session_message` 的父 session 不存在
- `session_part` 的父 message/session 不存在
- `blob_ref` 的父 session/message/blob 不存在
- `blob` 没有被任何 `blob_ref` 引用
- `instruction_entry` / `instruction_state` 的父 session 不存在
- `instruction_blob` 没有被任何 instruction state/entry 引用
- `event` / `event_sequence` 的 aggregate 既不是 session 也不是 project
- `job` / `schedule` / `session_inbox` 的父 session 不存在
- `permission` / `worktree` / `project_directory` 的父 project 不存在

不会删除：

- `account`、`credential`、`control_account` 等认证数据
- `migration` 历史
- 当前正在运行的会话，及其祖先/后代会话（通过 `OPENCODE_SESSION_ID`）
- 当前工作目录所属的项目
- 最近 24h 内更新过的会话（即使传入更小阈值）

删除会话时：

- 在一个事务内完成
- `foreign_keys=ON`，依赖级联删除消息、part、blob_ref、instruction、schedule、inbox
- 显式清理无外键的 `event_sequence`、`event`、`job`
- 仍存活的子会话会把 `parent_id` / `fork_session_id` 置空
- 结束后执行 `PRAGMA foreign_key_check`

删除项目时：

- 只选择没有任何 session 的项目
- 级联删除 worktree/permission/project_directory
- 同步删除对应的 shell 输出目录

## 安全机制

- `scan` 永远只读；`clean` 和 `sessions delete` 默认只预览，`--apply` 才删除
- 写库前默认用 `VACUUM INTO` 自动备份到 `<data>/backups/autobackup-YYYYMMDD-HHMMSS.db`，保留最近 3 份自动备份；`--no-backup` 可关闭（手动 `backup` 生成的 `opencode-*.db` 不会被自动轮换删除）
- 备份完成后会对副本执行 `PRAGMA quick_check`
- `sessions delete` 始终保护当前会话及其祖先/后代会话；`--force` 只绕过 24h 限制，不能绕过保护
- `sessions delete` 默认保留子会话并置空 `parent_id`；`--with-children` 才递归删除
- 检测到 OpenCode 服务正在运行（读取 `state/service.json` + TCP 探测）时：
  - 默认跳过所有数据库写入，文件清理继续执行
  - `--allow-running` 可强制执行结构性数据库删除或会话删除（不建议）
  - `VACUUM` 永远不会在服务运行时执行，必须 `opencode service stop`
- 文件删除会校验目标位于对应根目录内，防止路径穿越
- 所有数据库删除在一个事务内完成；失败自动回滚
- 备份失败会中止数据库清理（除非显式 `--no-backup`）

服务运行中的典型输出：

```text
提示:
  - OpenCode 服务正在运行，已跳过数据库清理。请退出 OpenCode 后重试，或先执行 `opencode service stop`。
```

退出码：

- `0` 成功
- `1` 致命错误（数据库不存在、备份失败等）
- `2` 部分完成（服务运行导致 DB 跳过、部分文件删除失败等）

## 备份与恢复

自动备份默认位置（手动 `backup` 命令默认使用 `opencode-` 前缀）：

```text
<data>/backups/autobackup-20261101-210340.db
```

恢复步骤：

1. 退出 OpenCode，确保服务已停止：`opencode service stop`
2. 备份或移走当前数据库及 WAL/SHM：
   ```text
   opencode.db
   opencode.db-wal
   opencode.db-shm
   ```
3. 把备份文件复制为 `opencode.db`
4. 重新启动 OpenCode

## JSON 输出

所有命令都支持 `--json`：

```sh
opencode-gc scan --json
opencode-gc clean --json
opencode-gc clean --sessions 30d --json
```

返回值包含 `findings`、`warnings`、`errors`、`dbSize`、`walSize`、`freeSize`、`deletedFiles`、`dbRows`、`backupPath` 等字段。

## 测试

```sh
go test ./...
go vet ./...
```

测试覆盖：

- `age` 单位解析
- 临时文件按 mtime 过滤与删除
- shell/tool-output 引用保护
- 平铺 `tool_output` 文件名保护
- 删除路径必须位于根目录内
- SQLite 临时 fixture 上的孤立记录、项目/会话删除、当前会话保护、reasoning 压缩、备份、VACUUM
- 会话大小统计、排序、过滤、子会话展开
- `sessions delete --force` 绕过 24h 但保留受保护会话
- 服务运行时的写入保护

## 已知限制

- 只支持 OpenCode V2 的 `opencode.db`（包含 `session_v2` 表）；检测到其他结构时只跳过数据库部分并给出提示
- `repos` 的目录结构可能随版本变化；`--all` 会包含它，如果不确定请用显式类别参数避开
- OpenCode 自身已经有 tool-output 的 7 天保留清理；本工具会再次检查引用关系
- 不清理 `<cache>/bin` 下的旧二进制；如需要可后续扩展
- 服务运行时直接写库可能让运行中的 OpenCode 缓存过期，务必优先停止服务
- 如果你正在 OpenCode 会话中，请先退出 TUI，再从普通终端运行本工具；不要在 agent 的 shell 工具里执行 `opencode service stop`，那会终止当前会话

## 目录结构

```text
main.go                 CLI（go-arg）与命令分发
internal/age            7d/36h 等时长解析
internal/paths          OpenCode 路径探测与覆盖
internal/fsgc           tmp/log/shell/tool-output/repos 扫描与删除
internal/dbgc           SQLite 打开、schema 检测、事务、备份、VACUUM、会话统计
internal/gc             扫描/清理/会话分析/定点删除编排
internal/report         文本/JSON 输出（清理报告、会话排行、删除报告）
internal/service        运行中服务探测
```
