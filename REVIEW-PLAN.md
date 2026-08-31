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
> | P3 | 11 | ✅ 全部已修复（08-31 实施，逐项证据见下；另抓出 1 个潜伏正确性 bug） |
>
> 原"暂缓发布"阻断项（P1）已解除。2026-08-31 验证记录：build / vet / gofmt /
> `go test -race -count=1 ./...` 全绿；e2e 全套 **45 项断言**通过、零跳过（含 json-report
> 与 parallel-determinism 两节；此前因本机无 jq 跳过，装 jq 后补跑通过；42→45 为 P3-#15
> 新增 `t_chunkc` 复合键场景 +2 断言）。期间还修掉 `e2e/run_e2e.sh` 一处潜伏 bug：json 节
> 的 mtdiff 调用未防 `set -e`，有差异时 exit 1 会无声终止整个套件（此前因整节跳过从未暴露）。
> P3 实施中新 e2e 场景抓出 **`keyRow` 复合键 ORDER BY 方向 bug**（见 P3-#15 附注），属潜伏
> 正确性问题，已一并修复并纳入 e2e 回归。
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

## P3（按需）— ✅ 全部 11 项已修复（08-31 实施完成）

1. **P3-10 ✅** Validate 顺序：`build()`（`flags.go`）与 `diffRunE`（`diff.go`）两处均改为先 `Validate()` 再 `ApplyDefaults()`（defaults 会改写 <=0 值，旧顺序让负值检查成为死代码）。`Validate` 补 DrillLimit 负值检查，全部错误信息带实际值；`--config` 场景报错前缀配置文件路径。单测 `TestValidateAndDefaults`（负 parallel/chunk_size/drill_limit/tolerance 均拒）。
2. **P3-11 ✅** `password_env` 指向未设置的变量现在启动即报 `password_env %q is set but the environment variable is not`（原先 `os.Getenv` 静默返回空 → 无密码连接）；区分未设置（报错）与设置但为空（合法，`os.LookupEnv`）。单测 `TestResolvePasswordEnvMissing`。
3. **P3-12 ✅** 文档化 `${ENV}` 限制：MANUAL.md / README.md 注明"解析前原文替换，值含引号/换行/冒号会破坏 YAML，只建议用于密码这类简单值"。
4. **P3-13 ✅** 两侧扫描并发：`compare` 内 errgroup 各起一 goroutine 跑 `scanSide`（src/dst 各自 chunk 并发不变），墙钟 src+dst → max。`pickScanError` 归因：一侧因另一侧错误被取消（`context.Canceled`）时，报告存活侧的真实错误，前缀 `src scan:`/`dst scan:`。单测 `TestPickScanError`（6 用例含取消归因）。
5. **P3-14 ✅** 行数不等短路：`srcTotal != dstTotal` 且未 `--drill` 时跳过全量行扫描，直接 `DIFFERENT`（`--drill` 仍需逐行明细不短路）。单测 `TestSkipRowScan`（表驱动含 drill=true 分支）。
6. **P3-15 ✅** 复合键前导 INT 算术切分：`integerSingleKey` → `integerLeadKey`（前导列 INT/UINT 即可）；`intBoundaries(lo, hi, n, leadPrefix)`，复合键边界只含前导列单值并带 `LoPrefix/HiPrefix`；谓词渲染为普通列比较（`` `a` > 7501 AND `a` <= 15002 ``）而非字典序展开。单测 `TestIntBoundariesLeadPrefix`（1..30001 可整除形状 + prefix 断言）、`TestPredicateLeadPrefix`（含全宽 prefix 仍走字典序）；e2e `t_chunkc`（复合 PK 30001 行、`--chunk-size 10000` 可整除）改最大前导值行 → exit 1。
   - **附注（计划外发现，潜伏正确性 bug，已修）**：新 e2e 场景首跑报"一致"，排查发现 `keyRow` 渲染 `ORDER BY a, b DESC`——**SQL 方向只作用于末列**，复合键的 max 行被读成 min 行（a 最小 + 其下 b 最大），整表键域塌缩成一行、两侧对称塌缩 → 静默假"一致"。复合键此前无任何 e2e 覆盖，潜伏至今。修复：`keyOrder` 每列重复方向（`a DESC, b DESC`）。单测 `TestKeyOrder`；e2e `t_chunkc` 两断言即回归。
7. **P3-16 ✅** 消除每行拷贝：`Normalize` 改收 `[]any`（database/sql 不能把 NULL Scan 进 `*driver.Value`，只能 `[]any`；元素仍是 driver 值，any→driver.Value 直接可赋值）；`Scan` 复用 `[]any` 扫描缓冲直接送 Normalize，删掉每行 make+copy；drill 的 `next()` 同理。单测全部 `[]any` 化后 `go test -race` 全绿，e2e 全过。
8. **P3-17 ✅** `chunk_size` 下限：`const MinChunkSize = 10`，显式正值低于下限被拒（`chunk_size 5 is below the minimum of 10`），0 仍表示"用默认"。单测 `TestValidateAndDefaults`（5 拒、10 收）。
9. **P3-18 ✅** 发布 commit（`c7316d5`）：README 时区描述改为实际机制（session 级 `SET time_zone` +08:00/-04:00，非系统时区），.cnf 文件保留为可选替代并说明；附"已在 MySQL 8.0 验证"注记。
10. **P3-19 ✅** drill 键渲染防碰撞：`lookupKey` 对 string/[]byte 分量 `strconv.Quote(shorten(t))`、其余走 `renderValue`，`shorten` 超 77 字符截断加 `...`；`("a","b | c")` 与 `("a | b","c")` 不再碰撞，`"42"` 与 int 42 可区分。单测 `TestLookupKeyCollision`（分隔符碰撞、类型碰撞、500 字符截断）。
11. **P3-20 ✅** 计数差专用 RowKind：`RowCountDiff = "COUNT_DIFF"`，`multisetDiff` 用 switch 区分三态（两侧都在但计数不等 → COUNT_DIFF；`s == nil` → MISSING_IN_SRC；`d == nil` → MISSING_IN_DST）。单测 `TestMultisetDiffKinds`。

## 已核实非缺陷（勿重复排查）

## 已核实非缺陷（勿重复排查）

- 并发：scanSide channel 逻辑正确（缓冲 len(chunks)、closer goroutine、无泄漏/死锁），`-race` 通过（08-31 复跑）；TableFingerprint 按 ID 排序与并发无关。
- 安全：DSN 用 `url.UserPassword` 编码、全链路打码（e2e 泄漏断言 08-31 实测通过）；标识符反引号转义 + information_schema 参数化，无注入；`--where`/`--key` 是操作者自供 SQL（文档化设计）；两级只读护栏逻辑正确。
- DECIMAL 不走 float64、BIT 跨宽按数值、NULL/空串/0 TLV 独立 tag——均正确且有测试。
- 依赖、跨平台（纯 SQL、无本地文件操作、CI 有 Windows 交叉编译）无问题。
- ~~原测试缺口根因~~（08-31 已解决）：`TestIntBoundaries` 曾全避开可整除情形、e2e `t_large` 跨度 99999 不被 10 整除恰好避开 P1-1、`TestTimeFormatting` 只测 500ms——现已补 `IntBoundariesDivisible`、`FractionalSecondCollisions`、`t_chunk`/`t_nullkey`/`t_fracsec` e2e 场景，同类"全绿仍漏 P1"的盲区已消除。

## 仍未验证的风险（08-31 更新）

- ~~e2e 未实际运行~~ → **已运行**（2026-08-31，`make e2e`，**45 项断言**全过、零跳过；jq 装于 `~/bin/jq` 1.7.1 后 json-report 与 parallel-determinism 两节实测通过）。
- TiDB / PolarDB-X 与 MySQL 5.7 兼容性未验证（e2e 只覆盖 MySQL 8.0 容器）。
- 千万行级性能未实测（P2-6/P2-7 修复已落地并有单测，P3-13/14/16/17 性能改进已落地，但大规模行为仍是代码推导，建议发布后找一张千万行级真实表跑一次基准）。

## 发布状态

**已发布 v0.1.0**（2026-08-31 决策：发布）。P1 三个静默假阴性 + P2 六项全部修复并经单测 + e2e 双重验证后，打 annotated tag `v0.1.0`（指向 `c7316d5`；源码 `Version` 为 `0.1.0-dev`，发布构建需 `-ldflags "-X mtdiff/cmd/mtdiff.Version=v0.1.0"`）。

P3 十一项于同日实施完毕（commits `a762295`/`d1f9ed0`/`ff2441b`），并在此过程中发现并修复了 `keyRow` 复合键 ORDER BY 潜伏 bug（见 P3-#15 附注）。这些改进在 tag 之后；如需发布，可在当前 HEAD 打 `v0.1.1`。

剩余未验证项（TiDB/5.7 兼容、千万行性能基准）属兼容性/性能风险而非正确性阻断。仓库为纯本地 git（无 remote，按需求方要求不推远程）。
