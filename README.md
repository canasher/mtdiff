# mtdiff

mtdiff 比较两个 MySQL 端点（实例 / 库 / 表）之间的表数据是否一致。

工作方式类似 Percona `pt-table-checksum`：按主键分块 → 流式逐行读取 → 值规范化 → 哈希 → 对比指纹。与 pt 的关键区别是**哈希在应用层完成**（Go + xxhash，不依赖 MySQL 端 `MD5()` 函数），因此同样适用于 TiDB / PolarDB-X 等 MySQL 兼容层。

任何时刻都不会把整表读进内存；千万行级别只需要百 MB 级内存。

## 构建

```sh
make build        # 产出 bin/mtdiff 单二进制
make test         # 单元测试
make e2e          # 需要 docker：双 MySQL 容器跑全部场景
```

## 用法

```sh
# 比较两个库的全部共有表
mtdiff diff \
  --src root:pass@10.0.0.1:3306/dbA \
  --dst root:pass@10.0.0.2:3306/dbB

# 指定表、并发、忽略列、CI 输出
mtdiff diff \
  --src u:p@h1:3306/dbA --dst u:p@h2:3306/dbB \
  --tables orders,users --parallel 8 \
  --ignore-columns updated_at --json

# 列出两侧表
mtdiff tables --src ... --dst ...

# 把源库数据同步到目的库（先对齐表结构，再把 dst 补齐 / 修正 / 删多余，直到与 src 完全一致）
# 默认 dry-run：只做只读对比，打印同步计划和示例 SQL，零写入
mtdiff sync \
  --src root:pass@10.0.0.1:3306/dbA \
  --dst root:pass@10.0.0.2:3306/dbB \
  --tables orders,users

# 真正执行：交互式确认后只对 dst 写库（CI / 脚本用 --yes 跳过确认）
mtdiff sync --src ... --dst ... --tables orders,users --apply --yes
```

`diff` 子命令可省略：`mtdiff --src ... --dst ...` 直接执行对比（root 命令即 diff）。

长任务（大表扫描、sync 写入）会每 ~10% 向 **stderr** 打一行进度（表名 + 百分比 + 已完成 chunk / 已写行数）；stdout 始终是干净的报告 / JSON，`--json | jq` 不受影响。

连接信息也可以用细粒度 flag（`--src-host/--src-user/--src-password-env/--src-db`…）或 `--config cfg.yaml`：

```yaml
src:
  host: 10.0.0.1
  port: 3306
  user: replica
  password_env: SRC_MYSQL_PWD   # 密码从环境变量读（变量未设置会报错）；${ENV} 是原文替换，值含引号/换行慎用
  database: dbA
dst: { ... }
options:
  tables: [orders, users]
  parallel: 8
  ignore_columns: [updated_at]
```

密码优先级：`password_env` 环境变量（指向未设置的变量会直接报错，而非静默无密码连接）> YAML/DSN 内嵌 > 终端交互输入（非 TTY 时不询问，直接报连接错误）。所有日志与报错中的 DSN 都会打码。`${ENV}` 展开是解析前的原文替换，值含引号/换行/冒号会破坏 YAML，只建议用于密码这类简单值。

### 主要选项

| Flag | 默认 | 说明 |
|---|---|---|
| `--tables` / `--exclude-tables` | 全部共有表 | 逗号分隔 |
| `--parallel` | 4 | 每侧并发块扫描数 |
| `--chunk-size` | 10000 | 目标块行数 |
| `--key` | PK/唯一键 | 显式指定切块键（可复合列）；非唯一键会自动补全排序列 |
| `--where` | | 两侧同用的额外过滤条件 |
| `--ignore-columns` | | 不参与比较的列 |
| `--drill` | off | 有差异时展示示例差异行（有键：CHANGED / MISSING_IN_*；无键：多集合差） |
| `--drill-limit` | 10 | `--drill` 最多展示的示例行数 |
| `--tolerance` | 0（精确） | float/double 量化容差，如 `1e-9` |
| `--snapshot` | off | 每表在一致性快照事务内扫描（防写入抖动，较慢） |
| `--no-trim` | off | 不裁剪字符串尾部空格（默认裁剪，贴近 CHAR 语义） |
| `--fold-case` | off | 字符串忽略大小写（默认字节精确，宁可误报） |
| `--normalize-json` | off | JSON 值做规范化（键排序、数字归一）；默认按原始字节比较 |
| `--allow-tz-swap` | off | 允许两侧 DATETIME/TIMESTAMP 互换，按 UTC 时刻比较 |
| `--strict-types` | off | 要求两侧列类型完全一致 |
| `--secure` | off | 128 位指纹（默认 64 位） |
| `--json` | off | JSON 报告（CI 可 `jq .ok`） |
| `--max-allowed-packet` | 驱动默认 | 大 BLOB 场景调大 |
| `--apply`（仅 sync） | off | 真正执行写入（默认 dry-run，零写入） |
| `--yes`（仅 sync） | off | 跳过 `--apply` 的交互确认（非终端下必须） |
| `--batch-size`（仅 sync） | 1000 | 每条多行 INSERT 的行数上限 |
| `--sample-limit`（仅 sync） | 5 | dry-run 每表展示的示例 SQL 条数（0 = 不展示） |
| `--no-sync-schema`（仅 sync） | off | 跳过同步前的表结构同步（默认先对齐 dst 表结构：dry-run 显示将执行的 DDL，`--apply` 先执行 DDL 再写数据；开启后恢复旧的"结构不一致报错"行为） |

### 退出码

| 码 | 含义 |
|---|---|
| 0 | 全部表一致（sync：无需同步，或已同步且复验一致） |
| 1 | 存在差异（sync dry-run：有差异未应用，含结构漂移待 DDL 对齐；sync apply：同步后仍有差异） |
| 2 | 运行时错误（连接 / schema 不兼容 / introspection / 写入失败；sync 的结构漂移默认自动对齐，见"安全性"，加 `--no-sync-schema` 才在此报错） |
| 3 | 参数错误（sync：非终端下 `--apply` 不带 `--yes`；无键表 + `--where` 无法同步） |

## 安全性

- `diff` / `tables`（含裸 root 命令）**严格只读，硬保证**：每条连接（控制 + 扫描）建立后都会强制只读会话：优先 `SET SESSION read_only=ON`（TiDB 等兼容层直接支持）；MySQL 本体的 `read_only` 是 GLOBAL 变量，此时回退为 `SET SESSION TRANSACTION READ ONLY`（覆盖含 autocommit 在内的全部后续事务）。两者都失败则**拒绝运行**——这两个命令永远不会向被对比的库发起写操作。
- `sync` 是唯一有写操作的命令，但**默认同样零写入**：dry-run 只跑只读对比并打印计划与示例 SQL。只有 `--apply` 且（交互）确认后才会写入，而且**只写目的端（dst）库**——源端（src）连接以及两侧所有扫描 / 控制连接在 sync 里也一律强制只读，做不到就拒绝运行。写连接是单独的一条、确认之后才打开。apply 成功后自动重跑一次对比复验，退出码以复验为准（仍有差异 → 1，不会假报成功）。
- sync 的写入语义：缺的行 INSERT、值变的行 UPDATE、多的行 DELETE；当 dst 行数 > src（或表无可用键）时改为 `TRUNCATE` 后全量重灌——`TRUNCATE` 是 DDL（隐式提交、会重置 `AUTO_INCREMENT`），dst 用户需要 `DROP` 权限。
- sync 默认**先同步表结构**再写数据：dst 结构与 src 漂移（缺列、类型/可空/默认值变化、多余列、缺主键或唯一索引）时，每表一条 `ALTER TABLE` 对齐（补列恢复 src 列序、删多余列、补索引；索引按列序比较、与名字无关；DATETIME↔TIMESTAMP 互换不产生 DDL，仍走 `--allow-tz-swap`）。dry-run 先显示将执行的 DDL（零写入）；`--apply` 确认后先执行 DDL 再全量重灌该表。**`DROP COLUMN` 不可逆**，确认摘要对结构变更的表点名（`structure+resync`）。`--ignore-columns` 里的列同时排除在数据同步与结构同步之外；src 列默认值是非字面表达式（8.0.13+ 的 `(expr)`）时不伪造值，该表直接报错。`--no-sync-schema` 跳过结构前置步骤（恢复旧的"结构不一致 → 报错"行为）。若两侧**列完全相同、仅键漂移**（如 dst 缺主键）：列兼容所以仍可比——diff 回退为 keyless 全表多重集比较（报告附 warn，数据相同仍报一致）；sync 无法按行定位，走 TRUNCATE 全量重灌，配 `--where` 则报参数错（exit 3）。
- 逐行 sync 的作用域是 src 的键范围，外加**显式的范围外清理**：除了 src 最小～最大键范围内的行级操作，还会扫一遍 dst，把键值严格落在范围外的行**逐键删除**（严格 `<`/`>`，等值行归范围 diff 管；复合键与 NULL 安全，字符键按引号字面量比较；不是盲谓词批量删）。无 `--where` 时首轮直接收敛（计数仍失配时升级全量重灌仅作安全网）；带 `--where` 时只删**匹配过滤条件**的范围外行，不匹配的会保留（过滤表不能 TRUNCATE，复验如实报 1，无过滤 diff 可见，需人工处理）。例外：src 零匹配时直接删光 dst 匹配行。
- 尽力而为的护栏（失败仅告警）：`innodb_lock_wait_timeout=5`、`max_execution_time`、`NO_ZERO_DATE` sql_mode；sync 的写连接同样继承前两条。
- 密码只存在于连接内：所有日志、报错、JSON 报告中的 DSN 一律打码（`u:***@h:port/db`）。

## 比较语义（重要）

- **键选择**：优先主键，其次第一个非 NULL 唯一索引，否则 `--key` 显式指定；都没有则走无键路径。
- **无键表**：整表单块、order-independent 四元组指纹（行数 + ΣH + ⊕H + ΣH²）。语义是**多集合相等**：行序无关、允许重复行，但**无法下钻定位到行**。给表加上 `--key` 可升级为块级定位。
- **NULL ≠ 空串 ≠ 0**：三者互不相同（TLV 编码中 NULL 有独立 type tag）。
- **DECIMAL**：字符串十进制规范化后比较（`1.00` ≡ `1`），绝不经过 float64。
- **float/double**：默认**逐位精确**；`--tolerance` 显式开启量化容差。
- **TIMESTAMP**：两侧连接都强制 `loc=UTC`，按 UTC 时刻比较——不同 `system_time_zone` 的实例写同一时刻会判为相等（正确）。
- **DATETIME 是纯墙钟**：与 TIMESTAMP 语义不同，默认互不兼容（需 `--allow-tz-swap`）。
- **字符串**：默认 trim 尾部空格、字节精确（collation 差异会告警；`--fold-case` 显式忽略大小写）。
- **BIT**：按数值比较（`bit(1)` 的 1 ≡ `bit(8)` 的 1）。
- **零日期**（`0000-00-00`）：不支持，扫描会报明确错误；用 `--ignore-columns` 排除或先修数据。
- **移动目标**：默认无事务，扫描窗口内并发写入可能引入抖动；需要强一致时用 `--snapshot`（每表一个一致性快照事务，长事务会持有 read view / 增长 undo log，千万行慎用）。

## 性能

实测（docker 双 MySQL 8.0，聊天形态 1000 万行表：BIGINT 主键 + VARCHAR 正文，localhost；宿主机有其他负载，数字偏保守）：

| 操作 | 耗时 | 吞吐 |
|---|---|---|
| `diff` 10M×2 相同数据，默认 parallel 4 | ~2m09s | ~155k 行/s（双侧合计） |
| `diff` 10M×2，`--parallel 16` | ~48s | ~420k 行/s（扩展 2.7×） |
| sync row-level：补 5M 缺失行 + 全量复验 | ~8m11s | 写入段 ~14k 行/s（单写连接） |
| sync FULL：1M TRUNCATE 重灌 + 复验 | ~1m49s | ~11k 行/s |

按线性外推到 1 亿行（int 主键，算术切分零规划查询）：`diff` ≈ 20 min（parallel 4）/ 8 min（parallel 16）；row-level sync ≈ 45 min 固定开销（pre-pass + 复验）+ 增量写入（~14k 行/s）；FULL 全量重灌 ≈ 2.5~3 h，建议只用于初始化。大表建议 `--parallel 16`、sync 加 `--batch-size 5000~10000`，dst 处于静默期。

## 测试

- 单测：`make test`（normalizer / 切块 / 指纹为重点，覆盖全部陷阱对）
- E2E：`make e2e`（docker 双 MySQL 8.0 实例；"不同时区"由种子在会话级 `SET time_zone`（+08:00 vs -04:00）模拟，而非服务端 `system_time_zone`，见 `e2e/docker-compose.yml` 头注释；127 项断言（89 退出码 + 38 输出内容），覆盖退出码 / JSON 报告 / 并行指纹确定性，含 sync 的 dry-run 零写入 / row-level / TRUNCATE 全量 / 无键表 / `--where` 零匹配删除 / 范围外行删除（int / 复合 / VARCHAR / NULL 键、`--where` 残留、无 TRUNCATE 首轮收敛）/ 结构漂移自动对齐（DDL 展示、零写入、information_schema 内容断言、`--no-sync-schema` 回归）/ 键漂移（一侧有键一侧无键：diff 全表多重集回退、sync 全量重灌、默认结构同步补回主键）/ 参数错路径）
- 验证状态（2026-09）：单测（含 `-race`）与全套 e2e 在 **MySQL 8.0**（docker）上通过；10M 行基准亦在 MySQL 8.0 实测（见"性能"节）；TiDB / PolarDB-X / MySQL 5.7 按兼容性设计支持（应用层哈希、无 MySQL 专属函数），但尚未实测

```sh
# CI（GitHub Actions 参考）
- run: make build test
- run: make e2e
```
