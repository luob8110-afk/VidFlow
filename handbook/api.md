# HTTP 接口

[返回项目首页](../README.md) · [本地运行](development.md)

## 接口清单

### 账号 `/account`
| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/register` | 否 | 注册（限流 5次/时/IP） |
| POST | `/login` | 否 | 登录，返回 access_token + refresh_token |
| POST | `/refresh` | 否 | 刷新 access_token（用 refresh_token） |
| POST | `/changePassword` | 否 | 改密码（需旧密码） |
| POST | `/findByID` | 否 | 按 ID 查用户 |
| POST | `/findByUsername` | 否 | 按用户名查 |
| POST | `/getProfile` | 否 | 用户主页（视频数/获赞/粉丝数） |
| POST | `/logout` | JWT | 登出（同时失效双 token） |
| POST | `/rename` | JWT | 改名 |
| POST | `/uploadAvatar` | JWT | 上传头像（jpg/png/webp，≤10MB） |
| POST | `/updateProfile` | JWT | 更新简介/头像 |

### 视频 `/video`
| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/publish` | JWT | 发布视频（自动提取 #话题），媒体模式下要求 `asset_id` |
| GET | `/assets/config` | 否 | 返回对象存储模式是否启用 |
| POST | `/assets` | JWT | 使用 `draft_id`、`size`、`sha256` 创建或恢复上传，返回已确认分片 |
| POST | `/assets/:id/parts/:part` | JWT | 签发指定分片的 PUT URL，编号从 1 开始 |
| POST | `/assets/:id/complete` | JWT | 幂等完成上传并排队 |
| GET | `/assets/:id` | JWT | 查询资产状态与处理结果 |
| DELETE | `/assets/:id` | JWT | 放弃上传中、失败或尚未发布的资产 |
| POST | `/uploadVideo` | JWT | 上传视频文件（mp4，≤200MB） |
| POST | `/uploadCover` | JWT | 上传封面（jpg/png/webp，≤10MB） |
| POST | `/chunk/init` | JWT | 初始化分片上传（文件 MD5、大小、分片数） |
| POST | `/chunk/upload` | JWT | 上传单个分片（multipart，含分片 MD5 校验） |
| POST | `/chunk/status` | JWT | 查询已上传分片 |
| POST | `/chunk/complete` | JWT | 合并分片并返回 play_url |
| POST | `/listByAuthorID` | 否 | 按作者查视频 |
| POST | `/getDetail` | 否 | 视频详情缓存 |

对象存储模式下，原本地上传与 `/chunk/*` 接口由媒体资产接口替代。播放产物通过 `GET /media/:id/play.mp4` 和 `GET /media/:id/cover.jpg` 跳转到对象存储，仅对已发布视频签发。完整流程见[视频生产与发布](media-pipeline.md)。

### 全站视频搜索 `/search/videos`

`POST /search/videos` 无需登录。传 `q`、`tag`、`author_id`、`published_from`、`published_before` 中至少一项。关键词匹配标题、简介和发布时的作者名。话题按完整名称匹配，可带 `#`。日期参数单位为 Unix 毫秒，起点包含在内，终点为开区间。`limit` 默认 20，允许 1–50。

响应的 `video_list` 包含视频 ID、作者 ID、用户名、标题、简介、封面和发布时间。继续翻页时传入 `next_cursor`，并沿用原检索条件及页大小。空数组表示没有命中或本页的索引命中已从数据库移除，可依据 `next_cursor` 是否存在决定是否继续。游标无效返回 400，快照过期返回 410，搜索未启用或暂不可用返回 503。

页面链接到现有视频详情与播放接口。[全文索引与故障验证](video-search.md)

### 点赞 `/like`
| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/like` | JWT | 点赞 |
| POST | `/unlike` | JWT | 取消点赞 |
| POST | `/isLiked` | JWT | 是否已赞 |
| POST | `/listMyLikedVideos` | JWT | 我赞过的视频 |

### 评论 `/comment`
| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/listAll` | 否 | 评论列表（分页200，按时间升序） |
| POST | `/publish` | JWT | 发布评论（支持 @username 提及） |
| POST | `/delete` | JWT | 删除评论 |

### 关注 `/social`
| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/follow` | JWT | 关注 |
| POST | `/unfollow` | JWT | 取关 |
| POST | `/getAllFollowers` | JWT | 粉丝列表（含粉丝数） |
| POST | `/getAllVloggers` | JWT | 关注列表（含关注数） |
| POST | `/getCounts` | JWT | 粉丝/关注计数 |

### Feed `/feed`
| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/listLatest` | 软鉴权 | 最新视频（游标分页） |
| POST | `/listLikesCount` | 软鉴权 | 点赞排行（复合游标） |
| POST | `/listByPopularity` | 软鉴权 | 热度榜（快照分页） |
| POST | `/listByFollowing` | JWT | 关注流 |
| POST | `/listByTag` | 软鉴权 | 按 #话题 浏览 |

### 通知 `/notification`
| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| GET | `/stream?token=<access_token>` | 是 | SSE 实时推送，也支持 `Authorization: Bearer <token>` |
| POST | `/list` | 是 | 最近 50 条通知 |
| POST | `/markRead` | 是 | 标记已读；传 `id` 标记单条，省略 `id` 标记全部 |
| POST | `/unreadCount` | 是 | 未读计数 |

### 私信 `/message`
| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| POST | `/send` | JWT | 发送私信 |
| POST | `/list` | JWT | 对话列表 |


## 关注流新游标协议

`POST /feed/listByFollowing` 使用 JWT，首屏或主动刷新传 `{"limit":10,"refresh":true}`；后续页传 `{"limit":10,"cursor":"<上一页 next_cursor>"}`。`cursor` 为不透明字符串，前端原样回传。`has_more=false` 时停止翻页，末页 `next_cursor` 为空。

`refresh` 要求 `cursor` 为空且 `latest_time=0`；`cursor` 与非零 `latest_time` 互斥。旧 `latest_time` 秒级协议继续兼容，新客户端使用复合 cursor。

## 点赞接口语义

同一个按钮按当前状态发送 `/like/like` 或 `/like/unlike`。请求表达目标状态，重复同一目标状态不重复新增关系、不重复产生计数事件。成功表示关系及恢复日志已经提交；Redis 尽力即时更新，数据库展示总数随后由 Worker 批量追平。榜单排序和作者主页总获赞允许这段延迟。
