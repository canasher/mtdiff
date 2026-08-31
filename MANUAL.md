# mtdiff 使用手册（人话版）

> 赶时间只看前三节；其他部分按你的场景找。技术细节（设计、测试）见 [README.md](README.md)。

## 这东西是干嘛的

你有两个 MySQL（或者 TiDB、PolarDB-X，凡是说 MySQL 协议的都行），想知道"两边的表数据一不一样"。
跑一下它，它告诉你三种结果之一：**一致**、**不一致（哪张表、哪一行）**、**比不了（报错）**。

典型场景：

- 升级 / 迁移前后，核对新旧实例数据是否一致
- 检查主从（源库 / 副本）是否漂移
- 跑完数据订正脚本，确认改对了

它不是把数据全拉出来一行行比（那样又慢又费网），而是**分块算指纹**：两边某一块指纹一样就算一致，只对不一样的块回头细看。千万行的表，内存也就百 MB 量级，不会把库里的数据整个读进内存。

## 安装

```sh
make build
```

得到一个单文件 `bin/mtdiff`，拷到哪都能跑，没有别的依赖。

## 三十秒上手

```sh
# 比较两个库里所有共有的表
./bin/mtdiff \
  --src root:pass@10.0.0.1:3306/dbA \
  --dst root:pass@10.0.0.2:3306/dbB
```

连接串格式：`用户:密码@主机:端口/库名`。端口是 3306 可以省，没密码可以省。
（密码写在命令行上会留在 shell 历史里，介意就用下面的 `--src-password-env`。）

输出长这样：

```
TABLE                  SRC_ROWS  DST_ROWS  STATUS     DETAIL
t_large                  100000    100000  OK
t_orders                 52341     52340  DIFFERENT  row count differs (52341 vs 52340); 1/4 chunks differ
```

然后看退出码（写脚本用）：

| 退出码 | 意思 |
|---|---|
| 0 | 全部一致 |
| 1 | 有差异 |
| 2 | 出错（连不上 / 两边类型对不上 / 数据比不了） |
| 3 | 参数写错了（比如 `--parallel 0`） |

`diff` 子命令可以省略，`mtdiff diff ...` 和 `mtdiff --src ...` 是一回事。

## 连接怎么给

**细粒度 flag**（密码不想上命令行时用）：

```sh
mtdiff \
  --src-host 10.0.0.1 --src-port 3306 --src-user replica \
  --src-password-env SRC_PWD --src-db dbA \
  --dst-host 10.0.0.2 --dst-user replica \
  --dst-password-env DST_PWD --dst-db dbB
```

`--src-password-env SRC_PWD` 的意思是"密码去环境变量 SRC_PWD 里读"，`--dst-...` 同理。

**YAML 配置文件**（参数多不想一屏幕 flag 时）：

```sh
mtdiff --config cfg.yaml
```

```yaml
src:
  host: 10.0.0.1
  user: replica
  password_env: SRC_MYSQL_PWD   # 密码从环境变量读
  database: dbA
dst: { ... }
options:
  tables: [orders, users]
  parallel: 8
  ignore_columns: [updated_at]
```

命令行参数会覆盖 YAML 里的值；YAML 里可以用 `${环境变量名}` 引用环境变量。
注意这是**原文替换**（解析 YAML 之前替换），变量值里含引号、换行、冒号等会破坏 YAML 结构——只建议用于密码这类简单值，结构化值请直接写在文件里。

**密码的优先级**：环境变量（`password_env`）> 写在文件/命令行里的密码 > 交互式询问。
`password_env` 指向一个未设置的变量会直接报错（而不是静默地无密码连接）。
非交互场景（cron、CI）不会弹询问，直接报连接错误。
放心，任何日志、报错、报告里密码都是打码的（`u:***@h:port/db`），不会泄露。

## 最常用的操作

**只比几张表 / 排除几张表**

```sh
mtdiff --src ... --dst ... --tables orders,users
mtdiff --src ... --dst ... --exclude-tables tmp_xxx
```

默认比较两边都有的表；只在一边存在的表会被跳过（`mtdiff tables --src ... --dst ...` 可以列出两边的表）。

**想知道具体哪一行不一样**

```sh
mtdiff --src ... --dst ... --drill
```

会列出示例差异行：键是什么、源端值、目的端值。
`CHANGED` = 值变了，`MISSING_IN_DST` = 目的端缺，`MISSING_IN_SRC` = 源端缺。
默认最多列 10 行，`--drill-limit 50` 可以加。

**某列允许不一样**（比如 `updated_at` 这种）

```sh
mtdiff --src ... --dst ... --ignore-columns updated_at
```

注意两点：目的端如果有**既不在比较范围、又不在 ignore 列表**的列，会直接报错，不会悄悄跳过——宁可错报不放过；ignore 里的列名拼错了等于没写，同样会报出来。

**加个条件过滤**（只比这个月的订单）

```sh
mtdiff --src ... --dst ... --where "create_time >= '2026-08-01'"
```

条件原样作用在两边，行数、切块、比较都按过滤后的来。这是你自己提供的 SQL 片段，自己保证合法。

**表没有主键**

能比，但语义变成"行袋子"（多集合）比较：行顺序无所谓、重复行按次数算，
能告诉你"某个值源端有 3 次、目的端有 2 次"，但**指不出具体哪一行**。
能给出一个可用的列（哪怕不唯一）就能升级成行级定位：

```sh
mtdiff --src ... --dst ... --tables t_nopk --key w
```

自动选键的规则：主键 > 所有列都不允许 NULL 的唯一索引 > 都没有就按无键处理。
唯一索引建在可空列上的**不会**拿来切块（NULL 行切不干净），宁可走无键路径。
显式 `--key` 指到可空列会给你一条告警。

**浮点数两边有精度差**

```sh
mtdiff --src ... --dst ... --tolerance 1e-9
```

默认浮点是**逐位精确**的，差最后一位也算差异；显式给容差才按桶归并。

**数据还在被写（怕比较窗口不一致）**

```sh
mtdiff --src ... --dst ... --snapshot
```

每张表在一个一致性快照事务里扫完，扫描期间的写入不影响结果。
代价是慢，长事务占 read view，千万行级慎用。

**一边 DATETIME、一边 TIMESTAMP**

```sh
mtdiff --src ... --dst ... --allow-tz-swap
```

TIMESTAMP 存的是绝对时刻（两边都强制按 UTC 比，不同时区的机器写同一时刻判相等，这是对的）；
DATETIME 是纯墙钟，两者语义不同，默认直接报错，显式开了才按时刻比。

**字符串大小写 / 尾部空格**

- 默认：裁掉尾部空格（贴近 CHAR 语义）、大小写敏感、逐字节比。
- `--fold-case` 忽略大小写；`--no-trim` 不裁空格。
- JSON 列默认按原始字节比；`--normalize-json` 先做规范化（键排序、数字归一）再比。

**表里有超大 BLOB**

```sh
mtdiff --src ... --dst ... --max-allowed-packet 67108864
```

**接 CI**

```sh
mtdiff --src ... --dst ... --json | jq .ok
# 输出 false 就是有差异；或者干脆只看退出码
```

CI 里密码放环境变量，配合 `--src-password-env` / `--dst-password-env`。

## 它怎么判"一样"（别把结果用错）

- **NULL、空串、0 是三回事**，互不混淆。
- **DECIMAL 按十进制归一化**：`1.00` 和 `1` 算一样；全程字符串处理，绝不经过 float。`decimal(10,2)` 对 `decimal(12,3)` 能比（会提示）。
- **FLOAT/DOUBLE 默认逐位精确**；`--tolerance` 才开容差。
- **BIT 按数值比**：bit(1) 的 1 和 bit(8) 的 1 是同一个值。
- **字符串默认精确**：大小写敏感，只裁尾部空格；两边 collation 不同时提示一声，然后按字节比（宁可误报，不可漏报）。
- **日期**：TIMESTAMP 按时刻（UTC）比，DATETIME 按墙钟比，两者默认互不兼容。
- **零日期（`0000-00-00`）不支持**：遇到会明确报错。用 `--ignore-columns` 把那一列排除掉，或者先修数据。
- **类型兼容**：INT 对 BIGINT 这类"同类数字"允许（会提示）；DATETIME 对 TIMESTAMP 要 `--allow-tz-swap`；JSON 对 TEXT 这种跨类直接报错。想全部按字节级严格来，`--strict-types`。

## 敢不敢上生产

- **只读，硬保证**：每条连接用之前先强制只读会话（MySQL 本体没有会话级 read_only 时回退到只读事务），做不到就拒绝运行，绝不向被比的库发写语句。
- **影响小**：不持锁；锁等待超时 5 秒、单条语句限时 5 分钟（尽力而为，兼容层不支持就跳过）。
- **准不准**：块一致靠指纹，64 位指纹理论上有 2⁻⁶⁴ 的碰撞概率，不能接受就加 `--secure` 升到 128 位；指纹不一致的块会自动下钻到行，所以"有差异"永远带着具体证据，不是空口。
- **兼容层**：TiDB / PolarDB-X 这类 MySQL 协议实现可直接用。

## 性能

- 内存：流式读，绝不整表进内存；千万行级表约百 MB。
- `--parallel`（默认 4）：并发的块扫描数，大表可以开 8~16。
- `--chunk-size`（默认 10000 行）：块大小。有主键的表按块比，只有差异块才细看；无键表整表一趟。

## 常见问题

**Q：退出码 2，咋办？**
看报错信息，就三类：连不上（地址 / 密码 / 库名写错）、两边 schema 对不上（见上面类型兼容规则）、表不存在。

**Q：刚改的一行为什么说一致？**
表在并发写入，扫描窗口里两边看到的状态不同。加 `--snapshot`。

**Q：提示没有可比的表？**
默认只比两边都有的表。用 `--tables` 显式指定，或者 `mtdiff tables --src ... --dst ...` 先看看两边各有哪些表。

**Q：能反复跑吗？**
能，纯只读。挂 cron 每 5 分钟跑一次只看退出码都行。

**Q：怎么构建 / 测试？**

```sh
make build   # 构建
make test    # 单元测试
make e2e     # 端到端：docker 起两个 MySQL 容器跑全部场景
```
