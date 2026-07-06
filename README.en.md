<p align="right">
    <a href="./README.md">中文</a> | <strong>English</strong> | <a href="./README.ja.md">日本語</a>
</p>
> **Fork Notice**: This project is a fork of [One API](https://github.com/songquanpeng/one-api) and retains the original MIT License.

<p align="center">
  <a href="https://github.com/pai801/myapi"><img src="https://raw.githubusercontent.com/pai801/myapi/main/web/default/public/logo.png" width="150" height="150" alt="myapi logo"></a>
</p>

<div align="center">

# My API

_✨ Access all LLM through the standard OpenAI API format, easy to deploy & use ✨_

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
  <a href="#deployment">Deployment Tutorial</a>
  ·
  <a href="#usage">Usage</a>
  ·
  <a href="https://github.com/pai801/myapi/issues">Feedback</a>
</p>

> **Warning**: This README is translated by ChatGPT. Please feel free to submit a PR if you find any translation errors.

> **Note**: The latest image pulled from Docker may be an `alpha` release. Specify the version manually if you require stability.

## Features
1. Support for multiple large models:
   + [x] [OpenAI ChatGPT Series Models](https://platform.openai.com/docs/guides/gpt/chat-completions-api) (Supports [Azure OpenAI API](https://learn.microsoft.com/en-us/azure/ai-services/openai/reference))
   + [x] [Anthropic Claude Series Models](https://anthropic.com) (Supports AWS Claude)
   + [x] [Google PaLM2 and Gemini Series Models](https://developers.generativeai.google)
   + [x] [Baidu Wenxin Yiyuan Series Models](https://cloud.baidu.com/doc/WENXINWORKSHOP/index.html)
   + [x] [Alibaba Tongyi Qianwen Series Models](https://help.aliyun.com/document_detail/2400395.html)
   + [x] [Zhipu ChatGLM Series Models](https://bigmodel.cn)
2. Supports configuration of mirror sites and third-party proxy services.
3. Supports access to multiple channels through **load balancing**.
4. Supports **stream mode** that enables typewriter-like effect through stream transmission.
5. Supports **token management** that allows setting allowed IP ranges and allowed model access.
6. Supports **channel management** that allows bulk creation of channels.
7. Supports **user grouping** and **channel grouping**.
8. Supports channel **model list configuration**.
9. Supports **quota details checking**.
10. Supports model mapping to redirect user's request model. Please do not set it unless necessary, as it will cause the request body to be reconstructed instead of being directly passed through.
11. Supports automatic retry on failure.
12. Supports image generation API.
13. Supports [Cloudflare AI Gateway](https://developers.cloudflare.com/ai-gateway/providers/openai/). Fill in `https://gateway.ai.cloudflare.com/v1/ACCOUNT_TAG/GATEWAY/openai` in the proxy section of the channel settings.
14. Offers rich **customization** options:
    1. Supports customization of system name, logo, and footer.
15. Supports management API access through system access tokens, enabling extension and customization of My API without secondary development. See the [API documentation](./docs/API.md) for details.
16. Supports Cloudflare Turnstile user verification.
17. Supports user management and multiple user login/registration methods:
    + Email login/registration (with email whitelist support) and password reset via email.

## Deployment
### Docker Deployment

Deployment command:
`docker run --name myapi -d --restart always -p 3000:3000 -e TZ=Asia/Shanghai -v /home/ubuntu/data/myapi:/data pai801/myapi`

Update command: `docker run --rm -v /var/run/docker.sock:/var/run/docker.sock containrrr/watchtower -cR`

The first `3000` in `-p 3000:3000` is the port of the host, which can be modified as needed.

Data will be saved in the `/home/ubuntu/data/myapi` directory on the host. Ensure that the directory exists and has write permissions, or change it to a suitable directory.

Nginx reference configuration:
```
server{
   server_name your-domain.com;  # Modify your domain name accordingly

   location / {
          client_max_body_size  64m;
          proxy_http_version 1.1;
          proxy_pass http://localhost:3000;  # Modify your port accordingly
          proxy_set_header Host $host;
          proxy_set_header X-Forwarded-For $remote_addr;
          proxy_cache_bypass $http_upgrade;
          proxy_set_header Accept-Encoding gzip;
   }
}
```

Next, configure HTTPS with Let's Encrypt certbot:
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

### Manual Deployment
1. Download the executable file from [GitHub Releases](https://github.com/pai801/myapi/releases/latest) or compile from source:
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
3. Access [http://localhost:3000/](http://localhost:3000/) and log in. The initial account username is `root` and password is `123456`.

Please refer to the [environment variables](#environment-variables) section for details on using environment variables.

## Configuration
The system is ready to use out of the box.

You can configure it by setting environment variables or command line parameters.

After the system starts, log in as the `root` user to further configure the system.

## Usage
Add your API Key on the `Channels` page, and then add an access token on the `Tokens` page.

You can then use your access token to access My API. The usage is consistent with the [OpenAI API](https://platform.openai.com/docs/api-reference/introduction).

In places where the OpenAI API is used, remember to set the API Base to your My API deployment address, for example: `https://your-domain.com`. The API Key should be the token generated in My API.

Note that the specific API Base format depends on the client you are using.

```mermaid
graph LR
    A(User)
    A --->|Request| B(My API)
    B -->|Relay Request| C(OpenAI)
    B -->|Relay Request| D(Azure)
    B -->|Relay Request| E(Other downstream channels)
```

To specify which channel to use for the current request, you can add the channel ID after the token, for example: `Authorization: Bearer MY_API_KEY-CHANNEL_ID`.
Note that the token needs to be created by an administrator to specify the channel ID.

If the channel ID is not provided, load balancing will be used to distribute the requests to multiple channels.

### Environment Variables
1. `REDIS_CONN_STRING`: When set, Redis will be used as the storage for request rate limiting instead of memory.
    + Example: `REDIS_CONN_STRING=redis://default:redispw@localhost:49153`
2. `SESSION_SECRET`: When set, a fixed session key will be used to ensure that cookies of logged-in users are still valid after the system restarts.
    + Example: `SESSION_SECRET=random_string`
3. `SQL_DSN`: When set, the specified database will be used instead of SQLite. Please use MySQL version 8.0.
    + Example: `SQL_DSN=root:123456@tcp(localhost:3306)/myapi`
4. `LOG_SQL_DSN`: When set, a separate database will be used for the `logs` table; please use MySQL or PostgreSQL.
    + Example: `LOG_SQL_DSN=root:123456@tcp(localhost:3306)/myapi-logs`
5. `FRONTEND_BASE_URL`: When set, the specified frontend address will be used instead of the backend address.
    + Example: `FRONTEND_BASE_URL=https://your-domain.com`
6. 'MEMORY_CACHE_ENABLED': Enabling memory caching can cause a certain delay in updating user quotas, with optional values of 'true' and 'false'. If not set, it defaults to 'false'.
7. `SYNC_FREQUENCY`: When set, the system will periodically sync configurations from the database, with the unit in seconds. If not set, no sync will happen.
    + Example: `SYNC_FREQUENCY=60`
8. `CHANNEL_UPDATE_FREQUENCY`: When set, it periodically updates the channel balances, with the unit in minutes. If not set, no update will happen.
    + Example: `CHANNEL_UPDATE_FREQUENCY=1440`
9. `CHANNEL_TEST_FREQUENCY`: When set, it periodically tests the channels, with the unit in minutes. If not set, no test will happen.
    + Example: `CHANNEL_TEST_FREQUENCY=1440`
10. `POLLING_INTERVAL`: The time interval (in seconds) between requests when updating channel balances and testing channel availability. Default is no interval.
    + Example: `POLLING_INTERVAL=5`
11. `BATCH_UPDATE_ENABLED`: Enabling batch database update aggregation can cause a certain delay in updating user quotas. The optional values are 'true' and 'false', but if not set, it defaults to 'false'.
    +Example: ` BATCH_UPDATE_ENABLED=true`
    +If you encounter an issue with too many database connections, you can try enabling this option.
12. `BATCH_UPDATE_INTERVAL=5`: The time interval for batch updating aggregates, measured in seconds, defaults to '5'.
    +Example: ` BATCH_UPDATE_INTERVAL=5`
13. Request frequency limit:
    + `GLOBAL_API_RATE_LIMIT`: Global API rate limit (excluding relay requests), the maximum number of requests within three minutes per IP, default to 180.
    + `GLOBAL_WEL_RATE_LIMIT`: Global web speed limit, the maximum number of requests within three minutes per IP, default to 60.
14. Encoder cache settings:
    +`TIKTOKEN_CACHE_DIR`: By default, when the program starts, it will download the encoding of some common word elements online, such as' gpt-3.5 turbo '. In some unstable network environments or offline situations, it may cause startup problems. This directory can be configured to cache data and can be migrated to an offline environment.
    +`DATA_GYM_CACHE_DIR`: Currently, this configuration has the same function as' TIKTOKEN-CACHE-DIR ', but its priority is not as high as it.
15. `RELAY_TIMEOUT`: Relay timeout setting, measured in seconds, with no default timeout time set.
16. `RELAY_PROXY`: After setting up, use this proxy to request APIs.
17. `USER_CONTENT_REQUEST_TIMEOUT`: The timeout period for users to upload and download content, measured in seconds.
18. `USER_CONTENT_REQUEST_PROXY`: After setting up, use this agent to request content uploaded by users, such as images.
19. `SQLITE_BUSY_TIMEOUT`: SQLite lock wait timeout setting, measured in milliseconds, default to '3000'.
20. `GEMINI_SAFETY_SETTING`: Gemini's security settings are set to 'BLOCK-NONE' by default.
21. `GEMINI_VERSION`: The Gemini version used by My API, which defaults to 'v1'.
22. `THE`: The system's theme setting, default to 'default', specific optional values refer to [here] (./web/README. md).
23. `ENABLE_METRIC`: Whether to disable channels based on request success rate, default not enabled, optional values are 'true' and 'false'.
24. `METRIC_QUEUE_SIZE`: Request success rate statistics queue size, default to '10'.
25. `METRIC_SUCCESS_RATE_THRESHOLD`: Request success rate threshold, default to '0.8'.
27. `INITIAL_ROOT_TOKEN`: If this value is set, a root user token with the value of the environment variable will be automatically created when the system starts for the first time.
28. `INITIAL_ROOT_ACCESS_TOKEN`: If this value is set, a system management token will be automatically created for the root user with a value of the environment variable when the system starts for the first time.

### Command Line Parameters
1. `--port <port_number>`: Specifies the port number on which the server listens. Defaults to `3000`.
    + Example: `--port 3000`
2. `--log-dir <log_dir>`: Specifies the log directory. If not set, the logs will not be saved.
    + Example: `--log-dir ./logs`
3. `--version`: Prints the system version number and exits.
4. `--help`: Displays the command usage help and parameter descriptions.

## Note
This project is a fork of One API (MIT) and retains the MIT License.
