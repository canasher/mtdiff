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
```

`diff` 子命令可省略：`mtdiff --src ... --dst ...` 直接执行对比（root 命令即 diff）。

连接信息也可以用细粒度 flag（`--src-host/--src-user/--src-password-env/--src-db`…）或 `--config cfg.yaml`：

```yaml
src:
  host: 10.0.0.1
  port: 3306
  user: replica
  password_env: SRC_MYSQL_PWD   # 密码从环境变量读，支持 ${ENV} 展开
  database: dbA
dst: { ... }
options:
  tables: [orders, users]
  parallel: 8
  ignore_columns: [updated_at]
```

密码优先级：`password_env` 环境变量 > YAML/DSN 内嵌 > 终端交互输入（非 TTY 时不询问，直接报连接错误）。所有日志与报错中的 DSN 都会打码。

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

### 退出码

| 码 | 含义 |
|---|---|
| 0 | 全部表一致 |
| 1 | 存在差异 |
| 2 | 运行时错误（连接 / schema 不兼容 / introspection） |
| 3 | 参数错误 |

## 安全性

- 每条连接（控制 + 扫描）建立后都会强制只读会话：优先 `SET SESSION read_only=ON`（TiDB 等兼容层直接支持）；MySQL 本体的 `read_only` 是 GLOBAL 变量，此时回退为 `SET SESSION TRANSACTION READ ONLY`（覆盖含 autocommit 在内的全部后续事务）。两者都失败则**拒绝运行**——mtdiff 永远不会向被对比的库发起写操作。
- 尽力而为的护栏（失败仅告警）：`innodb_lock_wait_timeout=5`、`max_execution_time`、`NO_ZERO_DATE` sql_mode。
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

## 测试

- 单测：`make test`（normalizer / 切块 / 指纹为重点，覆盖全部陷阱对）
- E2E：`make e2e`（docker 双 MySQL 8.0 实例；"不同时区"由种子在会话级 `SET time_zone`（+08:00 vs -04:00）模拟，而非服务端 `system_time_zone`，见 `e2e/docker-compose.yml` 头注释；40+ 场景断言退出码 / JSON 报告 / 并行指纹确定性）
- 验证状态（2026-08）：单测（含 `-race`）与全套 e2e 在 **MySQL 8.0** 上通过；TiDB / PolarDB-X / MySQL 5.7 按兼容性设计支持（应用层哈希、无 MySQL 专属函数），但尚未实测

```sh
# CI（GitHub Actions 参考）
- run: make build test
- run: make e2e
```
