# VidFlow

**Go 短视频后端，覆盖视频生产、搜索、Feed 分发与互动。**

基于 Go + Vue 3 的短视频工程项目。原视频分片直传对象存储，后台生成播放文件和封面，通过资产状态控制发布。项目还提供浏览、点赞、评论、关注、私信与通知，并保留关注流与计数改造的回归测试和对照实验。

`Go / Gin` · `MySQL` · `Redis / Lua` · `RabbitMQ` · `Elasticsearch` · `S3` · `FFmpeg` · `Vue 3` · `Docker Compose`

[快速运行](#快速运行) · [视频生产与发布](handbook/media-pipeline.md) · [全站搜索](handbook/video-search.md) · [架构与一致性设计](handbook/architecture.md) · [压测证据](experiments/like-count/results/20261115T083209Z-ab/REPORT.md) · [接口文档](handbook/api.md)

## 工程问题

| 问题 | 实现与取舍 | 验证入口 |
|---|---|---|
| 上传中断，或 Worker 在生成产物后退出 | 分片状态从对象存储恢复；媒体资产状态、RabbitMQ 投递确认和处理租约支持重试 | [媒体链路与故障验证](handbook/media-pipeline.md) |
| 用户需要查找未刷到的视频，索引服务可能短暂故障 | Elasticsearch 中文检索、独立索引队列、Outbox 投递确认、历史回填 | [搜索与故障验证](handbook/video-search.md) |
| 同时间作品跨页遗漏；刷新后旧响应覆盖新列表 | 时间＋ID 复合游标；版本化缓存校验读写；前端请求取消与序号隔离 | [发布、刷新、10/10/6 连续分页验收](experiments/feed-following/03-combined-flow-results.md) |
| 热门视频每次点赞都更新同一数据库行 | 关系与恢复日志同事务；Redis 优先计数；按视频聚合增量，计数和完成标记同事务提交 | [计数实现](backend/internal/video/like_count.go) · [故障与幂等测试](backend/internal/video/like_count_test.go) |
| 优化收益缺少可核对的数据 | 隔离 A/B、固定请求率、热点/分散分布、交替执行两版；同时统计延迟、视频行更新和全库写入 | [24 轮压测与原始记录](experiments/like-count/README.md) |

## 一组有条件的优化结果

2026-11-15，在相同资源配额下，按 50 / 150 / 300 req/s 测试热点与 64 视频分散场景，每轮 6 秒、两轮交替 A→B / B→A，共 **24,000 次主测请求**。接口请求全部成功，最终逐视频关系数与数据库计数一致，并发重复点赞只累计一次。

以下是 **300 req/s、单视频热点场景**；行写入数为两轮合计，延迟为合并样本，追平时间为两轮范围。

| 指标 | 旧版 | 当前实现 |
|---|---:|---:|
| 视频行更新，含点赞数和热度 | 7,200 | **7** |
| 全库行写入操作 | 14,400 | 14,415 |
| HTTP P95 / P99 | 2.94 / 3.64 ms | 7.91 / 9.44 ms |
| 停写后的数据库计数追平 | 8.32–9.95 s | **4.22–4.73 s** |

收益是视频热点行更新减少 **99.90%**，以及该负载下积压追平更快。关系恢复日志增加了写入成本，全库行写入基本持平；新版接口要同步确认关系与日志提交，响应承诺和旧版不同。低负载下批处理等待也会拉长计数追平时间。当前实验没有测得系统吞吐上限或多 Worker 扩展收益。

[完整方法与边界](experiments/like-count/README.md) · [各场景结果](experiments/like-count/results/20261115T083209Z-ab/REPORT.md) · [每轮指标及每表 I/O](experiments/like-count/results/20261115T083209Z-ab/summary.json)

## 系统架构

```mermaid
flowchart LR
    UI[Vue 3 客户端] --> API[Go / Gin API]
    API -->|关系与计数日志同事务| DB[(MySQL)]
    API -->|鉴权 / Feed 缓存 / 展示计数| R[(Redis)]
    DB -->|轮询未完成计数事件| W[业务 Worker]
    W -->|合并总数与完成标记| DB
    W -->|计数事件幂等投影| R
    API -->|通知与热度等事件| Q[RabbitMQ]
    DB -->|轮询发布 Outbox| API
    API -->|确认投递发布事件| Q
    Q -->|独立索引队列| W
    W -->|按视频 ID 写入索引| ES[(Elasticsearch)]
    API -->|全文检索| ES
    Q -->|评论 / 关注 / 热度消费| W
    Q -->|通知消费与 SSE| API
    API -->|媒体静态路径| FS[本地媒体文件]
    UI -->|分片直传 / 播放| S3[对象存储]
    W -->|读取原视频 / 保存封面和转码产物| S3
    Q -->|媒体任务| W
```

采用模块化单体与独立业务 Worker。**计数批处理直接读取数据库增量日志**；RabbitMQ 承载通知、评论、关注、热度等事件。通知和时间线的部分消费者目前仍在 API 进程中。[详细时序、恢复机制与扩容边界 →](handbook/architecture.md)

## 功能与当前范围

| 模块 | 当前能力 |
|---|---|
| 视频 | S3 分片直传、断点续传、整文件 SHA-256 校验、异步转码与封面、资产状态校验后发布 |
| 搜索 | 中文标题、简介和发布时作者名检索，话题、作者 ID 和日期筛选，快照游标分页、历史回填和重投 |
| Feed | 最新流、关注流、点赞榜、窗口热榜、话题流；热索引/历史查询分层与实体缓存 |
| 互动 | 点赞/取消、评论、关注/取关、用户主页统计、私信及通知记录/SSE |
| 工程 | JWT 与双 Token、账号限流、Docker Compose、API/Worker、pprof、CI 与自动化测试 |

推荐标签页当前使用最新流。媒体产物为一份 H.264 MP4 和 JPEG 封面。关系分片及复合主键布局是独立实验方案，业务关系表沿用现有结构。

## 快速运行

```bash
git clone https://github.com/luob8110-afk/VidFlow.git
cd VidFlow
docker compose up -d --build
```

| 入口 | 地址 |
|---|---|
| 页面 | http://localhost:5173 |
| API | http://localhost:8080 |
| RabbitMQ 管理台 | http://localhost:15672 |
| 本地对象存储 | http://localhost:8333 |

注册账号，选择文件上传，等待处理完成后发布。可用访客窗口在顶部搜索标题关键词或按话题筛选，点击结果播放。建立关注关系后，在关注页主动刷新并连续翻页。对同一个点赞按钮再次点击取消。[媒体续传与恢复验证](handbook/media-pipeline.md#验证)

本地默认凭据及环境变量见[运行文档](handbook/development.md)。从旧版升级需先排空旧点赞消息并一起更新 API 与 Worker；真实 MySQL 集成测试须配置独立测试库，未配置时会跳过。[运行、升级与测试 →](handbook/development.md)

## 阅读代码

| 设计入口 | 关键文件 |
|---|---|
| 点赞关系与恢复日志事务 | [like_service.go](backend/internal/video/like_service.go) |
| 计数冷启动、回源与批量事务 | [like_count.go](backend/internal/video/like_count.go) |
| Redis 原子去重与计数 | [redis/like_count.go](backend/internal/middleware/redis/like_count.go) |
| 关注流版本缓存与分页 | [feed/service.go](backend/internal/feed/service.go) · [following_cursor.go](backend/internal/feed/following_cursor.go) |
| 前端请求生命周期 | [useVideoFeed.ts](frontend/src/composables/useVideoFeed.ts) |
| 发布与刷新组合回归 | [following_flow_test.go](backend/internal/feed/following_flow_test.go) · [useVideoFeed.test.mjs](frontend/tests/useVideoFeed.test.mjs) |

## 实验证据

- [关注流正确性与阶段验收](experiments/feed-following/03-combined-flow-results.md)：固定集合分页、主动刷新、失败重试和迟到请求；附历史容量实验归档。
- [点赞计数 A/B](experiments/like-count/README.md)：固定基线 commit、隔离依赖、原始延迟样本、每表行写入、追平状态与源码哈希。
- [百万条关系的存储布局比较](experiments/edge-storage/README.md)：在同等最近点赞索引能力下，复合主键候选样本空间下降约 29%。该实验的 DAU 与存量规模属于演算假设。

## 来源与改造范围

基础账号、上传和社交功能继承自上游。VidFlow 增加了媒体资产处理和全站搜索，并改进了关注流分页与点赞计数。[上游项目](https://github.com/LeoninCS/feedsystem_video_go) · [上游作者的后续项目](https://github.com/LeoninCS/GCFeed) · [VidFlow 架构与代码入口](handbook/architecture.md)
