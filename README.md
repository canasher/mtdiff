# mtdiff

mtdiff 比较两个 MySQL 端点（实例 / 库 / 表）之间的表数据是否一致。

工作方式类似 Percona `pt-table-checksum`：按主键分块 → 流式逐行读取 → 值规范化 → 哈希 → 对比指纹。与 pt 的关键区别是**哈希在应用层完成**（Go + xxhash，不依赖 MySQL 端 `MD5()` 函数），因此同样适用于 TiDB / PolarDB-X 等 MySQL 兼容层。

扫描侧任何时刻都不把整表读进内存（块级流式），两条删除流（范围内 / 范围外）的内存上界是一个块 / 一个批（10M 行基准实证：数据 ×10，内存不 ×10）；写侧内存正比于差异量 delta——千万行小差异约百 MB 级，完全分歧的大表则正比于差异量，建议 `--where` 按键范围分段逐段同步。

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
  password_env: SRC_MYSQL_PWD   # 密码从环境变量读（变量未设置会报错）；${ENV} 在结构解析后按值替换，字节级、不可注入
  database: dbA
dst: { ... }
options:
  tables: [orders, users]
  parallel: 8
  ignore_columns: [updated_at]
```

密码优先级：`password_env` 环境变量（指向未设置的变量会直接报错，而非静默无密码连接）> YAML/DSN 内嵌 > 终端交互输入（非 TTY 时不询问，直接报连接错误）。所有日志与报错中的 DSN 都会打码。
`${ENV}` 展开发生在**结构解析之后**，且只对取值标量生效：映射的键名永不替换；变量值里含引号、换行、冒号、`#` 等都会**原样字节级**落在同一个值里，不可能借一个变量值注入新的键、新的文档或翻动安全开关；引用一个**未设置**的变量会直接报错（指名变量名），而非静默替换成空串。当一个变量**构成某个类型化字段（int/bool/float）的整个取值**时（如 `parallel: ${P}`、`snapshot: ${S}`、`tolerance: ${T}`），替换结果按该字段的类型解析（`P=8` 得整数 8，`S=true` 得 true）；非类型化字段与部分替换保持字符串，解析不出目标类型会报配置错误（fail closed）。
连接串支持 `user:@host` 这种**显式空密码**写法（如 TiDB 默认 root 无密码：`root:@127.0.0.1:4000/dstdb`）：与省略密码段（`user@host`，仍走交互询问）不同，`user:@` 声明服务端就是无密码的，不再询问。

### 主要选项

| Flag | 默认 | 说明 |
|---|---|---|
| `--tables` / `--exclude-tables` | 全部共有表 | 逗号分隔 |
| `--parallel` | 4 | 每侧并发块扫描数 |
| `--chunk-size` | 10000 | 目标块行数 |
| `--key` | PK/唯一键 | 显式指定切块键（可复合列）；非唯一键会自动补全排序列。指到主键或 NOT NULL 唯一索引会被**识别为唯一**（行级 UPDATE，不按组替换）；指到普通（非唯一）索引时仅无 `--where` 可用（按组替换语义） |
| `--where` | | 两侧同用的额外过滤条件。sync 时要求**两侧的行定位键唯一**（主键或 NOT NULL 唯一索引）：过滤下的行级同步会按键删行，非唯一键会连坐整组被过滤掉的行——不满足报参数错（exit 3），dry-run 与 `--apply` 都在任何写入前拒绝 |
| `--ignore-columns` | | 不参与比较的列 |
| `--drill` | off | 有差异时展示示例差异行（有键：CHANGED / MISSING_IN_*；无键：多集合差） |
| `--drill-limit` | 10 | `--drill` 最多展示的示例行数 |
| `--tolerance` | 0（精确） | float/double 量化容差，如 `1e-9` |
| `--snapshot` | off | 每表**每侧**用一条专用连接 + 一个读事务：COUNT、键极值、切块规划与全部行扫描都在同一快照内完成（防写入抖动，较慢）。一致性是**单侧**的——两侧各自一个时间点，不跨侧对齐 |
| `--no-trim` | off | 不裁剪字符串尾部空格（默认裁剪，贴近 CHAR 语义） |
| `--fold-case` | off | 字符串忽略大小写（默认字节精确，宁可误报） |
| `--normalize-json` | off | JSON 值做规范化（键排序、数字归一、**类型保留**——JSON number 归一后仍是 number，`{"n":1}` 与 `{"n":"1"}` 判不同）；默认按原始字节比较 |
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
| `--allow-structure-truncate`（仅 sync） | off | 原地结构 DDL **失败**时，回退到"先 `TRUNCATE`，再按两侧**重新 introspect 重规划**的 DDL 续做"路径（不重放旧计划）。默认关闭：失败只报错、**dst 数据原样保留**（单条 ALTER 是原子的；多条 DDL **不是**原子的，第 N 条失败时前面的可能已生效，重跑会按当前 schema 重新规划收敛） |
| `--allow-unenforced-readonly`（各子命令） | off | 后端无法强制会话只读时（TiDB：`read_only` 仅 GLOBAL 级、`TRANSACTION READ ONLY` 是禁用的空操作）继续运行并逐连接告警；默认拒绝。读连接仍只发 SELECT（见"安全性"） |

### 退出码

| 码 | 含义 |
|---|---|
| 0 | 全部表一致（sync：无需同步，或已同步且复验一致） |
| 1 | 存在差异（sync dry-run：有差异未应用，含结构漂移待 DDL 对齐；sync apply：同步后仍有差异） |
| 2 | 运行时错误（连接 / schema 不兼容 / introspection / 写入失败；sync 的结构漂移默认自动对齐，见"安全性"，加 `--no-sync-schema` 才在此报错。含**安全拒绝**：源表含生成列、结构 ALTER 失败（数据保留）——均为该表报错、零写入） |
| 3 | 参数错误（sync：非终端下 `--apply` 不带 `--yes`；无键表 + `--where` 无法同步；`--where` + 非唯一 `--key` 无法安全地按键删行） |

## 安全性

- `diff` / `tables`（含裸 root 命令）**严格只读，硬保证**：每条连接（控制 + 扫描）**每次取出使用前**都会钉死会话时区并重放只读会话——先 `SET SESSION time_zone='+00:00'`（建连时已设，此处重验；设置失败的连接**不交出**，TIMESTAMP 比较绝不受服务器默认时区影响），再强制只读：优先 `SET SESSION read_only=ON`；MySQL 本体的 `read_only` 是 GLOBAL 变量（会报 1229），此时回退为 `SET SESSION TRANSACTION READ ONLY`（覆盖含 autocommit 在内的全部后续事务）。两者都失败则**拒绝运行**——这两个命令永远不会向被对比的库发起写操作。策略是**逐次重放**的：连接被 `KILL`、断网或服务端重启后被池替换、或会话被带外重置，替换出来的新物理会话在交出前都会重新拿到完整策略（先策略后查询，绝不存在无防护会话被使用）；sync 的写连接同理，每条新物理写会话都会重放会话护栏（`innodb_lock_wait_timeout` / `max_execution_time` / `NO_ZERO_DATE` 等）。
- 例外是**无法强制只读的后端**，典型是 TiDB：`read_only` 同样只有 GLOBAL 级，而 `SET SESSION TRANSACTION READ ONLY` 是禁用的空操作（1235，除非服务端开了 `tidb_enable_noop_functions`），两级护栏都落空。默认行为是**拒绝连接**（不静默放宽）；确需在 TiDB 上跑时用 `--allow-unenforced-readonly` 显式豁免——读连接会逐条告警，且 mtdiff 对这些连接只发 SELECT，接受的风险仅是"服务端无法阻止该账户的其他语句"。PolarDB-X 等兼容层按实际行为走同一路径：能强制就强制，不能就默认拒绝。
- `sync` 是唯一有写操作的命令，但**默认同样零写入**：dry-run 只跑只读对比并打印计划与示例 SQL。只有 `--apply` 且（交互）确认后才会写入，而且**只写目的端（dst）库**——源端（src）连接以及两侧所有扫描 / 控制连接在 sync 里也一律强制只读，做不到就拒绝运行。写连接是单独的一条、确认之后才打开。apply 成功后自动重跑一次对比复验，退出码以复验为准（仍有差异 → 1，不会假报成功）。
- sync 的写入语义：缺的行 INSERT、值变的行 UPDATE、多的行 DELETE——**两侧存在兼容且 range-addressable 的共享键时一律行级**（该键不仅唯一定位行，还要能证明源侧产生的范围边界在目的侧有相同的排序/比较语义，例如普通 INT/UINT 主键、或 collation 一致的字符串键）：dst 上多出来的行（一行杂行，或整段超出 src 键范围）按键逐键删除，行数差异（如 1M 对 1M+1）不改变模式。全量重灌（`TRUNCATE` + 整表回灌）用于**无法按共享键安全做范围寻址**的情况，包括：任一侧无可用键、两侧没有兼容的共享键、键存在但排序语义无法证明安全、ENUM/SET 键（当前实现明确非 range-addressable，即便两侧定义相同）、或行级计划在 pre-pass 与写入之间因数据移动失效且无法安全解释（`--where` 下该情形报参数错）。`TRUNCATE` 是 DDL（隐式提交），dst 用户需要 `DROP` 权限。
- sync 默认**先对齐表集与表结构，再写数据**：
  - **表集**：给了 `--tables` 就严格只同步这些表，**永不删除** dst 上的其他表；不给则是整库模式，期望集 = **源侧的 BASE TABLE 集**（dst 库为空 / 只有部分表都能工作，无需强制 `--tables`）：dst 缺的表先 `CREATE TABLE` 再同步数据（列及列序 / 类型 / 可空 / 默认值 / AUTO_INCREMENT 属性 / 主键 / 唯一索引，外加 engine 与**源端当前下一个自增值**作初始值）；dst 独有的表 `DROP TABLE` 掉。`--exclude-tables` 同时把表排除出同步集与删除集。
  - **结构漂移**（缺列、类型/可空/默认值变化、多余列、缺主键或唯一索引）：每表一条 `ALTER TABLE` 对齐（补列恢复 src 列序、删多余列、补索引；索引按列序比较、与名字无关；DATETIME↔TIMESTAMP 互换不产生 DDL，仍走 `--allow-tz-swap`）。**普通（非唯一）索引不在结构同步范围**：差异不产生 DDL，也不影响一致性判定。
  - **结构修复默认原地 `ALTER`，不再先 `TRUNCATE`**（单条 InnoDB ALTER 是原子的，失败即回滚、表原样）：DDL 之后重读 dst 元数据、重新比较、按修后的结构**重新规划**（修复后得到兼容且 range-addressable 的共享键就回到行级同步；若虽补回了主键/唯一键、但该键仍非 range-addressable（例如 ENUM/SET，即便两侧定义一致）则仍走全量重灌，仍无任何可用键的同样全量重灌，且只在确认全量重灌前才 `TRUNCATE`）。原地 DDL **失败**（如 dst 现有数据装不进 src 的列类型）时默认**只报错、dst 数据原样保留**（exit 2；单条 ALTER 失败是零写入，但多条 DDL 不是原子的——报错会说明"前面的语句可能已生效"，重跑按当前 schema 重新规划收敛剩余差异）；`--allow-structure-truncate` 显式回退到"先 `TRUNCATE`，再按两侧重新 introspect **重规划** DDL（已生效的语句不会重放）"路径。
  - **生成列安全拒绝**：源表含生成列（`GENERATED ALWAYS AS`，VIRTUAL/STORED）时结构同步**不尝试复现**生成表达式（跨后端不可靠），该表报运行错（exit 2）——对齐 schema 或 `--no-sync-schema`。数据路径上生成列**参与比较、永不写入**（`INSERT`/`UPDATE` 不含它，由 dst 自行推导）。结构比较**包含生成表达式本身**（只归一化空白/外层括号，其余严格比较）：两侧表达式不同、存储方式不同（VIRTUAL↔STORED）、或一侧读不到表达式（后端不暴露）都算漂移并安全拒绝，绝不假绿；两侧都读不到时退回 Generated/存储方式比较。
  - **破坏性语句单独可见**：`DROP TABLE` / `DROP COLUMN` / `DROP PRIMARY KEY` / `DROP INDEX` 在确认摘要与 dry-run 报告里单列一节（`DESTRUCTIVE`），不藏在"N 条语句"里。`DROP TABLE` 仅整库模式且无 `--where` 时才会被计划；`--where` 是行级过滤，禁止整表删除。
- sync 同时对齐**表状态**（下一个 `AUTO_INCREMENT` 值）：建表时以源端值作初始值；全量重灌 / 结构修复（TRUNCATE 会重置计数器）之后重新对齐；行级同步之后再查；apply 后的复验包含它（状态没收敛同样报 exit 1）。读的是**服务端实际会用的计数器**：显式值（`SHOW CREATE TABLE` 的 `AUTO_INCREMENT=` 子句）与 `max(列)+1` 取较大者——InnoDB 的 `information_schema` 值是估算，计数器第二次变化（第二次 `ALTER`、或 `TRUNCATE`）后不再刷新，不可信。源表无自增列（NULL）时不产生无意义的 `ALTER`；**目的端计数器高于源端时不产生注定无效的 `ALTER`**（计数器只能抬高、不能降低）——如实报告该分歧并退出非零，全量重灌是唯一能重新对齐它的途径。两类能力降级（一次性告警并跳过状态对齐，不是失败）：后端读不到该状态；**预分配 ID 区间的后端**（如 TiDB 的批量分配器：报告的"下一个值"比 `max(列)+1` 高出数万个、显式值低于已分配区间末端会被静默忽略、即使全量重灌也会重新分配新区间）——其报告值不是精确的下一个值，状态对齐在那里不可收敛，跳过而不假装成功；`--where` 表不做状态对齐（表级状态对过滤子集无意义）。
- 若两侧**列完全相同、仅键漂移**（如 dst 缺主键）：列兼容所以仍可比——diff 回退为 keyless 全表多重集比较（报告附 warn，数据相同仍报一致）；默认的结构同步会先补回缺失的键、再按修后的键**重新规划**（键恢复了且可 range-addressable 就行级；若恢复的是 ENUM/SET 键则仍走全量重灌，不再无条件全量），`--no-sync-schema` 时才走 TRUNCATE 全量重灌，配 `--where` 报参数错（exit 3）。dst 缺表时同理：默认建表，`--no-sync-schema` / `--where` 下报运行错（exit 2）而不是静默跳过。
- 逐行 sync 的作用域是 src 的键范围，外加**显式的范围外清理**：除了 src 最小～最大键范围内的行级操作，还会扫一遍 dst，把键值严格落在范围外的行**逐键删除**（严格 `<`/`>`，等值行归范围 diff 管；复合键与 NULL 安全，字符键按引号字面量比较；不是盲谓词批量删）。无 `--where` 时首轮直接收敛（行级计划因数据移动失效时升级全量重灌仅作安全网）；带 `--where` 时只删**匹配过滤条件**的范围外行，不匹配的会保留（过滤表不能 TRUNCATE，复验如实报 1，无过滤 diff 可见，需人工处理）。例外：src 零匹配时直接删光 dst 匹配行。
- 尽力而为的护栏（失败仅告警）：`innodb_lock_wait_timeout=5`、`max_execution_time`、`NO_ZERO_DATE` sql_mode；sync 的写连接同样继承前两条。
- 密码只存在于连接内：所有日志、报错、JSON 报告中的 DSN 一律打码（`u:***@h:port/db`）。

## 比较语义（重要）

- **键选择**：优先主键，其次第一个非 NULL 唯一索引，否则 `--key` 显式指定；都没有则走无键路径。
- **无键表**：整表单块、order-independent 四元组指纹（行数 + ΣH + ⊕H + ΣH²）。语义是**多集合相等**：行序无关、允许重复行，但**无法下钻定位到行**。给表加上 `--key` 可升级为块级定位。
- **NULL ≠ 空串 ≠ 0**：三者互不相同（TLV 编码中 NULL 有独立 type tag）。
- **DECIMAL**：字符串十进制规范化后比较（`1.00` ≡ `1`），绝不经过 float64。
- **float/double**：默认**逐位精确**；`--tolerance` 显式开启量化容差。
- **TIMESTAMP**：所有会话钉死 `time_zone='+00:00'`（建连时设置、每次 checkout 重验；设置失败的连接不交出），按绝对时刻比较——不同默认时区的实例写同一时刻判相等（正确）；两个**不同**时刻即使在不同默认时区下显示成同一个字符串（如 `system_time_zone` 一边 +00:00 一边 +08:00 都显示 `08:00:00`）也判不同。
- **DATETIME 是纯墙钟**：与 TIMESTAMP 语义不同，默认互不兼容（需 `--allow-tz-swap`）。
- **字符串**：默认 trim 尾部空格、字节精确（collation 差异会告警；`--fold-case` 显式忽略大小写）。
- **BIT**：按数值比较（`bit(1)` 的 1 ≡ `bit(8)` 的 1）。
- **零日期**（`0000-00-00`）：不支持，扫描会报明确错误；用 `--ignore-columns` 排除或先修数据。
- **移动目标**：默认无事务，扫描窗口内并发写入可能引入抖动；需要强一致时用 `--snapshot`（**每侧**一条专用连接 + 一个读事务，COUNT/键极值/切块规划/行扫描/下钻都在同一快照内完成；一致性是单侧的——两侧各读各的时间点，不跨侧对齐。长事务会持有 read view / 增长 undo log，千万行慎用。sync 的 pre-pass 继承 `--snapshot`，apply 阶段的复扫刻意保持新鲜——升级逻辑靠它兜住快照期间的新写入）。

## 性能

实测（docker 双 MySQL 8.0，聊天形态 1000 万行表：BIGINT 主键 + VARCHAR 正文，localhost；宿主机有其他负载，数字偏保守）：

| 操作 | 耗时 | 吞吐 |
|---|---|---|
| `diff` 10M×2 相同数据，默认 parallel 4 | ~2m09s | ~155k 行/s（双侧合计） |
| `diff` 10M×2，`--parallel 16` | ~48s | ~420k 行/s（扩展 2.7×） |
| sync row-level：补 5M 缺失行 + 全量复验 | ~8m11s | 写入段 ~14k 行/s（单写连接） |
| sync FULL：1M TRUNCATE 重灌 + 复验 | ~1m49s | ~11k 行/s |

按线性外推到 1 亿行（int 主键，算术切分零规划查询）：`diff` ≈ 20 min（parallel 4）/ 8 min（parallel 16）；row-level sync ≈ 45 min 固定开销（pre-pass + 复验）+ 增量写入（~14k 行/s）；FULL 全量重灌 ≈ 2.5~3 h，建议只用于初始化。大表建议 `--parallel 16`、sync 加 `--batch-size 5000~10000`，dst 处于静默期。内存边界：扫描与两条删除流（范围内块级 / 范围外 keyset 分页）上界 O(chunk)/O(batch)（1M/10M 合成键基准：峰值缓冲 = 一个 chunk，与表规模无关）；行级 ops 本身正比于差异量 delta——完全分歧的 1 亿行表会保留全部行的 ops，此类表建议 `--where` 按键范围分段、逐段同步（每段 dry-run 先看计划）。

## 测试

- 单测：`make test`（normalizer / 切块 / 指纹为重点，覆盖全部陷阱对）
- E2E：`make e2e`（docker 双 MySQL 8.0 实例；"不同时区"由种子在会话级 `SET time_zone`（+08:00 vs -04:00）模拟，而非服务端 `system_time_zone`，见 `e2e/docker-compose.yml` 头注释；354 项断言（221 退出码 + 133 输出内容），覆盖退出码 / JSON 报告 / 并行指纹确定性，含 sync 的 dry-run 零写入 / row-level（dst 多行只删不灌）/ 无键表全量 / `--where` 零匹配删除 / 范围外行删除（int / 复合 / VARCHAR / NULL 键、`--where` 残留、无 TRUNCATE 首轮收敛）/ 结构漂移自动对齐（DDL 展示、零写入、information_schema 内容断言、修复后重新规划、`--no-sync-schema` 回归）/ 键漂移（一侧有键一侧无键：diff 全表多重集回退、默认结构同步补回主键后回到行级）/ 整库同步（缺表建表、多余表 DROP、表状态 AUTO_INCREMENT 收敛、`--tables` 不删、`--exclude-tables` 豁免、缺表 + `--where`/`--no-sync-schema` 报错）/ 参数错路径，以及数据安全回归：**显式 `--key` 唯一性**（指到 NOT NULL UNIQUE 列识别为唯一 → 行级 UPDATE；键值本身变化 → delete+insert；`--where` + 非唯一键 → exit 3 且在 dry-run 与 `--apply` 下均先于任何写入拒绝、dst 零写入）/ **唯一值互换默认拒绝**（swap/环/holder：默认 exit 2 明确拒绝——FK/触发器副作用不可证明安全，报错指名 `--allow-row-rewrite`；加 flag 才允许重写；跨块互换默认拒绝、flag 下升级全量重灌；FK `ON DELETE CASCADE` 表实证默认路径从不级联删子行）/ **字符串键读侧参数化**（VARCHAR 主键对抗值：反斜杠 / 引号 / 中文 / 换行，小 chunk 强制键界落在值内部，dst 开 `NO_BACKSLASH_ESCAPES`，diff/sync/范围外删除全过，HEX 全表字节级一致）/ **生成列**（只比较、永不写入；两侧表达式比较：归一（仅 trim + 剥最外层括号）后不同 / VIRTUAL↔STORED 不一致 / 读不到 → 结构同步安全拒绝；dst 丢列 → 同样拒绝，dry-run 与 `--apply` 均 exit 2 且数据原样保留）/ **结构 DDL 部分失败**（单条 ALTER 失败原子回滚；多条 DDL 中途失败 → 默认停下报错（数据未清）且重跑从当前 schema 重新规划、不重放旧 DDL；`--allow-structure-truncate` 才 TRUNCATE + 重规划剩余 DDL 重灌）/ **写路径转义**（dst 全局 `NO_BACKSLASH_ESCAPES` 下反斜杠 / 引号 / 中文值经参数化写入字节级一致，HEX 全表比对）/ **`--snapshot` 并发写入存活** / **`--snapshot` 严格性**（后端全局 READ-COMMITTED 下仍走 REPEATABLE READ + CONSISTENT SNAPSHOT，不支持则显式拒绝、不静默降级）/ **只读会话策略探针**（并行扫描期间 general_log 实证：参数化读按 Prepare/Execute 命令量识别 scan worker、经典读按语句文本，全部读连接带只读初始化、无一漏设）/ **唯一约束元组化**（复合 UNIQUE 成员列不各自唯一；可空唯一列 NULL 不占槽；跨块/跨约束不互撞）/ **超宽表占位符上限**（120 列：批次自动收缩到 ≤60000 占位符、单行超限显式报错）/ **稀疏 `--where` 采样**（切分点按过滤后的行）/ **BIGINT 极值切块**（MinInt64~MaxInt64 跨度 → 采样切分）/ **`--sample-limit 0` 不出示例 SQL** / **连接替换重初始化**（KILL 物理连接后 scan/control/writer 的替换会话交出前重新拿完整只读策略与会话护栏，真实库 KILL→重连→guardrail 重建实测）/ **特殊字符凭据闭环**（`p@ss:word/a?b` 经 YAML `${ENV}` 与 shorthand DSN 两条入口到 MySQL 认证全字节级一致，输出打码）/ **>64KiB 大载荷碰撞**（两个 LONGBLOB 列 65536/65537 字节 + 1MiB 同值行：旧 16 位 canonical 长度按 mod 65536 截断、不同行可确定性碰撞，默认与 `--secure` 哈希模式均须报出分歧）/ **跨时区 TIMESTAMP 假相同**（src 全局 +00:00 / dst 全局 +08:00：两个**不同**时刻在两侧默认时区下都显示 "08:00:00"，diff 必须报不同；同一时刻跨不同默认时区仍判等；sync `--apply` 后两侧 `UNIX_TIMESTAMP` 相等——收敛的是时刻不是显示串）/ **跨 collation 字符串键安全**（`utf8mb4_bin` vs `utf8mb4_general_ci` 主键：diff 回退无键整表多集合 + 明确告警；`sync --no-sync-schema` 拒绝行级寻址（dry-run 与 `--apply` 均 exit 2、零写入、FK 级联子表行数不变）；`--where` 组合为参数错误 exit 3；默认结构同步对齐键 collation 后回到行级）/ **大有限浮点 + tolerance**（1e10 vs 2e10 在 `--tolerance 1e-9` 下必须报不同——旧量化器在 |v/tol|>9.2e18 饱和成 ±Inf 造出假相同）/ **真实 MySQL TIME**（TIME(6) 含负值 / 小数 / ±838:59:59 边界：驱动实际类型（文本 `[]byte` / 二进制 string）按 `[-]HHH:MM:SS[.ffffff]` 解析；相同判等、值变化报不同、sync `--apply` 收敛并以 `TIMESTAMPDIFF` 断言收敛值）/ **跨家族数值相等**（INT 1 vs BIGINT UNSIGNED 1：非严格判等 + 明确公告，`--strict-types` 报 schema 错；驱动对 64 位无符号值的三种形态 `uint64` / `int64` / 超 MaxInt64 的十进制字符串同形且与 INT 家族等义）/ **normalize-json 精确数值**（`9007199254740992` vs `…93` 经 `--normalize-json` 必须报不同——MySQL JSON 精确存储、旧 float64 round-trip 会塌缩；`1`/`1.0`/`1.00`/`1e0` 判等；大指数不展开成巨型串）/ **单调用连接恢复 + 时区钉死**（go hook 真实库：KILL scan/control/writer 物理连接后**恰好一次** AcquireScan/AcquireControl/Conn 即恢复（无手工重试），新会话策略完整（只读 / 护栏 / 时区）；src scan / src control / dst writer 三池逐池断言 `@@SESSION.time_zone='+00:00'`）/ **ENUM·SET 键 fail closed**（ENUM/SET 主键：即便两侧**定义相同**也非 range-addressable——ORDER BY 按成员定义序、WHERE 按成员名 collation 序，键区间寻址不安全→diff 走整表多重集、sync 走全量重灌；`--no-sync-schema` sync 拒行级寻址（exit 2 零写入）、`--where` 组合 exit 3；成员定义漂移时默认结构同步**先把目的端定义对齐源端，但定义一致后仍走 FULL resync（TRUNCATE + reload），不恢复 range 行级**）/ **JSON 数值类型保真**（`{"n":1}` vs `{"n":"1"}` 经 `--normalize-json` 必报不同——旧规范化把 JSON 数值塌缩成加引号字符串的假相同；`{"n":1}` vs `{"n":1.0}` 判等）/ **跨家族数值大值相等**（BIGINT 1000000 vs DOUBLE 1e+06：非严格判等 + 公告、`--strict-types` 报 schema 错；DECIMAL(20,10) 0.0000100000 vs DOUBLE 0.00001 tolerance 下判等——round-8 共用 tag 但 payload 各按家族编码，同值跨家族大值仍假 DIFFERENT））
- 跨后端兼容套件：`make compat-57`（双 MySQL 5.7）与 `make compat-tidb`（MySQL 8.0 源 + 单节点 TiDB 7.5 目标），5.7 侧 89 项 / TiDB 侧 86 项断言，覆盖同一场景集（核心 diff/sync、`--where` 零匹配、范围外删除 int/复合/NULL 键、keyless 全量、结构同步 DDL、键漂移回退、整库同步 建/删/状态——表状态按能力门控：读不到的后端与预分配 ID 区间的后端（TiDB 批量分配器）降级为跳过）；种子 SQL 无递归 CTE、`TIMESTAMP NULL DEFAULT NULL`，同一文件两侧通用。TiDB 侧需 `--allow-unenforced-readonly`（见"安全性"）
- 验证状态（2026-09-07，含第九轮 review 修复轮复跑）：单测（含 `-race`）与全套 e2e / compat 在 **MySQL 8.0**（docker）、**MySQL 5.7（5.7.44）**、**TiDB（v7.5.1，单节点）** 上全部通过（主 e2e 354 / 5.7 89 / TiDB 86+1 skip，零失败）；10M 行基准亦在 MySQL 8.0 实测（见"性能"节）。第九轮 review 修复轮落地的行为变化：**normalize-json 数值类型保真**（canonical tree 里 JSON 数值保持 `json.Number` 而非塌缩成加引号字符串——`{"n":1}` 与 `{"n":"1"}` 规范化后**不同**、`{"n":1}` 与 `{"n":1.0}` 判等，数值仍走精确十进制不经 float64）/ **ENUM·SET 键 fail closed**（键序门从 blanket-allow 非字符串改为逐家族显式门：ENUM/SET 键分量要求成员定义**完全相等**（同成员同序、解析失败拒收），diff 侧不 range-addressable→整表多重集回退 + 告警，sync 侧 `DecidePlan` 拒行级寻址→全量重灌（`--where` 组合报参数错）；新增 `conn.KeyRangeChunkable`（跨侧，含单侧 range-addressable 判定）与 `KeyRangeAddressable`（单侧，供无对端的全量重灌路径），ENUM/SET 键即便两侧定义相同也判非 range-addressable——ORDER BY 按成员定义序、WHERE 按成员名 collation 序，`[min..max]` 键区间在 WHERE 里是空集）/ **共享数值 payload 统一**（round-8 共用 tag 但 payload 各按家族编码：INT 1000000=`"1000000"`、DOUBLE 1000000=`"1e+06"`，同值跨家族仍假 DIFFERENT；现所有数值家族走同一 canonical 十进制语法 `canonicalNumber`——INT/UINT/DECIMAL 精确不经 ParseFloat、FLOAT/DOUBLE 量化后同形，同值跨家族同形、不同值永不同形）/ **CI unit 增加独立 race 步骤**（GitHub Actions unit 任务在 Test 之后加 `go test -race ./...` 独立步骤、保留原 `go test ./...`——本地 `make race` 对齐，竞争与逻辑错分步定位，CI 仍 4 任务）。第八轮 review 修复轮落地的行为变化：**canonical 编码长度改 8 字节**（值 = tag(1B)+uint64 长度(8B)+payload：旧 2 字节长度在载荷 ≥65536 时按 mod 65536 截断，两行不同数据可**确定性**碰撞成同一 canonical 字节——不同逻辑行同指纹的假相同；任意长度注入单射，>64KiB BLOB/TEXT/JSON 实测覆盖）、**所有会话钉死 `time_zone='+00:00'`**（建连时经驱动 Params 设置 + 每次 checkout 重验，设置失败的连接不交出——TIMESTAMP 比较不再落进服务器默认时区：两个不同时刻即使两侧默认时区下显示成同一个字符串也判不同；写连接的 TIMESTAMP 字面量按 UTC 解释）、**键序/排序规则兼容性门**（两侧键逐分量同列名同家族、字符串键要求有效 collation 一致；不一致时 diff 回退无键整表多集合比较 + 告警，sync 拒绝行级寻址——源键界永不用于排序语义不同的目的侧；`--where` 组合报参数错误；默认结构同步对齐 collation 后恢复行级）、**有限 tolerance 不再饱和**（量化器删掉 |v/tol|>9.2e18 → ±Inf 的饱和分支：合法有限值 + 合法 tolerance 永不产出人造 NaN/±Inf，1e10 与 2e10 在 `--tolerance 1e-9` 下正确判不同）、**TIME 按驱动实际类型解析**（`[-]HHH:MM:SS[.ffffff]` 文本语法，负值 / 小数 / ±838:59:59 边界全覆盖）、**数值家族共用 canonical tag**（INT/UINT/DECIMAL/FLOAT/DOUBLE 同值跨家族判等——兼容层放行的组合值层不再全假 DIFFERENT；`--strict-types` 仍按原始类型拒收；BIGINT UNSIGNED 的三种驱动形态 `uint64`/`int64`/超 MaxInt64 十进制字符串同形）、**normalize-json 精确数值**（不经 float64：`9007199254740992` vs `…93` 判不同、`1`/`1.0`/`1.00`/`1e0` 判等、大指数紧凑渲染不展开）、**读会话单调用有界恢复**（applySession 不再忽略护栏结果；scan/control/writer 的替换走共享 checkoutChecked：一次调用内至多 3 次有界替换——匹配驱动对 KILLed 空闲 socket 的两阶段 dead 报告——只交出活的 + 策略完整的会话，生产路径不写手工重试）。第七轮 review 修复轮落地的行为变化：**非有限 tolerance 拒绝**（Validate 拒 NaN/±Inf/负值 + normalizer 第二道防线显式拒规范化——`tolerance: .inf` 不再把所有浮点值规范化成同一渲染的假 CONVERGED）、**control 单池自死锁修复**（swap 先释放死 checkout 再取新连接：KILLed 活跃会话的查询在 watchdog 内透明恢复，先策略后查询）、**writer 事务前 dead session 恢复**（Conn 交出前处理两阶段 dead 报告，至多 3 次有界替换；**禁止事务自动重试**——BeginTx 成功后网络错误使 commit 结果 UNKNOWN，重放 = 重复写，fail fast）。第六轮 review 修复轮落地的行为变化：**连接生命周期策略重放**（control 池只读策略与 writer 会话护栏在**每次 checkout 使用前**重放——KILL/断网/重启被替换、或带外重置的新物理会话在交出前重新拿到完整策略，先策略后查询，绝不存在无防护会话被使用；元数据/规划查询全部经 policy-applied control 通道，不再暴露裸池）、**DSN 由驱动自身 formatter 组装**（`mysql.Config` + `FormatDSN`，弃手工拼装：特殊字符密码 `:` `@` `/` `?` `#` 与含 `/` 库名字节级 round-trip，凭据原样到达服务端）、**typed `${ENV}` 标量**（类型化字段的整个取值恰为一个占位符时按字段 Go 类型解析——`parallel: ${P}` 恢复兼容，值解析不出类型报错指名；结构注入防御不变）、**计划块序确定化**（差异块列表按块 ID 升序输出，计划层再排一次作安全兜底——跨块唯一 holder 的"安全"裁决依赖块按键序顺序 apply，随机块序会让唯一索引 1062）。第三轮 review 修复轮（P0 作用域门 / P1 GenExpr·dstDeletes·OOR 流式 / P2 批量 DELETE）落地的行为变化：**确认后破坏性作用域不可扩大**（preflight 在确认提示前记录确认计划的破坏性作用域；apply 重新规划后若需 TRUNCATE 或比确认计划更多的重写组 → 停表、零破坏性写入、exit 2、提示重跑确认；ROWLEVEL 永不在同一次 apply 内升级 FULL/TRUNCATE；`--allow-row-rewrite` 只授权 DELETE+INSERT 重写，从不授权全量重灌）、**破坏性重写在确认前可见**（确认提示前完整 preflight）、**生成表达式读不到 → 保守拒绝**（任一侧读不到即拒绝，不再有 `""==""` 假绿）、**两条删除流式化**（范围内：块级排序 + 批量 DELETE；范围外：keyset 分页逐页删；客户端越界集合删除、改服务端 flag 列——内存上界 = 一个 chunk，1M/10M 基准实证数据 ×10 内存不 ×10）、**批量参数化 DELETE**（`IN` / `IS NULL` / 复合键 OR-of-AND 全绑定，占位符上限 60000，删除不再逐行 RTT）、**dry-run 不扫全键集**（COUNT + 有界样本 + `STREAM DELETE` 计划行）。第二轮 review 修复轮（P0-1/2、P1-1~6、P2-1~4）落地的行为变化：**读侧谓词全参数化**（chunk 键界 / 范围外严格比较 / 采样切分点 / 跨块持有者查询全部绑定参数，`--where` 保持原始 SQL；字符串键对抗值字节级一致）、**唯一值互换/环/holder 默认明确拒绝**（FK/触发器副作用不可证明安全；`--allow-row-rewrite` 显式开启，dry-run 单独标注 DESTRUCTIVE ROW REWRITE）、**生成列表达式比较**（归一后不同 / VIRTUAL↔STORED / 读不到 → 结构同步安全拒绝）、**结构 DDL 部分失败不重放旧计划**（默认停下 + 数据未清提示；flag 路径 TRUNCATE 后两侧重新 introspect 只执行剩余 DDL）、**跨块互换检测 O(chunk+delta)**（只跟踪实际写入的元组，上限 10000 超出即拒绝/升级全量；键序不可证明的家族——ci 排序 / DECIMAL / TIME / ENUM / JSON——明确拒绝而非猜）、**`--snapshot` 严格**（不支持 CONSISTENT SNAPSHOT 显式拒绝，不静默降级）、**每条 worker 固定一条 scan 连接**（致命错误重取重试一次）、**占位符上限 60000**（超宽表批次自动收缩，单行超限显式报错）、**稀疏 `--where` 按过滤后行切分**。第一版数据安全修复轮（P0-1/2/3、P1-1~5、P1-BIGINT、P2-1~3）落地的行为变化：真实写语句全参数化（展示 SQL 与执行 SQL 分离，`NO_BACKSLASH_ESCAPES` 下字节级一致）、`--where` + 非唯一键在写入前拒绝（exit 3）、生成列只比较不写入且结构同步安全拒绝、结构漂移默认原地 ALTER 不再先 TRUNCATE（失败保留数据）、每条实际 scan 连接都强制只读会话（e2e 用 general_log 探针实测）、键漂移（两侧键不同/顺序不同）回退全量重灌、唯一值互换块内转 delete+insert、BIGINT 极值跨度 overflow-safe 切块、AUTO_INCREMENT 能力判定去全局化（逐侧 + 逐表）。实测中修掉的真实不兼容：TiDB 无会话级只读（新增 `--allow-unenforced-readonly`，默认仍拒绝）、`user:@host` 显式空密码语法、结构比较的跨后端归一（整数显示宽度 `int(11)`≡`int`；两侧各自的默认 collation 不算漂移）、TiDB 单条 ALTER 禁止同列双操作（`MODIFY COLUMN id…+ADD PRIMARY KEY(id)` 拆两条 DDL；随列删除的索引不再显式 DROP）、**AUTO_INCREMENT 状态读取**（`information_schema.TABLES.AUTO_INCREMENT` 在计数器二次变更后永久陈旧，改读 `SHOW CREATE TABLE` 子句并与 `max(col)+1` 取较大；计数器不可降低，dst 高于 src 时报告 + 非零退出、全量重灌才能重对齐；预分配 ID 区间的后端——TiDB 批量分配器实测报告值 30002 vs 数据 max+1=11——按行为探测（报告值高出 > 10000）降级为跳过状态比较）、**多余表 DROP 用 `IF EXISTS`**（重跑收敛，不再因表已不存在而失败）

```sh
# CI：.github/workflows/ci.yml 有四个任务
#   unit            make lint / make test / go test -race ./...（独立 Race 步骤）/ make-push 回归 / Windows 交叉编译检查
#   mysql8-e2e      make build + bash e2e/run_e2e.sh（自带 docker compose 容器）
#   mysql57-compat  make build + bash e2e/compat/run_compat.sh 57
#   tidb-compat     make build + bash e2e/compat/run_compat.sh tidb
# 每个容器化任务自建自毁容器（compose 项目名互不相同，可并行）。
```
