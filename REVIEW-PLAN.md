# mtdiff 代码 Review 修复计划（2026-08-28 制定，2026-08-31 状态复核）

> 来源：全项目严格 Review（build / vet / `go test -race` / gofmt 全部通过）。
> 原结论（08-28）：暂缓发布——3 个 P1 静默假阴性 bug（会把不一致的表报成"一致"，exit 0）。
>
> **状态汇总（2026-08-31 逐项复核）**：
>
> | 级别 | 项数 | 状态 |
> |---|---|---|
> | P1 | 3 | ✅ 全部已修复，单测回归在位 + e2e 实测通过 |
> | P2 | 6 | ✅ 全部已修复（P2-4/5/7/9 带单测回归；P2-6/8 代码核实） |
> | P3 | 11 | ⬜ 全部未修复（非阻断，按原计划"需要时再修"） |
>
> 原"暂缓发布"阻断项（P1）已解除。2026-08-31 验证记录：build / vet / gofmt /
> `go test -race -count=1 ./...` 全绿；e2e 全套 42 项断言通过、零跳过（含 json-report
> 与 parallel-determinism 两节；此前因本机无 jq 跳过，装 jq 后补跑通过）。补跑时还修掉
> `e2e/run_e2e.sh` 一处潜伏 bug：json 节的 mtdiff 调用未防 `set -e`，有差异时 exit 1
> 会无声终止整个套件（此前因整节跳过从未暴露）。
> 注意：本环境 `go` 不在默认 PATH，工具链在 `/home/liukl/sdk/go/bin`；e2e 需 docker（`make e2e`）。

## P1（阻断，发布前必须修复）— ✅ 全部完成

### P1-1 ✅ 已修复（08-31 复核）：整型键切块 off-by-one
- 原位置：`internal/chunk/plan.go`（`intBoundaries`）
- 原 Bug：`step := (hi - lo + n - 1) / n` 是 `ceil((hi-lo)/n)`，覆盖闭区间 `[lo,hi]`（hi-lo+1 个值）应为 `ceil((hi-lo+1)/n)`。当 `(hi-lo) % n == 0` 时 step 小 1，末块 Hi 止于 `hi-1`，最大键值行被两侧同时漏扫 → 两侧对称漏扫 → 假"一致"。
- **现状**：`plan.go:149` 已改 `step := (hi - lo + n) / n`，`intBoundaries` 注释（:138-141）专门记录了该坑。
- **验证**：单测 `TestIntBoundariesDivisible`（`plan_test.go:80-98`：(1,7,3)、(0,99,11)、(1,90001,10) 可整除用例）；e2e `t_chunk` id 1..90001 + `--chunk-size 10000`，`m_chunk_max.sql` 改最大 id 行 → exit 1（`run_e2e.sh:111-117`）。

### P1-2 ✅ 已修复（08-31 复核）：NULL 键值被排除出所有块谓词
- 原位置：`internal/chunk/plan.go`（extremes）、`internal/chunk/predicate.go`、`internal/conn/schema.go`（`SelectKey`）
- 原后果：① 单列可空唯一键：NULL 键行不被任何块扫描但 `COUNT(*)` 计入 → 假"一致"。② 复合键前导列可空且含 NULL：所有块为空 → 两侧行数相同的任意两张表判"一致"。③ 键值全 NULL：报 "empty key range" 硬错误。
- **现状**：
  1. `SelectKey`（`schema.go:229-244`）关联 `COLUMNS.IS_NULLABLE`（`nullableColumns` :249），只选**所有列均 NOT NULL** 的唯一索引；否则回退无键路径（KeySource "none"）。
  2. extremes（`plan.go:57-78`）改用 `ORDER BY … LIMIT 1` 取首/末行（不再 MIN/MAX，NULL 安全）；谓词层对 NULL 边界显式处理（`predicate.go:56-69` 单列 `k IS NULL OR k <= hi` 等；`rowCompare` :112-148 复合键 NULL 分量生成 `k1 IS NULL AND …` 分支）。
  3. 显式 `--key` 命中可空列时打印告警（`comparer.go:311-335` `applyKey` + `keyNullabilityWarns`）。
- **验证**：单测 `TestPredicateNULLBounds`（`plan_test.go:142-184`）；e2e `t_nullkey`（无 PK、可空唯一键）自动降级无键比较，`m_nullkey_change.sql` 改 NULL 键行 → exit 1（`run_e2e.sh:118-123`）。

### P1-3 ✅ 已修复（08-31 复核）：秒级小数规范化碰撞
- 原位置：`internal/normalize/value.go`（`formatMySQLTime` / `formatMySQLDateTime`）
- 原 Bug：把微秒计数当整数 `TrimRight(..., "0")`：1ms/10ms/100ms 全部渲染 `.1`（所有 10 的幂次小数秒两两碰撞）。
- **现状**：先零填充到 6 位再裁尾零（`value.go:173-174` TIME、`:190-191` DATETIME）：`frac := fmt.Sprintf("%06d", f); strings.TrimRight(frac, "0")`。
- **验证**：单测 `TestTimeFormatting`（`normalize_test.go:107-152`：1ms→`.001`、10ms→`.01`、100ms→`.1` 互不相同；DATETIME 10ms/100ms 同）+ `TestFractionalSecondCollisions`；e2e `t_fracsec` 100ms vs 10ms，`m_fracsec_change.sql` → exit 1（`run_e2e.sh:124-127`）。

## P2（建议发布前处理）— ✅ 全部完成

1. **P2-4 ✅** `internal/compare/comparer.go:342` `filterIgnored`：原 = `--ignore-columns` 非空时 dst 独有列被静默丢弃（ignore 为空时同样漂移却硬报错，行为不一致）。**现状**：重建 dstKeep 后，dst 中既不在 keep 也不在 ignore 的列报错（:359-361 `"destination column %s is neither compared nor listed in --ignore-columns"`），两路径行为统一。单测 `TestFilterIgnoredDrift`（`compare_test.go:44`：src 2 列/dst 3 列、ignore 拼错名、被 ignore 的 dst 独有列、全 ignore、缺列）。
2. **P2-5 ✅** `cmd/mtdiff/diff.go:61-85`：原 = `--tolerance 0`/`--drill-limit 0`/`--max-allowed-packet 0` 用 `!= 0` 判断，无法覆盖 YAML 非零值。**现状**：五处（parallel/chunk-size/tolerance/drill-limit/max-allowed-packet）全部 `cmd.Flags().Changed(...)`，diff.go 无残留 `!= 0` 检查。单测 `TestApplyOptionsZeroOverrides`、`TestApplyOptionsIllegalValues`（`diff_test.go:27` 起）。
3. **P2-6 ✅** `internal/conn/conn.go`：原 = `AcquireScan` 每块 checkout 重放 5 条 SET；`sql_mode = CONCAT(@@sql_mode,…)` 非幂等，超 `sql_max_mode_size` 后每块一条 stderr 告警。**现状**：`OpenSide`（:118-127）一次性预热 scan 池（每连接应用一次策略），`AcquireScan`（:197-203）退化为纯 checkout，每块零 SET 往返；`addSQLModeFlags`（:163-185）先读 `@@SESSION.sql_mode`、缺失才补 flag，幂等，mode 不再增长。（无专门单测，代码核实；e2e 万行级扫描未见告警噪音。）
4. **P2-7 ✅** `internal/compare/drilldown.go`：原 = 无键表整表单块 `--drill` 时整表两侧行物化进 map，大表 OOM。**现状**：`drillMaxRows = 100_000`（:49），`bufferRows`（:141-187）超限停止缓冲、继续抽干迭代器、返回 `truncated=true`；`comparer.go:155-158` 一次性 stderr 告警 "…capped at 100000 rows per side; row-level results are a sample (truncated)"。内存有界（~1e5 行/侧）。单测 `TestDrillDownRowCap`。
5. **P2-8 ✅** `.github/workflows/ci.yml:15,29` `go-version: "1.25"` 与 `go.mod:3` `go 1.25.0` 已一致（1.25 ≡ 1.25.0，无需工具链自动下载兜底）。
6. **P2-9 ✅** 测试缺口已补：新增 `internal/compare/compare_test.go`（`TestFilterIgnoredDrift`/`TestDrillDownRowCap`/`TestApplyKey`/`TestFoldDigests`——后者覆盖行数不等分支，原"无 DB 不可测"）与 `cmd/mtdiff/diff_test.go`（`TestApplyOptionsZeroOverrides`/`TestApplyOptionsIllegalValues`）；e2e 已接入 P1 三场景（`run_e2e.sh:111-127`）。全部测试文件：`compare_test.go`、`diff_test.go`、`conn_test.go`（TestClassify/QuoteIdent/BuildDSN/Compatible）、`config_test.go`（ParseShorthand/MaskedDSN/LoadFileEnvExpansion/ResolvePasswordPriority/ValidateAndDefaults）、`plan_test.go`（IntBoundaries/IntBoundariesDivisible/Predicate/PredicateNULLBounds/Literal/ValuesEqual）、`normalize_test.go`（Decimal/TLVLayout/NULLVsEmptyVsZero/FormatFloat/TimeFormatting/FractionalSecondCollisions/FormatBit/NormalizeJSON/StringOptions/RowValueTypes）、`hash/digest_test.go`（顺序依赖、指纹块序无关、SumSq/Xor 缺口、Secure）。

## P3（按需）— ⬜ 全部未修复（08-31 逐项复核确认）

| # | 位置（08-31 复核后现行） | 一句话（原描述 + 现状证据） |
|---|---|---|
| 10 | `cmd/mtdiff/flags.go:88-91`、`diff.go:167-168` | `build()` 仍是先 ApplyDefaults 后 Validate，YAML 负值（parallel: -1）被静默改写，`config.go:109-117` 负值检查依旧死代码；DrillLimit 无负值检查（`config.go:135-137` 仅 `<=0 → 10`）。改为先 Validate 再 ApplyDefaults |
| 11 | `internal/config/config.go:83-86` | `password_env` 配置了但环境变量未设置 → 仍静默无密码连接（注释 :80-81 甚至明文认可 password-less）；应报明确错误。`config_test.go:92-97` 只测 env 已设置场景 |
| 12 | `internal/config/config.go:56-73` | `${ENV}` 仍为 YAML 解析前原始字节替换，值含引号/换行/冒号会破坏 YAML；MANUAL.md:90 / README.md:44 均无限制说明。文档化或限制作用域 |
| 13 | `internal/compare/comparer.go:133-140` | 两侧仍串行扫描（errgroup 只用于单侧内部 chunk 并发）；可 errgroup 并发两侧（墙钟 src+dst → max） |
| 14 | `internal/compare/comparer.go:104-127` | `srcTotal != dstTotal` 无短路，行数已不等仍全量扫描两侧（`foldDigests:301` 判 DIFFERENT 在扫描全部完成之后） |
| 15 | `internal/chunk/plan.go:174-221` | 非整型/复合键 `planSample` 仍是每分裂一次 `LIMIT 1 OFFSET off`，O(N log n)；`integerSingleKey`（:52-55）仅限单键，前导 INT 复合键无算术切分路径 |
| 16 | `internal/chunk/plan.go:274-277`、`normalize.go:74` | Scan 仍每行 `make([]driver.Value)` + 逐元素拷贝桥接（`drilldown.go:121-124` 同）；`Normalize` 仍收 `[]driver.Value`，未改收 `[]any` |
| 17 | `internal/config/config.go:112-133`、`comparer.go:120` | 仅 `ChunkSize < 0` 被拒，正值无下限：`--chunk-size 1` 作用于亿行 → Chunk/channel/byID 无界增长。设合理下限 |
| 18 | `e2e/docker-compose.yml`、`README.md:110` | .cnf 仍是孤儿（compose 无 volumes，头注释改用 session 级 `SET time_zone` 模拟；种子 `seed_src.sql:8`/`seed_dst.sql:8` 固定 +08:00/-04:00），README 仍称 "Asia/Shanghai vs America/New_York"。接进 compose 或改 README |
| 19 | `internal/compare/drilldown.go:169,269,286-290` | keyed drill map 键仍 `" | "` 拼接、string 分支不引号，`("a","b \| c")` 与 `("a \| b","c")` 碰撞（仅展示层）。每键值 `%q` |
| 20 | `internal/compare/drilldown.go:20-24,248-251` | RowKind 仍只有 CHANGED/MISSING_IN_DST/MISSING_IN_SRC，无键表两侧都在但计数不等仍落 `MISSING_IN_DST`。加专用 RowKind |

## 已核实非缺陷（勿重复排查）

- 并发：scanSide channel 逻辑正确（缓冲 len(chunks)、closer goroutine、无泄漏/死锁），`-race` 通过（08-31 复跑）；TableFingerprint 按 ID 排序与并发无关。
- 安全：DSN 用 `url.UserPassword` 编码、全链路打码（e2e 泄漏断言 08-31 实测通过）；标识符反引号转义 + information_schema 参数化，无注入；`--where`/`--key` 是操作者自供 SQL（文档化设计）；两级只读护栏逻辑正确。
- DECIMAL 不走 float64、BIT 跨宽按数值、NULL/空串/0 TLV 独立 tag——均正确且有测试。
- 依赖、跨平台（纯 SQL、无本地文件操作、CI 有 Windows 交叉编译）无问题。
- ~~原测试缺口根因~~（08-31 已解决）：`TestIntBoundaries` 曾全避开可整除情形、e2e `t_large` 跨度 99999 不被 10 整除恰好避开 P1-1、`TestTimeFormatting` 只测 500ms——现已补 `IntBoundariesDivisible`、`FractionalSecondCollisions`、`t_chunk`/`t_nullkey`/`t_fracsec` e2e 场景，同类"全绿仍漏 P1"的盲区已消除。

## 仍未验证的风险（08-31 更新）

- ~~e2e 未实际运行~~ → **已运行**（2026-08-31，`make e2e`，42 项断言全过、零跳过；jq 装于 `~/bin/jq` 1.7.1 后 json-report 与 parallel-determinism 两节实测通过）。
- TiDB / PolarDB-X 与 MySQL 5.7 兼容性未验证（e2e 只覆盖 MySQL 8.0 容器）。
- 千万行级性能未实测（P2-6/P2-7 修复已落地并有单测，但大规模行为仍是代码推导；P3-13/14/17 与性能直接相关）。

## 发布状态

原"暂缓发布"的阻断项（P1 三个静默假阴性）已修复并经单测 + e2e 双重验证，P2 亦全部落地。
剩余未验证项（TiDB/5.7 兼容、千万行性能）属兼容性/性能风险而非正确性阻断；P3 为改进项。
是否发布由需求方决定；建议发布说明中注明"已在 MySQL 8.0 验证"。
