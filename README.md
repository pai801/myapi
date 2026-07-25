<p align="right">
   <strong>中文</strong> | <a href="./README.en.md">English</a> | <a href="./README.ja.md">日本語</a>
</p>

> **Fork 声明**：本项目基于 [One API](https://github.com/songquanpeng/one-api) 修改而来，保留原始 MIT 许可证。

<p align="center">
  <a href="https://github.com/pai801/myapi"><img src="https://raw.githubusercontent.com/pai801/myapi/main/web/default/public/logo.png" width="150" height="150" alt="myapi logo"></a>
</p>

<div align="center">

# My API

_✨ 通过标准的 OpenAI API 格式访问所有的大模型，开箱即用 ✨_

**面向个人与小团队自用的 AI API 网关 —— 移除运营与充值模块，专注中继与路由。**

</div>

<p align="center">
  <a href="https://raw.githubusercontent.com/pai801/myapi/main/LICENSE">
    <img src="https://img.shields.io/github/license/pai801/myapi?color=brightgreen" alt="license">
  </a>
  <a href="https://github.com/pai801/myapi/releases/latest">
    <img src="https://img.shields.io/github/v/release/pai801/myapi?color=brightgreen&include_prereleases" alt="release">
  </a>
  <a href="https://hub.docker.com/repository/docker/pai801/myapi">
    <img src="https://img.shields.io/docker/pulls/pai801/myapi?color=brightgreen" alt="docker pull">
  </a>
  <a href="https://github.com/pai801/myapi/releases/latest">
    <img src="https://img.shields.io/github/downloads/pai801/myapi/total?color=brightgreen&include_prereleases" alt="release">
  </a>
  <a href="https://goreportcard.com/report/github.com/pai801/myapi">
    <img src="https://goreportcard.com/badge/github.com/pai801/myapi" alt="GoReportCard">
  </a>
</p>

<p align="center">
  <a href="#部署">部署教程</a> ·
  <a href="#使用方法">使用方法</a> ·
  <a href="#特色功能">特色功能</a> ·
  <a href="https://github.com/pai801/myapi/issues">意见反馈</a>
</p>

> [!NOTE]
> 本项目为开源项目，使用者必须在遵循 OpenAI 的[使用条款](https://openai.com/policies/terms-of-use)以及**法律法规**的情况下使用，不得用于非法用途。
>
> 根据[《生成式人工智能服务管理暂行办法》](http://www.cac.gov.cn/2023-07/13/c_1690898327029107.htm)的要求，请勿对中国地区公众提供一切未经备案的生成式人工智能服务。

> [!NOTE]
> 稳定版 / 预览版镜像地址：[pai801/myapi](https://hub.docker.com/repository/docker/pai801/myapi)
> 或者 [ghcr.io/pai801/myapi](https://github.com/pai801/myapi/pkgs/container/myapi)
>
> alpha 版镜像地址：[pai801/myapi-alpha](https://hub.docker.com/repository/docker/pai801/myapi-alpha)
> 或者 [ghcr.io/pai801/myapi-alpha](https://github.com/pai801/myapi/pkgs/container/myapi-alpha)

> [!WARNING]
> 使用 root 用户初次登录系统后，务必修改默认密码 `123456`！

---

## 与 One API 的差异

My API 在 One API 的基础上进行了大量重构和功能增强，以下是核心差异概览：

| 特性 | One API | My API |
|------|---------|--------|
| 分组维度 | 用户级别分组 | **令牌级别分组**，更细粒度的计费控制 |
| 渠道选择 | 简单负载均衡 | **亲和性路由 + 冷却机制 + 加权随机** |
| 失败重试 | 基础重试 | **语义级智能重试**，按错误类型决策 |
| 实时监控 | 无 | **SSE 实时推送**，流式请求全程可观测 |
| ChatGPT 订阅账号 | 不支持 | **ChatGPTSub 适配器**，含粘滞会话、熔断、健康探测 |
| OpenAI Codex | 不支持 | **Codex 适配器**，支持 Responses API 格式转换 |
| Responses API | 不支持 | 完整支持 `/v1/responses` 和 `/v1/responses/compact` |
| 透明代理 | 不支持 | **Proxy Relay 模式**，直透上游任意端点 |
| 模型元数据 | 无 | 丰富元数据：上下文窗口、模态、推理等级、工具类型 |
| 日志系统 | 基础日志 | **完整请求/响应体记录**，自动清理，独立日志库 |
| 令牌模型映射 | 无 | **令牌级模型名重写**，在分发前透明改写 |
| 运营与充值 | 包含 | **已移除**，面向个人/小团队自用，不需要运营模块 |

**定位差异**：One API 面向运营场景，包含充值、分销、邀请码等商业化模块。My API 定位是个人和小团队自用，移除了所有运营与充值相关功能，代码更精简，部署更轻量，专注于 API 中继、智能路由和可观测性。

---

## 特色功能

### 令牌级分组与灵活计费

分组概念从用户迁移至令牌——同一个用户可以签发属于不同分组的令牌，每个分组独立配置模型倍率（乘数模式）。计费公式为：

```
费用 = (输入 Token + 输出 Token × 输出倍率) × 模型倍率 × 分组倍率
```

管理员可通过 `/api/group/*` 接口进行分组的增删改查，分组删除前会校验是否仍有令牌或渠道引用，防止误删。默认分组倍率恒为 1.0，保证向后兼容。

### 智能渠道路由

渠道选择不再是简单的轮询或随机，而是三层策略协同工作：

**亲和性（Affinity）**：记住每个 `(用户, 模型)` 对上次成功使用的渠道，在 TTL（默认 300 秒）内优先复用同一渠道，保证多轮对话的上下文一致性。

**冷却机制（Cooldown）**：渠道失败后立即进入冷却期（默认 600 秒），冷却期间被排除在候选列表之外，避免将流量持续打入故障渠道。

**加权随机**：在候选渠道中按优先级权重进行加权随机分配，高优先级渠道获得更多流量。

三者协同的效果是：优先复用熟悉的渠道 → 故障后自动冷却隔离 → 在健康渠道中按权重分配。

### 语义级智能重试

重试决策基于对错误语义的深度分析，而非简单的状态码匹配：

- **触发重试**：429 限流、5xx 服务端错误、传输层失败、模型不兼容（`unsupported_model`）、上游响应格式异常
- **不触发重试**：请求格式错误（`invalid_request_error`）、输入参数非法、指定了固定渠道 ID 的请求

每次重试自动排除上一次失败的渠道，通过 `SelectChannel()` 选择新的候选渠道，避免重复踩坑。

### 实时流式请求监控（SSE）

管理后台通过 SSE（Server-Sent Events）实时接收所有进行中流式请求的状态推送：

```
GET /api/log/active/events
```

每个被追踪的请求包含：请求 ID、用户、令牌、模型、渠道、已耗时、请求体/请求头（敏感信息已脱敏）。事件类型覆盖 `start`、`update`、`end`、`complete`，其中 `complete` 事件携带完整数据库日志记录，前端可立即渲染展示。

内部采用 pub/sub 事件总线 + 有界缓冲（64），对慢消费者执行优雅丢弃，不影响主请求链路。TTL 清理循环与 `RELAY_TIMEOUT` 联动，默认 30 分钟兜底回收。

### ChatGPT 订阅账号适配器（ChatGPTSub）

将 ChatGPT 订阅账号作为渠道接入网关，参考 Sub2API 设计，支持完整的账号池化管理：

**粘滞会话**：通过 `conversation_id` / `session_hash` 将多轮对话绑定到同一订阅账号，TTL 1 小时，确保对话上下文连贯。

**熔断器**：基于 EWMA 算法跟踪每个账号的错误率。当错误率达到 50% 或连续失败 5 次时自动熔断该账号。后台探针在熔断后持续探测，连续 3 次成功后自动恢复。

**健康统计**：每个账号独立维护 EWMA 错误率和首 Token 时间（TTFT）指标。

**请求头白名单**：仅透传 8 个安全请求头，避免触发上游风控。

**双向格式转换**：Chat Completions 与 Responses API 格式的完整双向转换，包括流式 SSE 事件和工具调用。

### Codex 适配器与 Responses API

**Codex 适配器**：代理请求 OpenAI 的 Codex API（`chatgpt.com/backend-api`），支持 Chat Completions 和 Responses API 格式的自动转换，流式和非流式双模式。

**Responses API**：完整支持 OpenAI 的新一代 API 格式：

- `POST /v1/responses`：标准 Responses 端点
- `POST /v1/responses/compact`：紧凑模式
- 流式 SSE 事件严格保证终态语义：成功时恰好一个 `response.completed` + `[DONE]`；失败时恰好一个 `response.failed`，不会在失败后错误地发出 `completed`

**Proxy Relay 模式**：通过 `/v1/myapi/proxy/:channelid/*target` 将请求完整透传到指定渠道的上游 URL，用于访问供应商特有的非 OpenAI 兼容端点。

### 模型元数据系统

每个模型可存储丰富的元数据，并在 `/v1/models` 接口中返回：

| 字段 | 说明 |
|------|------|
| `display_name` | 展示名称 |
| `context_window` | 上下文窗口大小 |
| `max_output_tokens` | 最大输出 Token |
| `input_modalities` / `output_modalities` | 输入/输出模态（text, image, audio...） |
| `supported_reasoning_levels` | 支持的推理等级 |
| `supported_endpoint_types` | 支持的端点类型 |
| `truncation_policy` | 截断策略 |
| `web_search_tool_type` | 网页搜索工具类型 |
| `apply_patch_tool_type` | Patch 应用工具类型 |

未知模型自动生成默认元数据，管理员可手动编辑覆盖。

### 增强日志系统

日志不再只是"谁在什么时候调了什么模型"，而是完整的请求审计链路：

- **请求体 / 响应体 / 请求头**：完整记录，存储为 TEXT 字段，敏感信息（Authorization 等）自动脱敏
- **列表查询优化**：列表接口使用布尔标志（`has_request_body` 等）代替传输大字段，详情通过独立接口获取
- **自动清理**：后台每小时执行清理，日志保留期默认 168 小时（7 天），请求/响应体保留期默认 4 小时
- **独立日志库**：通过 `LOG_SQL_DSN` 将日志表分离到独立数据库，避免高频写入影响主业务库
- **缓存 Token 追踪**：记录 prompt caching 命中量，便于分析成本优化效果

### 令牌级模型映射

在令牌维度配置模型名重写规则（JSON 格式），在渠道分发之前透明地将用户请求的模型名映射为实际模型名。这允许不同令牌持有者使用自定义的模型别名，而不影响渠道层面的模型配置。

### 渠道自动管理

**智能禁用**：当渠道返回 401 未授权、余额不足、API Key 无效、账号被封禁等错误时自动禁用，无需人工干预。

**指标驱动禁用**：滑动窗口跟踪渠道请求成功率，当成功率低于阈值（默认 80%）时自动禁用。

**自动恢复**：定期测试被禁用的渠道，测试通过后自动重新启用。

**响应时间阈值**：超过配置的响应时间上限的渠道自动禁用。

---

## 支持的模型渠道

支持 **56 种渠道类型**，**21 个 API 适配器**，覆盖主流国内外大模型供应商：

**国际供应商**：OpenAI ChatGPT 系列（含 Azure OpenAI）、Anthropic Claude 系列（含 AWS Claude）、Google PaLM2/Gemini/Vertex AI、Mistral、Cohere、xAI、Groq、together.ai、Cloudflare Workers AI、DeepL、Ollama、Replicate、OpenRouter

**国内供应商**：百度文心一言、阿里通义千问（含阿里百炼）、讯飞星火、智谱 ChatGLM、360 智脑、腾讯混元、Moonshot、百川大模型、MiniMax、字节跳动豆包（火山引擎）、零一万物、阶跃星辰、DeepSeek、硅基流动 SiliconCloud、Coze、novita.ai

**特殊渠道**：Codex（OpenAI 后端 API 代理）、ChatGPTSub（ChatGPT 订阅账号池化）、Proxy（通用上游透传）

---

## 部署

### 基于 Docker 进行部署

```shell
# 使用 SQLite 的部署命令：
docker run --name myapi -d --restart always -p 3000:3000 -e TZ=Asia/Shanghai -v /home/ubuntu/data/myapi:/data pai801/myapi

# 使用 MySQL 的部署命令，在上面的基础上添加 -e SQL_DSN="root:123456@tcp(localhost:3306)/myapi"
# 请自行修改数据库连接参数，不清楚如何修改请参见下面环境变量一节。
docker run --name myapi -d --restart always -p 3000:3000 -e SQL_DSN="root:123456@tcp(localhost:3306)/myapi" -e TZ=Asia/Shanghai -v /home/ubuntu/data/myapi:/data pai801/myapi
```

其中，`-p 3000:3000` 中的第一个 `3000` 是宿主机的端口，可以根据需要进行修改。

数据和日志将会保存在宿主机的 `/home/ubuntu/data/myapi` 目录，请确保该目录存在且具有写入权限，或者更改为合适的目录。

如果启动失败，请添加 `--privileged=true`，具体参考 https://github.com/pai801/myapi/issues/482 。

如果上面的镜像无法拉取，可以尝试使用 GitHub 的 Docker 镜像，将上面的 `pai801/myapi` 替换为 `ghcr.io/pai801/myapi` 即可。

如果你的并发量较高，**务必**设置 `SQL_DSN`，详见下面[环境变量](#环境变量)一节。

更新命令：`docker run --rm -v /var/run/docker.sock:/var/run/docker.sock containrrr/watchtower -cR`

Nginx 的参考配置：

```nginx
server {
   server_name your-domain.com;  # 请根据实际情况修改你的域名

   location / {
          client_max_body_size  64m;
          proxy_http_version 1.1;
          proxy_pass http://localhost:3000;  # 请根据实际情况修改你的端口
          proxy_set_header Host $host;
          proxy_set_header X-Forwarded-For $remote_addr;
          proxy_cache_bypass $http_upgrade;
          proxy_set_header Accept-Encoding gzip;
          proxy_read_timeout 300s;  # GPT-4 需要较长的超时时间，请自行调整
   }
}
```

之后使用 Let's Encrypt 的 certbot 配置 HTTPS：

```bash
# Ubuntu 安装 certbot：
sudo snap install --classic certbot
sudo ln -s /snap/bin/certbot /usr/bin/certbot
# 生成证书 & 修改 Nginx 配置
sudo certbot --nginx
# 根据指示进行操作
# 重启 Nginx
sudo service nginx restart
```

初始账号用户名为 `root`，密码为 `123456`。

### 基于 Docker Compose 进行部署

> 仅启动方式不同，参数设置不变，请参考基于 Docker 部署部分

```shell
# 目前支持 MySQL 启动，数据存储在 ./data/mysql 文件夹内
docker-compose up -d

# 查看部署状态
docker-compose ps
```

### 手动部署

1. 从 [GitHub Releases](https://github.com/pai801/myapi/releases/latest) 下载可执行文件或者从源码编译：

   ```shell
   git clone https://github.com/pai801/myapi.git

   # 构建前端
   cd myapi/web/default
   npm install
   npm run build

   # 构建后端
   cd ../..
   go mod download
   go build -ldflags "-s -w" -o myapi
   ```

2. 运行：

   ```shell
   chmod u+x myapi
   ./myapi --port 3000 --log-dir ./logs
   ```

3. 访问 [http://localhost:3000/](http://localhost:3000/) 并登录。初始账号用户名为 `root`，密码为 `123456`。

---

## 使用方法

在`渠道`页面中添加你的 API Key，之后在`令牌`页面中新增访问令牌。

之后就可以使用你的令牌访问 My API 了，使用方式与 [OpenAI API](https://platform.openai.com/docs/api-reference/introduction) 一致。

你需要在各种用到 OpenAI API 的地方设置 API Base 为你的 My API 的部署地址，例如 `https://your-domain.com`，API Key 则为你在 My API 中生成的令牌。

注意，具体的 API Base 的格式取决于你所使用的客户端。

例如对于 OpenAI 的官方库：

```bash
OPENAI_API_KEY="sk-xxxxxx"
OPENAI_API_BASE="https://<HOST>:<PORT>/v1"
```

```mermaid
graph LR
    A(用户)
    A --->|使用 My API 分发的 key 进行请求| B(My API)
    B -->|中继请求| C(OpenAI)
    B -->|中继请求| D(Azure)
    B -->|中继请求| E(其他 OpenAI API 格式下游渠道)
    B -->|中继并修改请求体和返回体| F(非 OpenAI API 格式下游渠道)
```

可以通过在令牌后面添加渠道 ID 的方式指定使用哪一个渠道处理本次请求，例如：`Authorization: Bearer MY_API_KEY-CHANNEL_ID`。
注意，需要是管理员用户创建的令牌才能指定渠道 ID。

不加的话将会使用负载均衡的方式使用多个渠道。

---

## 配置

系统本身开箱即用。你可以通过设置环境变量或者命令行参数进行配置。

等到系统启动后，使用 `root` 用户登录系统并做进一步的配置。

**Note**：如果你不知道某个配置项的含义，可以临时删掉值以看到进一步的提示文字。

### 环境变量

> My API 支持从 `.env` 文件中读取环境变量，请参照 `.env.example` 文件，使用时请将其重命名为 `.env`。

**数据库与缓存：**

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `SQL_DSN` | 使用 MySQL 或 PostgreSQL 替代 SQLite | 无（使用 SQLite） |
| `LOG_SQL_DSN` | 日志表独立数据库 | 无（与主库共用） |
| `SQL_MAX_IDLE_CONNS` | 最大空闲连接数 | `100` |
| `SQL_MAX_OPEN_CONNS` | 最大打开连接数 | `1000` |
| `SQL_CONN_MAX_LIFETIME` | 连接最大生命周期（分钟） | `60` |
| `SQLITE_BUSY_TIMEOUT` | SQLite 锁等待超时（毫秒） | `3000` |
| `REDIS_CONN_STRING` | Redis 连接字符串（用作缓存层） | 无 |
| `REDIS_PASSWORD` | Redis 集群/哨兵模式密码 | 无 |
| `REDIS_MASTER_NAME` | Redis 哨兵模式主节点名称 | 无 |
| `MEMORY_CACHE_ENABLED` | 启用内存缓存 | `false` |
| `BATCH_UPDATE_ENABLED` | 启用数据库批量更新聚合 | `false` |
| `BATCH_UPDATE_INTERVAL` | 批量更新间隔（秒） | `5` |
| `SYNC_FREQUENCY` | 缓存与数据库同步频率（秒） | `600` |

**渠道与路由：**

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `CHANNEL_UPDATE_FREQUENCY` | 定期更新渠道余额（分钟） | 无（不更新） |
| `CHANNEL_TEST_FREQUENCY` | 定期检查渠道可用性（分钟） | 无（不检查） |
| `CHANNEL_COOLDOWN_SECONDS` | 渠道失败后冷却期（秒） | `600` |
| `AFFINITY_EXPIRE_SECONDS` | 用户-模型-渠道亲和性 TTL（秒） | `300` |
| `POLLING_INTERVAL` | 批量更新/测试时的请求间隔（秒） | 无 |
| `ENABLE_METRIC` | 启用成功率驱动的渠道自动禁用 | `false` |
| `METRIC_QUEUE_SIZE` | 成功率统计队列大小 | `10` |
| `METRIC_SUCCESS_RATE_THRESHOLD` | 成功率阈值 | `0.8` |

**中继与代理：**

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `RELAY_TIMEOUT` | 中继超时（秒） | 无 |
| `RELAY_PROXY` | 中继请求代理地址 | 无 |
| `ENFORCE_INCLUDE_USAGE` | 强制流式响应返回 usage | `false` |
| `GEMINI_SAFETY_SETTING` | Gemini 安全设置 | `BLOCK_NONE` |
| `GEMINI_VERSION` | Gemini API 版本 | `v1` |

**日志：**

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `LOG_CLEAN_HOURS` | 日志保留时长（小时） | `168` |
| `LOG_CLEAN_BODIES_HOURS` | 请求/响应体保留时长（小时） | `4` |
| `MAX_LOGGED_BODY_SIZE` | 最大记录请求体大小（字节） | `2097152`（2MB） |

**安全与限流：**

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `SESSION_SECRET` | 固定会话密钥（重启后 cookie 仍有效） | 无 |
| `GLOBAL_API_RATE_LIMIT` | 单 IP 三分钟最大 API 请求数 | `480` |
| `GLOBAL_WEB_RATE_LIMIT` | 单 IP 三分钟最大 Web 请求数 | `240` |

**其他：**

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `FRONTEND_BASE_URL` | 前端页面重定向地址 | 无 |
| `INITIAL_ROOT_TOKEN` | 首次启动时自动创建的 root 令牌值 | 无 |
| `INITIAL_ROOT_ACCESS_TOKEN` | 首次启动时自动创建的系统管理令牌值 | 无 |
| `TEST_PROMPT` | 测试模型时的用户 prompt | `Print your model name exactly...` |
| `TIKTOKEN_CACHE_DIR` | Tokenizer 编码缓存目录 | 无 |
| `USER_CONTENT_REQUEST_TIMEOUT` | 用户上传内容下载超时（秒） | 无 |
| `USER_CONTENT_REQUEST_PROXY` | 用户内容请求代理 | 无 |

### 命令行参数

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `--port <port>` | 服务器监听端口 | `3000` |
| `--log-dir <dir>` | 日志目录 | `./logs` |
| `--version` | 打印版本号并退出 | - |
| `--help` | 查看帮助 | - |

---

## 注意

本项目基于 One API (MIT) 进行二次开发，保留 MIT 许可证。
