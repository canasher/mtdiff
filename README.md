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
连接串支持 `user:@host` 这种**显式空密码**写法（如 TiDB 默认 root 无密码：`root:@127.0.0.1:4000/dstdb`）：与省略密码段（`user@host`，仍走交互询问）不同，`user:@` 声明服务端就是无密码的，不再询问。

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
| `--allow-unenforced-readonly`（各子命令） | off | 后端无法强制会话只读时（TiDB：`read_only` 仅 GLOBAL 级、`TRANSACTION READ ONLY` 是禁用的空操作）继续运行并逐连接告警；默认拒绝。读连接仍只发 SELECT（见"安全性"） |

### 退出码

| 码 | 含义 |
|---|---|
| 0 | 全部表一致（sync：无需同步，或已同步且复验一致） |
| 1 | 存在差异（sync dry-run：有差异未应用，含结构漂移待 DDL 对齐；sync apply：同步后仍有差异） |
| 2 | 运行时错误（连接 / schema 不兼容 / introspection / 写入失败；sync 的结构漂移默认自动对齐，见"安全性"，加 `--no-sync-schema` 才在此报错） |
| 3 | 参数错误（sync：非终端下 `--apply` 不带 `--yes`；无键表 + `--where` 无法同步） |

## 安全性

- `diff` / `tables`（含裸 root 命令）**严格只读，硬保证**：每条连接（控制 + 扫描）建立后都会强制只读会话：优先 `SET SESSION read_only=ON`；MySQL 本体的 `read_only` 是 GLOBAL 变量（会报 1229），此时回退为 `SET SESSION TRANSACTION READ ONLY`（覆盖含 autocommit 在内的全部后续事务）。两者都失败则**拒绝运行**——这两个命令永远不会向被对比的库发起写操作。
- 例外是**无法强制只读的后端**，典型是 TiDB：`read_only` 同样只有 GLOBAL 级，而 `SET SESSION TRANSACTION READ ONLY` 是禁用的空操作（1235，除非服务端开了 `tidb_enable_noop_functions`），两级护栏都落空。默认行为是**拒绝连接**（不静默放宽）；确需在 TiDB 上跑时用 `--allow-unenforced-readonly` 显式豁免——读连接会逐条告警，且 mtdiff 对这些连接只发 SELECT，接受的风险仅是"服务端无法阻止该账户的其他语句"。PolarDB-X 等兼容层按实际行为走同一路径：能强制就强制，不能就默认拒绝。
- `sync` 是唯一有写操作的命令，但**默认同样零写入**：dry-run 只跑只读对比并打印计划与示例 SQL。只有 `--apply` 且（交互）确认后才会写入，而且**只写目的端（dst）库**——源端（src）连接以及两侧所有扫描 / 控制连接在 sync 里也一律强制只读，做不到就拒绝运行。写连接是单独的一条、确认之后才打开。apply 成功后自动重跑一次对比复验，退出码以复验为准（仍有差异 → 1，不会假报成功）。
- sync 的写入语义：缺的行 INSERT、值变的行 UPDATE、多的行 DELETE——**两侧都有可用键时一律行级**：dst 上多出来的行（一行杂行，或整段超出 src 键范围）按键逐键删除，行数差异（如 1M 对 1M+1）不改变模式。全量重灌（`TRUNCATE` + 整表回灌）只用于：任一侧无可用键、或行级计划在 pre-pass 与写入之间因数据移动失效且无法安全解释（`--where` 下该情形报参数错）。`TRUNCATE` 是 DDL（隐式提交），dst 用户需要 `DROP` 权限。
- sync 默认**先对齐表集与表结构，再写数据**：
  - **表集**：给了 `--tables` 就严格只同步这些表，**永不删除** dst 上的其他表；不给则是整库模式，期望集 = **源侧的 BASE TABLE 集**（dst 库为空 / 只有部分表都能工作，无需强制 `--tables`）：dst 缺的表先 `CREATE TABLE` 再同步数据（列及列序 / 类型 / 可空 / 默认值 / AUTO_INCREMENT 属性 / 主键 / 唯一索引，外加 engine 与**源端当前下一个自增值**作初始值）；dst 独有的表 `DROP TABLE` 掉。`--exclude-tables` 同时把表排除出同步集与删除集。
  - **结构漂移**（缺列、类型/可空/默认值变化、多余列、缺主键或唯一索引）：每表一条 `ALTER TABLE` 对齐（补列恢复 src 列序、删多余列、补索引；索引按列序比较、与名字无关；DATETIME↔TIMESTAMP 互换不产生 DDL，仍走 `--allow-tz-swap`）。**普通（非唯一）索引不在结构同步范围**：差异不产生 DDL，也不影响一致性判定。DDL 之后重读 dst 元数据**重新规划**：修回了可用键（如补回主键）的表回到行级同步，仍无键的才留在全量重灌。dry-run 先显示将执行的 DDL（零写入）；`--apply` 确认后先执行 DDL 再写数据。
  - **破坏性语句单独可见**：`DROP TABLE` / `DROP COLUMN` / `DROP PRIMARY KEY` / `DROP INDEX` 在确认摘要与 dry-run 报告里单列一节（`DESTRUCTIVE`），不藏在"N 条语句"里。`DROP TABLE` 仅整库模式且无 `--where` 时才会被计划；`--where` 是行级过滤，禁止整表删除。
- sync 同时对齐**表状态**（下一个 `AUTO_INCREMENT` 值）：建表时以源端值作初始值；全量重灌 / 结构修复（TRUNCATE 会重置计数器）之后重新对齐；行级同步之后再查；apply 后的复验包含它（状态没收敛同样报 exit 1）。读的是**服务端实际会用的计数器**：显式值（`SHOW CREATE TABLE` 的 `AUTO_INCREMENT=` 子句）与 `max(列)+1` 取较大者——InnoDB 的 `information_schema` 值是估算，计数器第二次变化（第二次 `ALTER`、或 `TRUNCATE`）后不再刷新，不可信。源表无自增列（NULL）时不产生无意义的 `ALTER`；**目的端计数器高于源端时不产生注定无效的 `ALTER`**（计数器只能抬高、不能降低）——如实报告该分歧并退出非零，全量重灌是唯一能重新对齐它的途径。两类能力降级（一次性告警并跳过状态对齐，不是失败）：后端读不到该状态；**预分配 ID 区间的后端**（如 TiDB 的批量分配器：报告的"下一个值"比 `max(列)+1` 高出数万个、显式值低于已分配区间末端会被静默忽略、即使全量重灌也会重新分配新区间）——其报告值不是精确的下一个值，状态对齐在那里不可收敛，跳过而不假装成功；`--where` 表不做状态对齐（表级状态对过滤子集无意义）。
- 若两侧**列完全相同、仅键漂移**（如 dst 缺主键）：列兼容所以仍可比——diff 回退为 keyless 全表多重集比较（报告附 warn，数据相同仍报一致）；默认的结构同步会先补回缺失的键、再按修后的键**重新规划**（键恢复了就行级，不再无条件全量），`--no-sync-schema` 时才走 TRUNCATE 全量重灌，配 `--where` 报参数错（exit 3）。dst 缺表时同理：默认建表，`--no-sync-schema` / `--where` 下报运行错（exit 2）而不是静默跳过。
- 逐行 sync 的作用域是 src 的键范围，外加**显式的范围外清理**：除了 src 最小～最大键范围内的行级操作，还会扫一遍 dst，把键值严格落在范围外的行**逐键删除**（严格 `<`/`>`，等值行归范围 diff 管；复合键与 NULL 安全，字符键按引号字面量比较；不是盲谓词批量删）。无 `--where` 时首轮直接收敛（行级计划因数据移动失效时升级全量重灌仅作安全网）；带 `--where` 时只删**匹配过滤条件**的范围外行，不匹配的会保留（过滤表不能 TRUNCATE，复验如实报 1，无过滤 diff 可见，需人工处理）。例外：src 零匹配时直接删光 dst 匹配行。
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
- E2E：`make e2e`（docker 双 MySQL 8.0 实例；"不同时区"由种子在会话级 `SET time_zone`（+08:00 vs -04:00）模拟，而非服务端 `system_time_zone`，见 `e2e/docker-compose.yml` 头注释；183 项断言（111 退出码 + 72 输出内容），覆盖退出码 / JSON 报告 / 并行指纹确定性，含 sync 的 dry-run 零写入 / row-level（dst 多行只删不灌）/ 无键表全量 / `--where` 零匹配删除 / 范围外行删除（int / 复合 / VARCHAR / NULL 键、`--where` 残留、无 TRUNCATE 首轮收敛）/ 结构漂移自动对齐（DDL 展示、零写入、information_schema 内容断言、修复后重新规划、`--no-sync-schema` 回归）/ 键漂移（一侧有键一侧无键：diff 全表多重集回退、默认结构同步补回主键后回到行级）/ 整库同步（缺表建表、多余表 DROP、表状态 AUTO_INCREMENT 收敛、`--tables` 不删、`--exclude-tables` 豁免、缺表 + `--where`/`--no-sync-schema` 报错）/ 参数错路径）
- 跨后端兼容套件：`make compat-57`（双 MySQL 5.7）与 `make compat-tidb`（MySQL 8.0 源 + 单节点 TiDB 7.5 目标），5.7 侧 89 项 / TiDB 侧 86 项断言，覆盖同一场景集（核心 diff/sync、`--where` 零匹配、范围外删除 int/复合/NULL 键、keyless 全量、结构同步 DDL、键漂移回退、整库同步 建/删/状态——表状态按能力门控：读不到的后端与预分配 ID 区间的后端（TiDB 批量分配器）降级为跳过）；种子 SQL 无递归 CTE、`TIMESTAMP NULL DEFAULT NULL`，同一文件两侧通用。TiDB 侧需 `--allow-unenforced-readonly`（见"安全性"）
- 验证状态（2026-09-03）：单测（含 `-race`）与全套 e2e / compat 在 **MySQL 8.0**（docker）、**MySQL 5.7（5.7.44）**、**TiDB（v7.5.1，单节点）** 上全部通过；10M 行基准亦在 MySQL 8.0 实测（见"性能"节）。实测中修掉的真实不兼容：TiDB 无会话级只读（新增 `--allow-unenforced-readonly`，默认仍拒绝）、`user:@host` 显式空密码语法、结构比较的跨后端归一（整数显示宽度 `int(11)`≡`int`；两侧各自的默认 collation 不算漂移）、TiDB 单条 ALTER 禁止同列双操作（`MODIFY COLUMN id…+ADD PRIMARY KEY(id)` 拆两条 DDL；随列删除的索引不再显式 DROP）、**AUTO_INCREMENT 状态读取**（`information_schema.TABLES.AUTO_INCREMENT` 在计数器二次变更后永久陈旧，改读 `SHOW CREATE TABLE` 子句并与 `max(col)+1` 取较大；计数器不可降低，dst 高于 src 时报告 + 非零退出、全量重灌才能重对齐；预分配 ID 区间的后端——TiDB 批量分配器实测报告值 30002 vs 数据 max+1=11——按行为探测（报告值高出 > 10000）降级为跳过状态比较）、**多余表 DROP 用 `IF EXISTS`**（重跑收敛，不再因表已不存在而失败）

```sh
# CI（GitHub Actions 参考）
- run: make build test
- run: make e2e
```
