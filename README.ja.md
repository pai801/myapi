<p align="right">
   <a href="./README.md">中文</a> | <a href="./README.en.md">English</a> | <strong>日本語</strong>
</p>

> **Fork 声明**: 本プロジェクトは [One API](https://github.com/songquanpeng/one-api) をベースに修正を加えたもので、元の MIT ライセンスを保持しています。

<p align="center">
  <a href="https://github.com/pai801/myapi"><img src="https://raw.githubusercontent.com/pai801/myapi/main/web/default/public/logo.png" width="150" height="150" alt="myapi logo"></a>
</p>

<div align="center">

# My API

_✨ 標準的な OpenAI API フォーマットを通じてすべての LLM にアクセス、すぐに使える ✨_

**個人・小チーム向けの AI API ゲートウェイ —— 運営・課金モジュールを削除し、リレーとルーティングに特化。**

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
  <a href="#デプロイメント">デプロイメント</a> ·
  <a href="#使用方法">使用方法</a> ·
  <a href="#特色機能">特色機能</a> ·
  <a href="https://github.com/pai801/myapi/issues">フィードバック</a>
</p>

> [!NOTE]
> 本プロジェクトはオープンソースプロジェクトです。利用者は OpenAI の[利用規約](https://openai.com/policies/terms-of-use)および**法令**を遵守した上で使用し、違法な用途に用いないでください。
>
> [「生成式人工知能サービス管理暫行弁法」](http://www.cac.gov.cn/2023-07/13/c_1690898327029107.htm)の要求に基づき、中国地域の公衆に対して未届出の生成式 AI サービスを提供しないでください。

> [!NOTE]
> 安定版 / プレビュー版イメージ: [pai801/myapi](https://hub.docker.com/repository/docker/pai801/myapi)
> または [ghcr.io/pai801/myapi](https://github.com/pai801/myapi/pkgs/container/myapi)
>
> alpha 版イメージ: [pai801/myapi-alpha](https://hub.docker.com/repository/docker/pai801/myapi-alpha)
> または [ghcr.io/pai801/myapi-alpha](https://github.com/pai801/myapi/pkgs/container/myapi-alpha)

> [!WARNING]
> root ユーザーで初回ログイン後、必ずデフォルトパスワード `123456` を変更してください。

---

## One API との違い

My API は One API を基盤に大規模なリファクタリングと機能強化を施しています。以下は主な差分の概要です。

| 機能 | One API | My API |
|------|---------|--------|
| グループの粒度 | ユーザー単位 | **トークン単位**、よりきめ細かな課金制御 |
| チャネル選択 | 単純なロードバランシング | **アフィニティルーティング + クールダウン + 加重ランダム** |
| 失敗時リトライ | 基本的なリトライ | **セマンティック分析によるインテリジェントリトライ**、エラー種別に応じた判断 |
| リアルタイム監視 | なし | **SSE リアルタイムプッシュ**、ストリームリクエストの全行程を可観測 |
| ChatGPT サブスクリプション | 非対応 | **ChatGPTSub アダプター**、スティッキーセッション・サーキットブレイカー・ヘルスプローブ搭載 |
| OpenAI Codex | 非対応 | **Codex アダプター**、Responses API フォーマット変換に対応 |
| Responses API | 非対応 | `/v1/responses` および `/v1/responses/compact` を完全サポート |
| 透過プロキシ | 非対応 | **Proxy Relay モード**、上流の任意エンドポイントへ透過転送 |
| モデルメタデータ | なし | 豊富なメタデータ: コンテキストウィンドウ・モダリティ・推論レベル・ツール種別 |
| ログシステム | 基本的なログ | **リクエスト/レスポンスボディの完全記録**、自動クリーンアップ、独立ログストア |
| トークンモデルマッピング | なし | **トークン単位のモデル名リライト**、ディスパッチ前に透過的に書き換え |
| 運営・課金機能 | 含む | **削除済み**、個人/小チームの自用向け、運営モジュール不要 |

**ポジショニングの違い**: One API は運営向け（課金・代理販売・招待コード等の商用モジュール含む）であり、My API は個人・小チームの自用向けです。My API は運営・課金関連機能をすべて削除し、コードを簡素化し、デプロイを軽量化して、API リレー・インテリジェントルーティング・可観測性に特化しています。

---

## 特色機能

### トークン単位のグループと柔軟な課金

グループの概念をユーザーからトークンへ移行しました。同じユーザーが異なるグループに属するトークンを発行でき、各グループでは独立したモデル倍率（乗数モード）を設定できます。課金計算式は以下の通りです。

```
コスト = (入力トークン + 出力トークン × 出力倍率) × モデル倍率 × グループ倍率
```

管理者は `/api/group/*` API を通じてグループの CRUD 操作を行えます。グループ削除前には、関連するトークンやチャネルが残っていないかを検証し、誤削除を防止します。デフォルトグループの倍率は常に 1.0 で、後方互換性を保証します。

### インテリジェントチャネルルーティング

チャネル選択は単純なラウンドロビンやランダムではなく、3つの戦略が協調して動作します。

**アフィニティ**: 各 `(ユーザー, モデル)` のペアについて前回成功したチャネルを記憶し、TTL（デフォルト 300 秒）内は同じチャネルを優先的に再利用します。マルチターン会話のコンテキスト一貫性を保つための仕組みです。

**クールダウン**: チャネルが失敗すると直ちにクールダウン期間（デフォルト 600 秒）に入り、その間は候補リストから除外されます。トラフィックが障害チャネルに流れ続けるのを防ぎます。

**加重ランダム**: 候補チャネルの中から優先度ウェイトに基づいた加重ランダム配分を行い、高優先度チャネルにより多くのトラフィックを割り当てます。

3つの戦略の連携効果: 馴染みのチャネルを優先的に再利用 → 障害発生時は自動的にクールダウンで隔離 → 健全なチャネルの中でウェイトに応じて配分。

### セマンティック分析によるインテリジェントリトライ

リトライの判断はステータスコードの単純なマッチングではなく、エラーの意味（セマンティクス）の深い分析に基づいて行われます。

- **リトライ対象**: 429 レートリミット、5xx サーバーエラー、トランスポート層の失敗、モデル非互換（`unsupported_model`）、上流のレスポンスフォーマット異常
- **リトライ非対象**: リクエストフォーマットエラー（`invalid_request_error`）、不正な入力パラメータ、固定チャネル ID を指定したリクエスト

各リトライでは前回失敗したチャネルを自動的に除外し、`SelectChannel()` で新しい候補チャネルを選択して、同じ失敗を繰り返すのを防ぎます。

### リアルタイムストリームリクエスト監視（SSE）

管理画面が SSE（Server-Sent Events）を通じて、進行中のすべてのストリームリクエストのステータスをリアルタイムで受信します。

```
GET /api/log/active/events
```

各追跡リクエストには以下の情報が含まれます: リクエスト ID、ユーザー、トークン、モデル、チャネル、経過時間、リクエストボディ/リクエストヘッダー（機密情報はマスキング済み）。イベントタイプは `start`、`update`、`end`、`complete` をカバーし、`complete` イベントには完全なデータベースログレコードが含まれるため、フロントエンドで即座に表示できます。

内部では pub/sub イベントバス + 有界バッファ（64）を採用し、スローコンシューマーに対してはグレースフルドロップを行ってメインのリクエストパスに影響を与えません。TTL クリーンアップサイクルは `RELAY_TIMEOUT` と連動し、デフォルト 30 分のフォールバック回収を行います。

### ChatGPT サブスクリプションアダプター（ChatGPTSub）

ChatGPT サブスクリプションアカウントをチャネルとしてゲートウェイに接続します。Sub2API の設計を参考にしており、完全なアカウントプール管理をサポートします。

**スティッキーセッション**: `conversation_id` / `session_hash` を通じてマルチターン会話を同じサブスクリプションアカウントにバインドし、TTL 1 時間で会話コンテキストの一貫性を確保します。

**サーキットブレイカー**: EWMA アルゴリズムに基づいて各アカウントのエラー率を追跡します。エラー率が 50% に達するか、5 回連続で失敗するとそのアカウントを自動的に遮断します。バックグラウンドプローブが遮断後も継続的にプロービングを行い、3 回連続で成功すると自動的に復旧します。

**ヘルス統計**: 各アカウントが EWMA エラー率と TTFT（Time To First Token）指標を独立して管理します。

**リクエストヘッダーホワイトリスト**: 安全な 8 個のリクエストヘッダーのみを透過し、上流のリスク制御のトリガーを回避します。

**双方向フォーマット変換**: Chat Completions と Responses API フォーマットの完全な双方向変換。ストリーム SSE イベントやツール呼び出しも含みます。

### Codex アダプターと Responses API

**Codex アダプター**: OpenAI の Codex API（`chatgpt.com/backend-api`）へのプロキシリクエスト。Chat Completions および Responses API フォーマットの自動変換をサポートし、ストリームと非ストリームの両モードに対応します。

**Responses API**: OpenAI の次世代 API フォーマットを完全サポート:

- `POST /v1/responses`: 標準 Responses エンドポイント
- `POST /v1/responses/compact`: コンパクトモード
- ストリーム SSE イベントは終端セマンティクスを厳密に保証: 成功時は `response.completed` + `[DONE]` がちょうど 1 回、失敗時は `response.failed` がちょうど 1 回で、失敗後に誤って `completed` を発行することはありません

**Proxy Relay モード**: `/v1/myapi/proxy/:channelid/*target` を通じて、リクエストを指定チャネルの上流 URL へ完全に透過転送します。ベンダー固有の OpenAI 非互換エンドポイントへのアクセスに使用します。

### モデルメタデータシステム

各モデルには豊富なメタデータを保存でき、`/v1/models` API で返却されます:

| フィールド | 説明 |
|------|------|
| `display_name` | 表示名 |
| `context_window` | コンテキストウィンドウサイズ |
| `max_output_tokens` | 最大出力トークン数 |
| `input_modalities` / `output_modalities` | 入力/出力モダリティ（text, image, audio...） |
| `supported_reasoning_levels` | サポートする推論レベル |
| `supported_endpoint_types` | サポートするエンドポイント種別 |
| `truncation_policy` | 切り捨てポリシー |
| `web_search_tool_type` | Web 検索ツール種別 |
| `apply_patch_tool_type` | パッチ適用ツール種別 |

未知のモデルにはデフォルトメタデータが自動生成され、管理者が手動で編集・上書きできます。

### 強化されたログシステム

ログは「誰がいつどのモデルを呼んだか」だけでなく、完全なリクエスト監査チェーンを提供します:

- **リクエストボディ / レスポンスボディ / リクエストヘッダー**: 完全記録、TEXT フィールドとして保存、機密情報（Authorization など）は自動マスキング
- **リストクエリの最適化**: リスト API ではブールフラグ（`has_request_body` など）を使用して大きなフィールドの転送を回避し、詳細は個別 API で取得
- **自動クリーンアップ**: バックグラウンドで毎時クリーンアップを実行。ログの保持期間はデフォルト 168 時間（7 日）、リクエスト/レスポンスボディの保持期間はデフォルト 4 時間
- **独立ログストア**: `LOG_SQL_DSN` によりログテーブルを独立データベースに分離し、高頻度書き込みによるメインビジネス DB への影響を回避
- **キャッシュトークン追跡**: prompt caching のヒット量を記録し、コスト最適化効果の分析に活用

### トークン単位のモデルマッピング

トークン単位でモデル名リライトルール（JSON 形式）を設定し、チャネルディスパッチ前にユーザーリクエストのモデル名を実際のモデル名へ透過的にマッピングします。これにより、各トークン保持者がカスタムのモデルエイリアスを使用でき、チャネルレベルのモデル設定に影響を与えません。

### チャネル自動管理

**インテリジェント無効化**: チャネルが 401 未認証、残高不足、API キー無効、アカウントBAN などのエラーを返した場合、自動的に無効化され、人手の介入は不要です。

**メトリクス駆動の無効化**: スライディングウィンドウでチャネルのリクエスト成功率を追跡し、成功率が閾値（デフォルト 80%）を下回った場合に自動的に無効化します。

**自動復旧**: 無効化されたチャネルを定期的にテストし、テスト成功後に自動的に再有効化します。

**レスポンス時間閾値**: 設定されたレスポンス時間の上限を超えたチャネルを自動的に無効化します。

---

## サポートされるモデルチャネル

**56 種のチャネルタイプ**、**21 の API アダプター**をサポートし、国内外の主要 LLM ベンダーをカバーしています:

**海外ベンダー**: OpenAI ChatGPT シリーズ（Azure OpenAI 含む）、Anthropic Claude シリーズ（AWS Claude 含む）、Google PaLM2/Gemini/Vertex AI、Mistral、Cohere、xAI、Groq、together.ai、Cloudflare Workers AI、DeepL、Ollama、Replicate、OpenRouter

**国内ベンダー**: 百度文心一言、阿里通義千問（阿里百煉含む）、訊飛星火、智譜 ChatGLM、360 智脳、騰訊混元、Moonshot、百川大模型、MiniMax、字節跳動豆包（火山引擎）、零一万物、階躍星辰、DeepSeek、硅基流動 SiliconCloud、Coze、novita.ai

**特殊チャネル**: Codex（OpenAI バックエンド API プロキシ）、ChatGPTSub（ChatGPT サブスクリプションアカウントプール）、Proxy（汎用上流透過転送）

---

## デプロイメント

### Docker を使用したデプロイメント

```shell
# SQLite を使用する場合のデプロイコマンド:
docker run --name myapi -d --restart always -p 3000:3000 -e TZ=Asia/Shanghai -v /home/ubuntu/data/myapi:/data pai801/myapi

# MySQL を使用する場合、上記に -e SQL_DSN="root:123456@tcp(localhost:3306)/myapi" を追加
# データベース接続パラメータは適宜修正してください。詳細は下の環境変数のセクションを参照してください。
docker run --name myapi -d --restart always -p 3000:3000 -e SQL_DSN="root:123456@tcp(localhost:3306)/myapi" -e TZ=Asia/Shanghai -v /home/ubuntu/data/myapi:/data pai801/myapi
```

`-p 3000:3000` の最初の `3000` はホスト側のポートで、必要に応じて変更できます。

データとログはホストの `/home/ubuntu/data/myapi` ディレクトリに保存されます。このディレクトリが存在し、書き込み権限があることを確認してください。または適切なディレクトリに変更してください。

起動に失敗した場合は `--privileged=true` を追加してください。詳しくは https://github.com/pai801/myapi/issues/482 を参照してください。

上記のイメージが取得できない場合は、GitHub の Docker イメージをお試しください。`pai801/myapi` を `ghcr.io/pai801/myapi` に置き換えてください。

高コンカレンシーが予想される場合は、**必ず** `SQL_DSN` を設定してください。詳細は[環境変数](#環境変数)のセクションを参照してください。

更新コマンド: `docker run --rm -v /var/run/docker.sock:/var/run/docker.sock containrrr/watchtower -cR`

Nginx の参考設定:

```nginx
server {
   server_name your-domain.com;  # ドメイン名は適宜変更してください

   location / {
          client_max_body_size  64m;
          proxy_http_version 1.1;
          proxy_pass http://localhost:3000;  # ポートは適宜変更してください
          proxy_set_header Host $host;
          proxy_set_header X-Forwarded-For $remote_addr;
          proxy_cache_bypass $http_upgrade;
          proxy_set_header Accept-Encoding gzip;
          proxy_read_timeout 300s;  # GPT-4 は長いタイムアウトが必要です。適宜調整してください
   }
}
```

その後、Let's Encrypt の certbot を使用して HTTPS を設定します:

```bash
# Ubuntu に certbot をインストール:
sudo snap install --classic certbot
sudo ln -s /snap/bin/certbot /usr/bin/certbot
# 証明書の生成 & Nginx 設定の変更
sudo certbot --nginx
# 指示に従って操作
# Nginx を再起動
sudo service nginx restart
```

初期アカウントのユーザー名は `root`、パスワードは `123456` です。

### Docker Compose を使用したデプロイメント

> 起動方法のみ異なり、パラメータ設定は同じです。Docker を使用したデプロイメントのセクションを参照してください

```shell
# 現在 MySQL 起動に対応、データは ./data/mysql ディレクトリに保存されます
docker-compose up -d

# デプロイ状態を確認
docker-compose ps
```

### 手動デプロイメント

1. [GitHub Releases](https://github.com/pai801/myapi/releases/latest) から実行ファイルをダウンロードするか、ソースからコンパイルします:

   ```shell
   git clone https://github.com/pai801/myapi.git

   # フロントエンドのビルド
   cd myapi/web/default
   npm install
   npm run build

   # バックエンドのビルド
   cd ../..
   go mod download
   go build -ldflags "-s -w" -o myapi
   ```

2. 実行:

   ```shell
   chmod u+x myapi
   ./myapi --port 3000 --log-dir ./logs
   ```

3. [http://localhost:3000/](http://localhost:3000/) にアクセスしてログインします。初期アカウントのユーザー名は `root`、パスワードは `123456` です。

---

## 使用方法

`チャネル` ページで API Key を追加し、`トークン` ページでアクセストークンを追加します。

その後、トークンを使用して My API にアクセスできます。使い方は [OpenAI API](https://platform.openai.com/docs/api-reference/introduction) と同じです。

OpenAI API を使用する各種環境で、API Base を My API のデプロイアドレス（例: `https://your-domain.com`）に設定し、API Key には My API で生成したトークンを指定してください。

API Base の具体的なフォーマットは、使用するクライアントによって異なります。

たとえば OpenAI の公式ライブラリの場合:

```bash
OPENAI_API_KEY="sk-xxxxxx"
OPENAI_API_BASE="https://<HOST>:<PORT>/v1"
```

```mermaid
graph LR
    A(ユーザー)
    A --->|My API のトークンでリクエスト| B(My API)
    B -->|リクエストをリレー| C(OpenAI)
    B -->|リクエストをリレー| D(Azure)
    B -->|リクエストをリレー| E(その他の OpenAI API 互換チャネル)
    B -->|リクエストボディとレスポンスボディを変換してリレー| F(非 OpenAI API 互換チャネル)
```

トークンの末尾にチャネル ID を追加することで、どのチャネルでリクエストを処理するかを指定できます: `Authorization: Bearer MY_API_KEY-CHANNEL_ID`。
なお、チャネル ID を指定できるのは管理者ユーザーが作成したトークンのみです。

チャネル ID を指定しない場合は、ロードバランシングにより複数のチャネルにリクエストが振り分けられます。

---

## 設定

システムはそのままですぐに使えます。環境変数またはコマンドラインパラメータを設定することでカスタマイズできます。

システム起動後、`root` ユーザーでログインして詳細な設定を行ってください。

**Note**: 設定項目の意味がわからない場合は、一時的に値を削除するとヒントテキストが表示されます。

### 環境変数

> My API は `.env` ファイルからの環境変数の読み込みをサポートしています。`.env.example` ファイルを参照し、使用する際は `.env` にリネームしてください。

**データベースとキャッシュ:**

| 変数 | 説明 | デフォルト値 |
|------|------|--------|
| `SQL_DSN` | SQLite の代わりに MySQL または PostgreSQL を使用 | なし（SQLite を使用） |
| `LOG_SQL_DSN` | ログテーブル用の独立データベース | なし（メイン DB と共用） |
| `SQL_MAX_IDLE_CONNS` | 最大アイドル接続数 | `100` |
| `SQL_MAX_OPEN_CONNS` | 最大オープン接続数 | `1000` |
| `SQL_CONN_MAX_LIFETIME` | 接続の最大存続時間（分） | `60` |
| `SQLITE_BUSY_TIMEOUT` | SQLite ロック待ちタイムアウト（ミリ秒） | `3000` |
| `REDIS_CONN_STRING` | Redis 接続文字列（キャッシュレイヤーとして使用） | なし |
| `REDIS_PASSWORD` | Redis クラスター/センチネルモードのパスワード | なし |
| `REDIS_MASTER_NAME` | Redis センチネルモードのマスターノード名 | なし |
| `MEMORY_CACHE_ENABLED` | メモリキャッシュを有効化 | `false` |
| `BATCH_UPDATE_ENABLED` | データベースバッチ更新集約を有効化 | `false` |
| `BATCH_UPDATE_INTERVAL` | バッチ更新間隔（秒） | `5` |
| `SYNC_FREQUENCY` | キャッシュとデータベースの同期頻度（秒） | `600` |

**チャネルとルーティング:**

| 変数 | 説明 | デフォルト値 |
|------|------|--------|
| `CHANNEL_UPDATE_FREQUENCY` | チャネル残高の定期更新間隔（分） | なし（更新しない） |
| `CHANNEL_TEST_FREQUENCY` | チャネル可用性の定期テスト間隔（分） | なし（テストしない） |
| `CHANNEL_COOLDOWN_SECONDS` | チャネル失敗後のクールダウン期間（秒） | `600` |
| `AFFINITY_EXPIRE_SECONDS` | ユーザー・モデル・チャネルのアフィニティ TTL（秒） | `300` |
| `POLLING_INTERVAL` | バッチ更新/テスト時のリクエスト間隔（秒） | なし |
| `ENABLE_METRIC` | 成功率に基づくチャネル自動無効化を有効化 | `false` |
| `METRIC_QUEUE_SIZE` | 成功率統計キューサイズ | `10` |
| `METRIC_SUCCESS_RATE_THRESHOLD` | 成功率閾値 | `0.8` |

**リレーとプロキシ:**

| 変数 | 説明 | デフォルト値 |
|------|------|--------|
| `RELAY_TIMEOUT` | リレータイムアウト（秒） | なし |
| `RELAY_PROXY` | リレーリクエストのプロキシアドレス | なし |
| `ENFORCE_INCLUDE_USAGE` | ストリームレスポンスで usage の返却を強制 | `false` |
| `GEMINI_SAFETY_SETTING` | Gemini セーフティ設定 | `BLOCK_NONE` |
| `GEMINI_VERSION` | Gemini API バージョン | `v1` |

**ログ:**

| 変数 | 説明 | デフォルト値 |
|------|------|--------|
| `LOG_CLEAN_HOURS` | ログ保持期間（時間） | `168` |
| `LOG_CLEAN_BODIES_HOURS` | リクエスト/レスポンスボディの保持期間（時間） | `4` |
| `MAX_LOGGED_BODY_SIZE` | 記録するリクエストボディの最大サイズ（バイト） | `2097152`（2MB） |

**セキュリティとレート制限:**

| 変数 | 説明 | デフォルト値 |
|------|------|--------|
| `SESSION_SECRET` | 固定セッションシークレット（再起動後も cookie が有効） | なし |
| `GLOBAL_API_RATE_LIMIT` | 単一 IP からの 3 分間最大 API リクエスト数 | `480` |
| `GLOBAL_WEB_RATE_LIMIT` | 単一 IP からの 3 分間最大 Web リクエスト数 | `240` |

**その他:**

| 変数 | 説明 | デフォルト値 |
|------|------|--------|
| `FRONTEND_BASE_URL` | フロントエンドページのリダイレクト先アドレス | なし |
| `INITIAL_ROOT_TOKEN` | 初回起動時に自動作成される root トークン値 | なし |
| `INITIAL_ROOT_ACCESS_TOKEN` | 初回起動時に自動作成されるシステム管理トークン値 | なし |
| `TEST_PROMPT` | モデルテスト時のユーザープロンプト | `Print your model name exactly...` |
| `TIKTOKEN_CACHE_DIR` | トークナイザーエンコーディングキャッシュディレクトリ | なし |
| `USER_CONTENT_REQUEST_TIMEOUT` | ユーザーアップロードコンテンツのダウンロードタイムアウト（秒） | なし |
| `USER_CONTENT_REQUEST_PROXY` | ユーザーコンテンツリクエストのプロキシ | なし |

### コマンドラインパラメータ

| パラメータ | 説明 | デフォルト値 |
|------|------|--------|
| `--port <port>` | サーバーのリッスンポート | `3000` |
| `--log-dir <dir>` | ログディレクトリ | `./logs` |
| `--version` | バージョン番号を出力して終了 | - |
| `--help` | ヘルプを表示 | - |

---

## 注意事項

本プロジェクトは One API (MIT) をベースに二次開発を行い、MIT ライセンスを保持しています。
