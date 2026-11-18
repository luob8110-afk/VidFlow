# 本地运行与验证

[返回项目首页](../README.md) · [架构与一致性设计](architecture.md)

Compose 默认启用 S3 兼容对象存储及 FFmpeg Worker。视频发布的配置和验收命令见[视频生产与发布](media-pipeline.md)。本机手动运行 Go 时，可继续使用原有本地上传，也可按该说明设置 `MEDIA_S3_ENDPOINT` 启用资产模式。

## Docker Compose 一键启动

```bash
docker compose up -d --build
```

访问：
- 前端：`http://localhost:5173`
- 后端 API：`http://localhost:8080`
- RabbitMQ 管理台：`http://localhost:15672`（`admin` / `password123`）

Docker Compose 会读取 `.env`，缺省使用 `feedsystem-dev-secret-key`。生产环境请修改 `JWT_SECRET`。

## 脚本启动

```bash
./start.sh
```

`start.sh` 默认启动 RabbitMQ、Redis、后端 API、Worker 与前端。常用开关：

```bash
START_FRONTEND=0 ./start.sh       # API + Worker
START_WORKER=0 ./start.sh         # API + 前端
STOP_DOCKER=1 ./start.sh          # 退出时停止脚本拉起的 compose 服务
CONFIG_PATH=configs/config.yaml ./start.sh
```

## 本地开发

```bash
# 启动依赖
docker compose up -d mysql redis rabbitmq

# 后端
cd backend
CONFIG_PATH=configs/config.compose-local.yaml go run ./cmd

# Worker
CONFIG_PATH=configs/config.compose-local.yaml go run ./cmd/worker

# 前端
cd frontend
npm install && npm run dev
```

## CI

GitHub Actions 配置位于 `.github/workflows/ci.yml`，在 Pull Request 以及推送到 `main`、`master` 时运行。

- 后端：Go 1.24.x，执行 `go mod download`、`go vet ./...`、`go test -race -count=1 ./...`
- 前端：Node.js 22，执行 `npm ci`、`npm run build`

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `JWT_SECRET` | `feedsystem-dev-secret-key` | JWT 签名密钥，生产须改 |
| `SERVER_PORT` | `8080` | 后端监听端口 |
| `MYSQL_HOST` / `MYSQL_PORT` | 配置文件值 | MySQL 地址 |
| `MYSQL_USER` / `MYSQL_PASSWORD` | 配置文件值 | MySQL 账号密码 |
| `MYSQL_ROOT_PASSWORD` | `123456` | MySQL root 密码 |
| `MYSQL_DATABASE` | `feedsystem` | MySQL 数据库名 |
| `REDIS_HOST` / `REDIS_PORT` | 配置文件值 | Redis 地址 |
| `REDIS_PASSWORD` | `123456` | Redis 密码 |
| `REDIS_DB` | `0` | Redis DB |
| `RABBITMQ_HOST` / `RABBITMQ_PORT` | 配置文件值 | RabbitMQ 地址 |
| `RABBITMQ_USER` / `RABBITMQ_PASS` | `admin` / `password123` | RabbitMQ 账号 |

详见 `.env.example`。

## 运维与可观测性

- `GET /healthz` 返回后端健康状态。
- 本地配置默认开启 pprof：API `localhost:6060`，Worker `localhost:6061`。
- 上传文件写入 `backend/.run/uploads`；Docker 环境挂载到 `backend_uploads` volume。
- Redis 用于 Token 缓存、视频实体缓存、Feed 时间线、热榜窗口、分片上传会话。
- RabbitMQ Topic Exchange 覆盖点赞、评论、关注、热度、视频时间线事件，并配置 DLX。


## 行为测试

```bash
cd backend
go test -count=1 ./...
go vet ./...

cd ../frontend
node --test tests/useVideoFeed.test.mjs
npm run build
```

后端真实 MySQL 测试分别由 `FEED_TEST_MYSQL_DSN`、`COUNT_TEST_MYSQL_DSN` 启用。两个 DSN 必须指向独立测试库，测试会创建和清理带前缀的表；未设置时对应集成测试跳过，单元测试通过不能替代真实数据库验证。`go test -race -count=1 ./...` 需要支持 CGO 的工具链；本次竞态验证在 Linux Go 容器中执行。

关注流组合测试覆盖发布后主动刷新、刷新失败、旧分页迟到与 10/10/6 条连续分页；前端有 8 项请求状态测试。计数测试覆盖并发点赞、重复请求、事务回滚、并发刷新、缓存丢失及恢复。

## 点赞链路升级

从旧版升级时先暂停点赞写入，用旧 Worker 排空 `like.events`，再同时更新 API 与 Worker。启动自动迁移 `like_count_events` 和 `like_count_flush_locks`；旧计数基线沿用 `videos.likes_count`，已有偏差需要独立对账。新 API 发出的通知带 `notification_only` 标记，应避免与旧版 Worker 混跑。

计数 Worker 默认每 5 秒读取最多 1,000 个事件，满批时继续处理积压；已完成日志保留 24 小时，待处理日志不会按时间删除。具体一致性边界见[架构说明](architecture.md)。

## 复现实验

- [点赞计数 A/B 压测](../experiments/like-count/README.md)：隔离 MySQL、Redis、RabbitMQ，固定请求率、热点/分散两种分布，保留原始样本与源码哈希。
- [关注流组合验收](../experiments/feed-following/03-combined-flow-results.md)：发布、刷新、连续翻页与迟到响应回归。
- [关系存储布局实验](../experiments/edge-storage/README.md)：同一百万条关系比较主键及索引布局，仅用于方案评估。
