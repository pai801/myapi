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
  <a href="https://github.com/pai801/myapi#部署">部署教程</a>
  <a href="https://github.com/pai801/myapi#使用方法">使用方法</a>
  ·
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

## 功能
1. 支持多种大模型：
   + [x] [OpenAI ChatGPT 系列模型](https://platform.openai.com/docs/guides/gpt/chat-completions-api)（支持 [Azure OpenAI API](https://learn.microsoft.com/en-us/azure/ai-services/openai/reference)）
   + [x] [Anthropic Claude 系列模型](https://anthropic.com) (支持 AWS Claude)
   + [x] [Google PaLM2/Gemini 系列模型](https://developers.generativeai.google)
   + [x] [Mistral 系列模型](https://mistral.ai/)
   + [x] [字节跳动豆包大模型（火山引擎）](https://www.volcengine.com/experience/ark?utm_term=202502dsinvite&ac=DSASUQY5&rc=2QXCA1VI)
   + [x] [百度文心一言系列模型](https://cloud.baidu.com/doc/WENXINWORKSHOP/index.html)
   + [x] [阿里通义千问系列模型](https://help.aliyun.com/document_detail/2400395.html)
   + [x] [讯飞星火认知大模型](https://www.xfyun.cn/doc/spark/Web.html)
   + [x] [智谱 ChatGLM 系列模型](https://bigmodel.cn)
   + [x] [360 智脑](https://ai.360.cn)
   + [x] [腾讯混元大模型](https://cloud.tencent.com/document/product/1729)
   + [x] [Moonshot AI](https://platform.moonshot.cn/)
   + [x] [百川大模型](https://platform.baichuan-ai.com)
   + [x] [MINIMAX](https://api.minimax.chat/)
   + [x] [Groq](https://wow.groq.com/)
   + [x] [Ollama](https://github.com/ollama/ollama)
   + [x] [零一万物](https://platform.lingyiwanwu.com/)
   + [x] [阶跃星辰](https://platform.stepfun.com/)
   + [x] [Coze](https://www.coze.com/)
   + [x] [Cohere](https://cohere.com/)
   + [x] [DeepSeek](https://www.deepseek.com/)
   + [x] [Cloudflare Workers AI](https://developers.cloudflare.com/workers-ai/)
   + [x] [DeepL](https://www.deepl.com/)
   + [x] [together.ai](https://www.together.ai/)
   + [x] [novita.ai](https://www.novita.ai/)
   + [x] [硅基流动 SiliconCloud](https://cloud.siliconflow.cn/i/rKXmRobW)
   + [x] [xAI](https://x.ai/)
2. 支持配置镜像以及第三方代理服务。
3. 支持通过**负载均衡**的方式访问多个渠道。
4. 支持 **stream 模式**，可以通过流式传输实现打字机效果。
5. 支持**令牌管理**，设置允许的 IP 范围以及允许的模型访问。
6. 支持**渠道管理**，批量创建渠道。
7. 支持**用户分组**以及**渠道分组**。
8. 支持渠道**设置模型列表**。
9. 支持**查看额度明细**。
10. 支持模型映射，重定向用户的请求模型，如无必要请不要设置，设置之后会导致请求体被重新构造而非直接透传，会导致部分还未正式支持的字段无法传递成功。
11. 支持失败自动重试。
12. 支持绘图接口。
13. 支持 [Cloudflare AI Gateway](https://developers.cloudflare.com/ai-gateway/providers/openai/)，渠道设置的代理部分填写 `https://gateway.ai.cloudflare.com/v1/ACCOUNT_TAG/GATEWAY/openai` 即可。
14. 支持丰富的**自定义**设置，
    1. 支持自定义系统名称，logo 以及页脚。
15. 支持通过系统访问令牌调用管理 API，进而**在无需二开的情况下扩展和自定义** My API 的功能，详情请参考此处 [API 文档](./docs/API.md)。
16. 支持 Cloudflare Turnstile 用户校验。
17. 支持用户管理，支持**多种用户登录注册方式**：
    + 邮箱登录注册（支持注册邮箱白名单）以及通过邮箱进行密码重置。

## 部署
### 基于 Docker 进行部署
```shell
# 使用 SQLite 的部署命令：
docker run --name myapi -d --restart always -p 3000:3000 -e TZ=Asia/Shanghai -v /home/ubuntu/data/myapi:/data pai801/myapi
# 使用 MySQL 的部署命令，在上面的基础上添加 `-e SQL_DSN="root:123456@tcp(localhost:3306)/myapi"`，请自行修改数据库连接参数，不清楚如何修改请参见下面环境变量一节。
# 例如：
docker run --name myapi -d --restart always -p 3000:3000 -e SQL_DSN="root:123456@tcp(localhost:3306)/myapi" -e TZ=Asia/Shanghai -v /home/ubuntu/data/myapi:/data pai801/myapi
```

其中，`-p 3000:3000` 中的第一个 `3000` 是宿主机的端口，可以根据需要进行修改。

数据和日志将会保存在宿主机的 `/home/ubuntu/data/myapi` 目录，请确保该目录存在且具有写入权限，或者更改为合适的目录。

如果启动失败，请添加 `--privileged=true`，具体参考 https://github.com/pai801/myapi/issues/482 。

如果上面的镜像无法拉取，可以尝试使用 GitHub 的 Docker 镜像，将上面的 `pai801/myapi` 替换为 `ghcr.io/pai801/myapi` 即可。

如果你的并发量较大，**务必**设置 `SQL_DSN`，详见下面[环境变量](#环境变量)一节。

更新命令：`docker run --rm -v /var/run/docker.sock:/var/run/docker.sock containrrr/watchtower -cR`

Nginx 的参考配置：
```
server{
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
   ````
2. 运行：
   ```shell
   chmod u+x myapi
   ./myapi --port 3000 --log-dir ./logs
   ```
3. 访问 [http://localhost:3000/](http://localhost:3000/) 并登录。初始账号用户名为 `root`，密码为 `123456`。

## 配置
系统本身开箱即用。

你可以通过设置环境变量或者命令行参数进行配置。

等到系统启动后，使用 `root` 用户登录系统并做进一步的配置。

**Note**：如果你不知道某个配置项的含义，可以临时删掉值以看到进一步的提示文字。

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

### 环境变量
> My API 支持从 `.env` 文件中读取环境变量，请参照 `.env.example` 文件，使用时请将其重命名为 `.env`。
1. `REDIS_CONN_STRING`：设置之后将使用 Redis 作为缓存使用。
   + 例子：`REDIS_CONN_STRING=redis://default:redispw@localhost:49153`
   + 如果数据库访问延迟很低，没有必要启用 Redis，启用后反而会出现数据滞后的问题。
   + 如果需要使用哨兵或者集群模式：
     + 则需要把该环境变量设置为节点列表，例如：`localhost:49153,localhost:49154,localhost:49155`。
     + 除此之外还需要设置以下环境变量：
       + `REDIS_PASSWORD`：Redis 集群或者哨兵模式下的密码设置。
       + `REDIS_MASTER_NAME`：Redis 哨兵模式下主节点的名称。
2. `SESSION_SECRET`：设置之后将使用固定的会话密钥，这样系统重新启动后已登录用户的 cookie 将依旧有效。
   + 例子：`SESSION_SECRET=random_string`
3. `SQL_DSN`：设置之后将使用指定数据库而非 SQLite，请使用 MySQL 或 PostgreSQL。
   + 例子：
     + MySQL：`SQL_DSN=root:123456@tcp(localhost:3306)/myapi`
     + PostgreSQL：`SQL_DSN=postgres://postgres:123456@localhost:5432/myapi`（适配中，欢迎反馈）
   + 注意需要提前建立数据库 `myapi`，无需手动建表，程序将自动建表。
   + 如果使用本地数据库：部署命令可添加 `--network="host"` 以使得容器内的程序可以访问到宿主机上的 MySQL。
   + 如果使用云数据库：如果云服务器需要验证身份，需要在连接参数中添加 `?tls=skip-verify`。
   + 请根据你的数据库配置修改下列参数（或者保持默认值）：
     + `SQL_MAX_IDLE_CONNS`：最大空闲连接数，默认为 `100`。
     + `SQL_MAX_OPEN_CONNS`：最大打开连接数，默认为 `1000`。
       + 如果报错 `Error 1040: Too many connections`，请适当减小该值。
     + `SQL_CONN_MAX_LIFETIME`：连接的最大生命周期，默认为 `60`，单位分钟。
4. `LOG_SQL_DSN`：设置之后将为 `logs` 表使用独立的数据库，请使用 MySQL 或 PostgreSQL。
5. `FRONTEND_BASE_URL`：设置之后将重定向页面请求到指定的地址。
   + 例子：`FRONTEND_BASE_URL=https://your-domain.com`
6. `MEMORY_CACHE_ENABLED`：启用内存缓存，会导致用户额度的更新存在一定的延迟，可选值为 `true` 和 `false`，未设置则默认为 `false`。
   + 例子：`MEMORY_CACHE_ENABLED=true`
7. `SYNC_FREQUENCY`：在启用缓存的情况下与数据库同步配置的频率，单位为秒，默认为 `600` 秒。
   + 例子：`SYNC_FREQUENCY=60`
8. `CHANNEL_UPDATE_FREQUENCY`：设置之后将定期更新渠道余额，单位为分钟，未设置则不进行更新。
   + 例子：`CHANNEL_UPDATE_FREQUENCY=1440`
9. `CHANNEL_TEST_FREQUENCY`：设置之后将定期检查渠道，单位为分钟，未设置则不进行检查。
   +例子：`CHANNEL_TEST_FREQUENCY=1440`
10. `POLLING_INTERVAL`：批量更新渠道余额以及测试可用性时的请求间隔，单位为秒，默认无间隔。
    + 例子：`POLLING_INTERVAL=5`
11. `BATCH_UPDATE_ENABLED`：启用数据库批量更新聚合，会导致用户额度的更新存在一定的延迟可选值为 `true` 和 `false`，未设置则默认为 `false`。
    + 例子：`BATCH_UPDATE_ENABLED=true`
    + 如果你遇到了数据库连接数过多的问题，可以尝试启用该选项。
12. `BATCH_UPDATE_INTERVAL=5`：批量更新聚合的时间间隔，单位为秒，默认为 `5`。
    + 例子：`BATCH_UPDATE_INTERVAL=5`
13. 请求频率限制：
    + `GLOBAL_API_RATE_LIMIT`：全局 API 速率限制（除中继请求外），单 ip 三分钟内的最大请求数，默认为 `180`。
    + `GLOBAL_WEB_RATE_LIMIT`：全局 Web 速率限制，单 ip 三分钟内的最大请求数，默认为 `60`。
14. 编码器缓存设置：
    + `TIKTOKEN_CACHE_DIR`：默认程序启动时会联网下载一些通用的词元的编码，如：`gpt-3.5-turbo`，在一些网络环境不稳定，或者离线情况，可能会导致启动有问题，可以配置此目录缓存数据，可迁移到离线环境。
    + `DATA_GYM_CACHE_DIR`：目前该配置作用与 `TIKTOKEN_CACHE_DIR` 一致，但是优先级没有它高。
15. `RELAY_TIMEOUT`：中继超时设置，单位为秒，默认不设置超时时间。
16. `RELAY_PROXY`：设置后使用该代理来请求 API。
17. `USER_CONTENT_REQUEST_TIMEOUT`：用户上传内容下载超时时间，单位为秒。
18. `USER_CONTENT_REQUEST_PROXY`：设置后使用该代理来请求用户上传的内容，例如图片。
19. `SQLITE_BUSY_TIMEOUT`：SQLite 锁等待超时设置，单位为毫秒，默认 `3000`。
20. `GEMINI_SAFETY_SETTING`：Gemini 的安全设置，默认 `BLOCK_NONE`。
21. `GEMINI_VERSION`：My API 所使用的 Gemini 版本，默认为 `v1`。
22. `ENABLE_METRIC`：是否根据请求成功率禁用渠道，默认不开启，可选值为 `true` 和 `false`。
23. `METRIC_QUEUE_SIZE`：请求成功率统计队列大小，默认为 `10`。
24. `METRIC_SUCCESS_RATE_THRESHOLD`：请求成功率阈值，默认为 `0.8`。
26. `INITIAL_ROOT_TOKEN`：如果设置了该值，则在系统首次启动时会自动创建一个值为该环境变量值的 root 用户令牌。
27. `INITIAL_ROOT_ACCESS_TOKEN`：如果设置了该值，则在系统首次启动时会自动创建一个值为该环境变量的 root 用户创建系统管理令牌。
28. `ENFORCE_INCLUDE_USAGE`：是否强制在 stream 模型下返回 usage，默认不开启，可选值为 `true` 和 `false`。
29. `TEST_PROMPT`：测试模型时的用户 prompt，默认为 `Print your model name exactly and do not output without any other text.`。

### 命令行参数
1. `--port <port_number>`: 指定服务器监听的端口号，默认为 `3000`。
   + 例子：`--port 3000`
2. `--log-dir <log_dir>`: 指定日志文件夹，如果没有设置，默认保存至工作目录的 `logs` 文件夹下。
   + 例子：`--log-dir ./logs`
3. `--version`: 打印系统版本号并退出。
4. `--help`: 查看命令的使用帮助和参数说明。

## 注意

本项目基于 One API (MIT) 进行二次开发，保留 MIT 许可证。
