# 点赞计数 A/B 压测

比较固定基线 `7c1c27d179bf22246931d249267a57df8b5cca4b` 的旧点赞链路与当前工作区的缓存计数、异步批量落库链路。可用 `--baseline-ref <commit>` 显式更换基线；不会因新提交上传而自动改用最新 HEAD。测试独占 `feedsystem-likebench` Compose 项目，使用 13317 / 16389 / 15689 / 18089 端口，不访问原有业务数据库、缓存或消息队列。

[2026-11-15 完整结果与结论](results/20261115T083209Z-ab/REPORT.md)：24 轮、24,000 次请求，包含逐请求原始样本和每表写入统计。

```powershell
python -m pip install -r experiments/like-count/requirements.txt
python experiments/like-count/benchmark.py --rates 50 150 300 --seconds 6 --repeats 2
```

需要 Docker、Go、Python。脚本把指定基线与当前后端源代码分别冻结到仓库 `.run/likebench/`，用同一个 Go 编译器生成 Linux API/Worker。元数据保存基线 commit、候选源文件 SHA256、编译器与环境信息。生产代码未插入压测专用逻辑。

每轮停止 API/Worker，重建隔离数据库并清空隔离 Redis/MQ；准备相同账号、JWT 缓存和视频，预热相同详情接口。按计划到达时间发请求，最多 64 个并发客户端，每次主测请求使用不同用户，避免账号限流及重复关系干扰。热点场景只写一个视频，分散场景轮询 64 个视频。各请求率依次测试，两轮使用 A→B、B→A 顺序，测试进程与被测服务均在本机。

CPU/内存配额：API、Worker 各 1 CPU / 384 MiB，`GOMAXPROCS=1`；MySQL 2 CPU / 1 GiB，缓冲池 256 MiB；Redis 1 CPU / 256 MiB，AOF 开启；RabbitMQ 1 CPU / 512 MiB，Erlang 两个 scheduler。运行环境还有原项目空闲容器，不能把本次结果当作独占宿主机的容量上限。

## 指标口径

- **接口 P95/P99**：成功 HTTP 请求实际发出至响应读完；另保留计划到达至响应延迟、调度延迟，便于识别压测端排队。成功吞吐用成功响应数除以 `max(计划持续时间, 最后响应时间减计划开始时间)`。
- **视频行更新次数**：MySQL `performance_schema.table_io_waits_summary_by_table.COUNT_UPDATE` 的区间差，包含点赞总数与热度。旧版每次有效点赞分别更新这两列，共两次；新版每批对同一视频合并更新两列。该指标不是全库 SQL 语句数。
- **全部数据库行写入操作**：隔离业务库所有表的 `COUNT_WRITE` 差，包含关系、视频、通知、增量日志插入及完成标记、刷新锁等操作。它不等于磁盘字节、redo/binlog 量，也不是成功业务变更条数；不能由它推断磁盘成本同比变化。
- **追平时间**：全部 HTTP 请求结束后，按约 200 ms 间隔轮询每个视频，连续两次确认有效关系数=数据库计数=成功请求预期；新版还要求待刷增量为零。包含轮询与查询时间，是保守上界。不是每条事件的精确延迟分布。
- 每轮另用同一账号并发发起 8 次重复点赞，验证最终只新增一条关系并只计一次。该验证位于性能与 I/O 快照之后，不混入主测统计。
- 写入统计截取到通知落库及队列排空之后，覆盖异步成本。旧版 HTTP 成功代表已发消息，新版成功代表关系与增量日志已提交；两者响应承诺不同，延迟对比必须保留这个前提。

MySQL 当前构建不为 Go 驱动的 `COM_STMT_EXECUTE` 填写 SQL digest，所以使用包含 prepared statements 的表 I/O 计数，避免把 digest 中缺失的写入误报为零。原始 JSON 仍保存文本协议 digest 的部分统计，但不将其作为全量写入指标。

## 产物与清理

每次执行在 `results/<UTC时间>-<标签>/` 保存元数据、逐请求样本、逐次追平状态和汇总 JSON。`--label smoke` 用于脚本调试，调试目录被 Git 忽略，不作为正式结果。正式实验报告见对应目录的 `REPORT.md`。

2026-11-15 的结果目录另存 `benchmark-measured.py.txt`，其 SHA256 与该次 `metadata.json` 一致，保留当时实际执行的脚本原文。发布版随后仅增加了显式基线选择参数，避免提交后默认基线漂移；历史实验数据没有重算或替换。

测试结束停止 API/Worker，保留隔离依赖供复查。确认不再需要环境后，可执行以下命令清理这个专用 Compose 项目：

```powershell
$env:BENCH_BIN_DIR = (Resolve-Path .run/likebench).Path
docker compose -p feedsystem-likebench -f experiments/like-count/compose.yaml --profile app down --volumes
```

6 秒负载、两轮重复适合小规模可复现对照；并未测得最大持续吞吐、长期积压、故障恢复吞吐或多 Worker 扩展上限。Redis 故障、数据库失败与重投正确性由后端集成测试覆盖。本实验没有模拟真实用户观看、取消点赞和复杂风控流量。
