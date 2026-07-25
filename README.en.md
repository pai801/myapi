<p align="right">
   <a href="./README.md">中文</a> | <strong>English</strong> | <a href="./README.ja.md">日本語</a>
</p>

> **Fork Notice**: This project is modified from [One API](https://github.com/songquanpeng/one-api) and retains the original MIT License.

<p align="center">
  <a href="https://github.com/pai801/myapi"><img src="https://raw.githubusercontent.com/pai801/myapi/main/web/default/public/logo.png" width="150" height="150" alt="myapi logo"></a>
</p>

<div align="center">

# My API

_✨ Access all LLMs through the standard OpenAI API format, ready to use out of the box ✨_

**An AI API gateway for individuals and small teams — operational and recharge modules removed, focused on relay and routing.**

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
  <a href="#deployment">Deployment</a> ·
  <a href="#usage">Usage</a> ·
  <a href="#featured-features">Featured Features</a> ·
  <a href="https://github.com/pai801/myapi/issues">Feedback</a>
</p>

> [!NOTE]
> This project is open source. Users must comply with OpenAI's [Terms of Use](https://openai.com/policies/terms-of-use) and all applicable **laws and regulations**, and must not use it for illegal purposes.
>
> Per the requirements of the [Interim Measures for the Management of Generative Artificial Intelligence Services](http://www.cac.gov.cn/2023-07/13/c_1690898327029107.htm), do not provide any unregistered generative AI services to the public in China.

> [!NOTE]
> Stable / preview image: [pai801/myapi](https://hub.docker.com/repository/docker/pai801/myapi)
> or [ghcr.io/pai801/myapi](https://github.com/pai801/myapi/pkgs/container/myapi)
>
> Alpha image: [pai801/myapi-alpha](https://hub.docker.com/repository/docker/pai801/myapi-alpha)
> or [ghcr.io/pai801/myapi-alpha](https://github.com/pai801/myapi/pkgs/container/myapi-alpha)

> [!WARNING]
> After logging into the system for the first time as the `root` user, be sure to change the default password `123456`!

---

## Differences from One API

My API has undergone extensive refactoring and feature enhancements on top of One API. Here is an overview of the core differences:

| Feature | One API | My API |
|---------|---------|--------|
| Grouping | User-level grouping | **Token-level grouping** for finer-grained billing control |
| Channel Selection | Simple load balancing | **Affinity routing + cooldown + weighted random** |
| Failure Retry | Basic retry | **Semantic-level intelligent retry** with error-type-aware decisions |
| Real-time Monitoring | None | **SSE real-time push**, full observability for streaming requests |
| ChatGPT Subscription Accounts | Not supported | **ChatGPTSub adapter** with sticky sessions, circuit breaking, and health probes |
| OpenAI Codex | Not supported | **Codex adapter** with Responses API format conversion |
| Responses API | Not supported | Full support for `/v1/responses` and `/v1/responses/compact` |
| Transparent Proxy | Not supported | **Proxy Relay mode**, pass-through to any upstream endpoint |
| Model Metadata | None | Rich metadata: context window, modalities, reasoning levels, tool types |
| Logging | Basic logging | **Full request/response body recording**, auto-cleanup, separate log database |
| Token Model Mapping | None | **Token-level model name rewriting**, transparently remapped before dispatch |
| Operations & Recharge | Included | **Removed** — designed for personal/small-team use, no operational modules needed |

**Positioning**: One API targets operational scenarios and includes commercialization modules such as recharge, reselling, and invitation codes. My API is designed for personal and small-team use — all operational and recharge features have been removed, resulting in leaner code, lighter deployment, and a focus on API relay, intelligent routing, and observability.

---

## Featured Features

### Token-Level Grouping & Flexible Billing

The grouping concept has moved from users to tokens — a single user can issue tokens belonging to different groups, each with independently configured model multipliers (multiplier mode). The billing formula is:

```
Cost = (Input Tokens + Output Tokens × Output Multiplier) × Model Multiplier × Group Multiplier
```

Administrators can manage groups via the `/api/group/*` API endpoints (CRUD). Group deletion is validated to ensure no tokens or channels still reference it, preventing accidental removal. The default group multiplier is always 1.0, ensuring backward compatibility.

### Intelligent Channel Routing

Channel selection is no longer simple round-robin or random — three strategies work together:

**Affinity**: Remembers the last successful channel for each `(user, model)` pair and preferentially reuses it within a TTL (default 300 seconds), ensuring context consistency across multi-turn conversations.

**Cooldown**: After a channel failure, it enters a cooldown period (default 600 seconds) during which it is excluded from the candidate list, preventing traffic from being routed to a faulty channel.

**Weighted Random**: Among candidate channels, weighted random selection based on priority ensures higher-priority channels receive more traffic.

The combined effect: prefer reuse of familiar channels → automatically isolate failed channels via cooldown → distribute among healthy channels by weight.

### Semantic-Level Intelligent Retry

Retry decisions are based on deep analysis of error semantics, not simple status code matching:

- **Triggers retry**: 429 rate limiting, 5xx server errors, transport-layer failures, model incompatibility (`unsupported_model`), abnormal upstream response format
- **Does not trigger retry**: Malformed requests (`invalid_request_error`), invalid input parameters, requests pinned to a specific channel ID

Each retry automatically excludes the previously failed channel and selects a new candidate via `SelectChannel()`, avoiding repeated failures.

### Real-Time Streaming Request Monitoring (SSE)

The admin dashboard receives real-time status updates for all in-progress streaming requests via SSE (Server-Sent Events):

```
GET /api/log/active/events
```

Each tracked request includes: request ID, user, token, model, channel, elapsed time, and request body/headers (sensitive information redacted). Event types cover `start`, `update`, `end`, and `complete`, where the `complete` event carries the full database log record for immediate frontend rendering.

Internally uses a pub/sub event bus with bounded buffering (64), gracefully dropping events for slow consumers without impacting the main request path. TTL cleanup cycles are linked to `RELAY_TIMEOUT`, with a default 30-minute fallback cleanup.

### ChatGPT Subscription Account Adapter (ChatGPTSub)

Integrates ChatGPT subscription accounts as gateway channels, designed with reference to Sub2API, supporting full account pool management:

**Sticky Sessions**: Binds multi-turn conversations to the same subscription account via `conversation_id` / `session_hash` with a 1-hour TTL, ensuring conversation context continuity.

**Circuit Breaker**: Tracks each account's error rate using the EWMA algorithm. When the error rate reaches 50% or 5 consecutive failures occur, the account is automatically circuit-broken. Background probes continuously test broken accounts and automatically recover after 3 consecutive successes.

**Health Statistics**: Each account independently maintains EWMA error rate and Time to First Token (TTFT) metrics.

**Header Whitelist**: Only forwards 8 safe request headers to avoid triggering upstream risk controls.

**Bidirectional Format Conversion**: Full bidirectional conversion between Chat Completions and Responses API formats, including streaming SSE events and tool calls.

### Codex Adapter & Responses API

**Codex Adapter**: Proxies requests to OpenAI's Codex API (`chatgpt.com/backend-api`), supporting automatic conversion between Chat Completions and Responses API formats in both streaming and non-streaming modes.

**Responses API**: Full support for OpenAI's next-generation API format:

- `POST /v1/responses`: Standard Responses endpoint
- `POST /v1/responses/compact`: Compact mode
- Streaming SSE events strictly guarantee terminal semantics: exactly one `response.completed` + `[DONE]` on success; exactly one `response.failed` on failure — never erroneously emitting `completed` after a failure

**Proxy Relay Mode**: Via `/v1/myapi/proxy/:channelid/*target`, forwards requests in full to the specified channel's upstream URL, for accessing vendor-specific non-OpenAI-compatible endpoints.

### Model Metadata System

Each model can store rich metadata, returned in the `/v1/models` endpoint:

| Field | Description |
|-------|-------------|
| `display_name` | Display name |
| `context_window` | Context window size |
| `max_output_tokens` | Maximum output tokens |
| `input_modalities` / `output_modalities` | Input/output modalities (text, image, audio...) |
| `supported_reasoning_levels` | Supported reasoning levels |
| `supported_endpoint_types` | Supported endpoint types |
| `truncation_policy` | Truncation policy |
| `web_search_tool_type` | Web search tool type |
| `apply_patch_tool_type` | Patch application tool type |

Default metadata is auto-generated for unknown models; administrators can manually edit to override.

### Enhanced Logging System

Logging goes beyond "who called what model when" to provide a complete request audit trail:

- **Request Body / Response Body / Headers**: Fully recorded, stored as TEXT fields, with sensitive information (Authorization, etc.) automatically redacted
- **List Query Optimization**: List endpoints use boolean flags (`has_request_body`, etc.) instead of transferring large fields; details are retrieved via a separate endpoint
- **Auto-Cleanup**: Background cleanup runs every hour; log retention defaults to 168 hours (7 days), request/response body retention defaults to 4 hours
- **Separate Log Database**: Use `LOG_SQL_DSN` to separate the log tables into an independent database, avoiding high-frequency writes impacting the main business database
- **Cache Token Tracking**: Records prompt caching hit counts for cost optimization analysis

### Token-Level Model Mapping

Configure model name rewriting rules (in JSON format) at the token level, transparently mapping user-requested model names to actual model names before channel dispatch. This allows different token holders to use custom model aliases without affecting channel-level model configuration.

### Automatic Channel Management

**Smart Disabling**: Channels are automatically disabled when they return errors such as 401 unauthorized, insufficient balance, invalid API key, or banned account — no manual intervention needed.

**Metric-Driven Disabling**: Sliding window tracks channel request success rate; channels are automatically disabled when the success rate drops below the threshold (default 80%).

**Auto-Recovery**: Periodically tests disabled channels and automatically re-enables them after passing tests.

**Response Time Threshold**: Channels exceeding the configured response time limit are automatically disabled.

---

## Supported Model Channels

Supports **56 channel types** and **21 API adapters**, covering major domestic and international LLM providers:

**International Providers**: OpenAI ChatGPT series (including Azure OpenAI), Anthropic Claude series (including AWS Claude), Google PaLM2/Gemini/Vertex AI, Mistral, Cohere, xAI, Groq, together.ai, Cloudflare Workers AI, DeepL, Ollama, Replicate, OpenRouter

**Chinese Providers**: Baidu Wenxin Yiyan, Alibaba Tongyi Qianwen (including Alibaba Bailian), iFlytek Spark, Zhipu ChatGLM, 360 Brain, Tencent Hunyuan, Moonshot, Baichuan, MiniMax, ByteDance Doubao (Volcengine), 01.AI, StepFun, DeepSeek, SiliconFlow, Coze, novita.ai

**Special Channels**: Codex (OpenAI backend API proxy), ChatGPTSub (ChatGPT subscription account pooling), Proxy (generic upstream pass-through)

---

## Deployment

### Docker Deployment

```shell
# Deployment with SQLite:
docker run --name myapi -d --restart always -p 3000:3000 -e TZ=Asia/Shanghai -v /home/ubuntu/data/myapi:/data pai801/myapi

# Deployment with MySQL — add -e SQL_DSN="root:123456@tcp(localhost:3306)/myapi" to the command above.
# Modify the database connection parameters as needed; see the Environment Variables section below for details.
docker run --name myapi -d --restart always -p 3000:3000 -e SQL_DSN="root:123456@tcp(localhost:3306)/myapi" -e TZ=Asia/Shanghai -v /home/ubuntu/data/myapi:/data pai801/myapi
```

The first `3000` in `-p 3000:3000` is the host port, which can be modified as needed.

Data and logs will be saved to `/home/ubuntu/data/myapi` on the host. Ensure this directory exists and has write permissions, or change it to a suitable directory.

If startup fails, add `--privileged=true`. See https://github.com/pai801/myapi/issues/482 for details.

If the image cannot be pulled, try using the GitHub Docker image by replacing `pai801/myapi` with `ghcr.io/pai801/myapi`.

If you have high concurrency, it is **strongly recommended** to set `SQL_DSN`. See the [Environment Variables](#environment-variables) section below for details.

Update command: `docker run --rm -v /var/run/docker.sock:/var/run/docker.sock containrrr/watchtower -cR`

Nginx reference configuration:

```nginx
server {
   server_name your-domain.com;  # Modify your domain name accordingly

   location / {
          client_max_body_size  64m;
          proxy_http_version 1.1;
          proxy_pass http://localhost:3000;  # Modify your port accordingly
          proxy_set_header Host $host;
          proxy_set_header X-Forwarded-For $remote_addr;
          proxy_cache_bypass $http_upgrade;
          proxy_set_header Accept-Encoding gzip;
          proxy_read_timeout 300s;  # GPT-4 requires a longer timeout; adjust as needed
   }
}
```

Then configure HTTPS using Let's Encrypt certbot:

```bash
# Install certbot on Ubuntu:
sudo snap install --classic certbot
sudo ln -s /snap/bin/certbot /usr/bin/certbot
# Generate certificates & modify Nginx configuration
sudo certbot --nginx
# Follow the prompts
# Restart Nginx
sudo service nginx restart
```

The initial account username is `root` and password is `123456`.

### Docker Compose Deployment

> Only the startup method differs; parameter settings remain the same. See the Docker Deployment section above.

```shell
# Currently supports MySQL startup; data is stored in the ./data/mysql directory
docker-compose up -d

# Check deployment status
docker-compose ps
```

### Manual Deployment

1. Download the executable from [GitHub Releases](https://github.com/pai801/myapi/releases/latest) or build from source:

   ```shell
   git clone https://github.com/pai801/myapi.git

   # Build the frontend
   cd myapi/web/default
   npm install
   npm run build

   # Build the backend
   cd ../..
   go mod download
   go build -ldflags "-s -w" -o myapi
   ```

2. Run:

   ```shell
   chmod u+x myapi
   ./myapi --port 3000 --log-dir ./logs
   ```

3. Visit [http://localhost:3000/](http://localhost:3000/) and log in. The initial account username is `root` and password is `123456`.

---

## Usage

Add your API Key on the `Channels` page, then create an access token on the `Tokens` page.

You can then use your token to access My API in the same way as the [OpenAI API](https://platform.openai.com/docs/api-reference/introduction).

In any place where you use the OpenAI API, set the API Base to your My API deployment address, e.g. `https://your-domain.com`, and set the API Key to the token generated in My API.

Note that the exact API Base format depends on the client you are using.

For example, with the official OpenAI library:

```bash
OPENAI_API_KEY="sk-xxxxxx"
OPENAI_API_BASE="https://<HOST>:<PORT>/v1"
```

```mermaid
graph LR
    A(User)
    A --->|Request using a key issued by My API| B(My API)
    B -->|Relay request| C(OpenAI)
    B -->|Relay request| D(Azure)
    B -->|Relay request| E(Other OpenAI API format downstream channels)
    B -->|Relay and transform request/response bodies| F(Non-OpenAI API format downstream channels)
```

You can specify which channel to use for the current request by appending the channel ID after the token, e.g.: `Authorization: Bearer MY_API_KEY-CHANNEL_ID`.
Note that only tokens created by an administrator can specify a channel ID.

If no channel ID is provided, load balancing will be used to distribute requests across multiple channels.

---

## Configuration

The system is ready to use out of the box. You can configure it by setting environment variables or command line parameters.

After the system starts, log in as the `root` user for further configuration.

**Note**: If you are unsure what a configuration item does, you can temporarily remove its value to see a descriptive hint.

### Environment Variables

> My API supports reading environment variables from a `.env` file. Refer to the `.env.example` file and rename it to `.env` when in use.

**Database & Caching:**

| Variable | Description | Default |
|----------|-------------|---------|
| `SQL_DSN` | Use MySQL or PostgreSQL instead of SQLite | None (uses SQLite) |
| `LOG_SQL_DSN` | Separate database for log tables | None (shares main DB) |
| `SQL_MAX_IDLE_CONNS` | Maximum idle connections | `100` |
| `SQL_MAX_OPEN_CONNS` | Maximum open connections | `1000` |
| `SQL_CONN_MAX_LIFETIME` | Connection max lifetime (minutes) | `60` |
| `SQLITE_BUSY_TIMEOUT` | SQLite lock wait timeout (milliseconds) | `3000` |
| `REDIS_CONN_STRING` | Redis connection string (used as cache layer) | None |
| `REDIS_PASSWORD` | Redis cluster/sentinel mode password | None |
| `REDIS_MASTER_NAME` | Redis sentinel mode master node name | None |
| `MEMORY_CACHE_ENABLED` | Enable memory cache | `false` |
| `BATCH_UPDATE_ENABLED` | Enable batched database update aggregation | `false` |
| `BATCH_UPDATE_INTERVAL` | Batch update interval (seconds) | `5` |
| `SYNC_FREQUENCY` | Cache-to-database sync frequency (seconds) | `600` |

**Channels & Routing:**

| Variable | Description | Default |
|----------|-------------|---------|
| `CHANNEL_UPDATE_FREQUENCY` | Periodically update channel balances (minutes) | None (no updates) |
| `CHANNEL_TEST_FREQUENCY` | Periodically test channel availability (minutes) | None (no tests) |
| `CHANNEL_COOLDOWN_SECONDS` | Channel cooldown period after failure (seconds) | `600` |
| `AFFINITY_EXPIRE_SECONDS` | User-model-channel affinity TTL (seconds) | `300` |
| `POLLING_INTERVAL` | Request interval during batch updates/tests (seconds) | None |
| `ENABLE_METRIC` | Enable success-rate-driven channel auto-disabling | `false` |
| `METRIC_QUEUE_SIZE` | Success rate statistics queue size | `10` |
| `METRIC_SUCCESS_RATE_THRESHOLD` | Success rate threshold | `0.8` |

**Relay & Proxy:**

| Variable | Description | Default |
|----------|-------------|---------|
| `RELAY_TIMEOUT` | Relay timeout (seconds) | None |
| `RELAY_PROXY` | Proxy address for relay requests | None |
| `ENFORCE_INCLUDE_USAGE` | Force streaming responses to include usage | `false` |
| `GEMINI_SAFETY_SETTING` | Gemini safety setting | `BLOCK_NONE` |
| `GEMINI_VERSION` | Gemini API version | `v1` |

**Logging:**

| Variable | Description | Default |
|----------|-------------|---------|
| `LOG_CLEAN_HOURS` | Log retention duration (hours) | `168` |
| `LOG_CLEAN_BODIES_HOURS` | Request/response body retention duration (hours) | `4` |
| `MAX_LOGGED_BODY_SIZE` | Maximum logged request body size (bytes) | `2097152` (2MB) |

**Security & Rate Limiting:**

| Variable | Description | Default |
|----------|-------------|---------|
| `SESSION_SECRET` | Fixed session secret (keeps cookies valid after restart) | None |
| `GLOBAL_API_RATE_LIMIT` | Max API requests per IP per 3 minutes | `480` |
| `GLOBAL_WEB_RATE_LIMIT` | Max web requests per IP per 3 minutes | `240` |

**Other:**

| Variable | Description | Default |
|----------|-------------|---------|
| `FRONTEND_BASE_URL` | Frontend page redirect URL | None |
| `INITIAL_ROOT_TOKEN` | Root token value auto-created on first launch | None |
| `INITIAL_ROOT_ACCESS_TOKEN` | System admin token value auto-created on first launch | None |
| `TEST_PROMPT` | User prompt used when testing models | `Print your model name exactly...` |
| `TIKTOKEN_CACHE_DIR` | Tokenizer encoding cache directory | None |
| `USER_CONTENT_REQUEST_TIMEOUT` | User-uploaded content download timeout (seconds) | None |
| `USER_CONTENT_REQUEST_PROXY` | User content request proxy | None |

### Command Line Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `--port <port>` | Server listen port | `3000` |
| `--log-dir <dir>` | Log directory | `./logs` |
| `--version` | Print version and exit | - |
| `--help` | Show help | - |

---

## Note

This project is a secondary development based on One API (MIT) and retains the MIT License.
