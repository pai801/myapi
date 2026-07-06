<p align="right">
    <a href="./README.md">中文</a> | <a href="./README.en.md">English</a> | <strong>日本語</strong>
</p>
> **Fork 通知**: このプロジェクトは [One API](https://github.com/songquanpeng/one-api) からフォークされたもので、元の MIT ライセンスを保持しています。

<p align="center">
  <a href="https://github.com/pai801/myapi"><img src="https://raw.githubusercontent.com/pai801/myapi/main/web/default/public/logo.png" width="150" height="150" alt="myapi logo"></a>
</p>

<div align="center">

# My API

_✨ 標準的な OpenAI API フォーマットを通じてすべての LLM にアクセスでき、導入と利用が容易です ✨_

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
  <a href="#deployment">デプロイチュートリアル</a>
  ·
  <a href="#usage">使用方法</a>
  ·
  <a href="https://github.com/pai801/myapi/issues">フィードバック</a>
</p>

> **警告**: この README は ChatGPT によって翻訳されています。翻訳ミスを発見した場合は遠慮なく PR を投稿してください。

> **注**: Docker からプルされた最新のイメージは、`alpha` リリースかもしれません。安定性が必要な場合は、手動でバージョンを指定してください。

## 特徴
1. 複数の大型モデルをサポート:
   + [x] [OpenAI ChatGPT シリーズモデル](https://platform.openai.com/docs/guides/gpt/chat-completions-api) ([Azure OpenAI API](https://learn.microsoft.com/en-us/azure/ai-services/openai/reference) をサポート)
   + [x] [Anthropic Claude シリーズモデル](https://anthropic.com) (AWS Claude をサポート)
   + [x] [Google PaLM2/Gemini シリーズモデル](https://developers.generativeai.google)
   + [x] [Baidu Wenxin Yiyuan シリーズモデル](https://cloud.baidu.com/doc/WENXINWORKSHOP/index.html)
   + [x] [Alibaba Tongyi Qianwen シリーズモデル](https://help.aliyun.com/document_detail/2400395.html)
   + [x] [Zhipu ChatGLM シリーズモデル](https://bigmodel.cn)
2. ミラーサイトおよびサードパーティのプロキシサービスの設定をサポート。
3. **ロードバランシング**による複数チャンネルへのアクセスをサポート。
4. ストリーム伝送によるタイプライター的効果を可能にする**ストリームモード**に対応。
5. 許可された IP 範囲とアクセス可能なモデルを設定できる**トークン管理**に対応。
6. **チャンネル管理**に対応し、チャンネルの一括作成が可能。
7. **ユーザーグループ**と**チャンネルグループ**をサポート。
8. チャンネル**モデルリスト設定**に対応。
9. **クォータ詳細チェック**をサポート。
10. モデルマッピングに対応し、ユーザーのリクエストモデルをリダイレクトします。必要がない限り設定しないでください。設定するとリクエストボディが再構築され、直接透過されなくなり、まだ正式にサポートされていない一部のフィールドが渡せなくなる可能性があります。
11. 失敗時の自動リトライをサポート。
12. 画像生成 API をサポート。
13. [Cloudflare AI Gateway](https://developers.cloudflare.com/ai-gateway/providers/openai/) をサポート。チャンネル設定のプロキシ部分に `https://gateway.ai.cloudflare.com/v1/ACCOUNT_TAG/GATEWAY/openai` を入力してください。
14. 豊富な**カスタマイズ**オプションを提供:
    1. システム名、ロゴ、フッターのカスタマイズが可能。
15. システムアクセストークンによる管理 API アクセスをサポートし、二次開発なしで My API の拡張とカスタマイズが可能。詳細は [API ドキュメント](./docs/API.md) を参照してください。
16. Cloudflare Turnstile によるユーザー認証に対応。
17. ユーザー管理と複数のユーザーログイン/登録方法をサポート:
    + メールによるログイン/登録（メールホワイトリスト対応）とパスワードリセット。

## デプロイメント
### Docker デプロイメント

デプロイコマンド:
`docker run --name myapi -d --restart always -p 3000:3000 -e TZ=Asia/Shanghai -v /home/ubuntu/data/myapi:/data pai801/myapi`。

コマンドを更新する: `docker run --rm -v /var/run/docker.sock:/var/run/docker.sock containrr/watchtower -cR`。

`-p 3000:3000` の最初の `3000` はホストのポートで、必要に応じて変更できます。

データはホストの `/home/ubuntu/data/myapi` ディレクトリに保存される。このディレクトリが存在し、書き込み権限があることを確認する、もしくは適切なディレクトリに変更してください。

Nginxリファレンス設定:
```
server{
   server_name your-domain.com;  # ドメイン名は適宜変更

   location / {
          client_max_body_size  64m;
          proxy_http_version 1.1;
          proxy_pass http://localhost:3000;  # それに応じてポートを変更
          proxy_set_header Host $host;
          proxy_set_header X-Forwarded-For $remote_addr;
          proxy_cache_bypass $http_upgrade;
          proxy_set_header Accept-Encoding gzip;
          proxy_read_timeout 300s;  # GPT-4 はより長いタイムアウトが必要
   }
}
```

次に、Let's Encrypt certbot を使って HTTPS を設定します:
```bash
# Ubuntu に certbot をインストール:
sudo snap install --classic certbot
sudo ln -s /snap/bin/certbot /usr/bin/certbot
# 証明書の生成と Nginx 設定の変更
sudo certbot --nginx
# プロンプトに従う
# Nginx を再起動
sudo service nginx restart
```

初期アカウントのユーザー名は `root` で、パスワードは `123456` です。

### マニュアルデプロイ
1. [GitHub Releases](https://github.com/pai801/myapi/releases/latest) から実行ファイルをダウンロードする、もしくはソースからコンパイルする:
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
3. [http://localhost:3000/](http://localhost:3000/) にアクセスし、ログインする。初期アカウントのユーザー名は `root`、パスワードは `123456` である。

Please refer to the [environment variables](#environment-variables) section for details on using environment variables.

## コンフィグ
システムは箱から出してすぐに使えます。

環境変数やコマンドラインパラメータを設定することで、システムを構成することができます。

システム起動後、`root` ユーザーとしてログインし、さらにシステムを設定します。

## 使用方法
`Channels` ページで API Key を追加し、`Tokens` ページでアクセストークンを追加する。

アクセストークンを使って My API にアクセスすることができる。使い方は [OpenAI API](https://platform.openai.com/docs/api-reference/introduction) と同じです。

OpenAI API が使用されている場所では、API Base に My API のデプロイアドレスを設定することを忘れないでください（例: `https://your-domain.com`）。API Key は My API で生成されたトークンでなければなりません。

具体的な API Base のフォーマットは、使用しているクライアントに依存することに注意してください。

```mermaid
graph LR
    A(ユーザ)
    A --->|リクエスト| B(My API)
    B -->|中継リクエスト| C(OpenAI)
    B -->|中継リクエスト| D(Azure)
    B -->|中継リクエスト| E(その他のダウンストリームチャンネル)
```

現在のリクエストにどのチャネルを使うかを指定するには、トークンの後に チャネル ID を追加します： 例えば、`Authorization: Bearer MY_API_KEY-CHANNEL_ID` のようにします。
チャンネル ID を指定するためには、トークンは管理者によって作成される必要があることに注意してください。

もしチャネル ID が指定されない場合、ロードバランシングによってリクエストが複数のチャネルに振り分けられます。

### 環境変数
1. `REDIS_CONN_STRING`: 設定すると、リクエストレート制限のためのストレージとして、メモリの代わりに Redis が使われる。
    + 例: `REDIS_CONN_STRING=redis://default:redispw@localhost:49153`
2. `SESSION_SECRET`: 設定すると、固定セッションキーが使用され、システムの再起動後もログインユーザーのクッキーが有効であることが保証されます。
    + 例: `SESSION_SECRET=random_string`
3. `SQL_DSN`: 設定すると、SQLite の代わりに指定したデータベースが使用されます。MySQL バージョン 8.0 を使用してください。
    + 例: `SQL_DSN=root:123456@tcp(localhost:3306)/myapi`
4. `LOG_SQL_DSN`: を設定すると、`logs`テーブルには独立したデータベースが使用されます。MySQLまたはPostgreSQLを使用してください。
5. `FRONTEND_BASE_URL`: 設定されると、バックエンドアドレスではなく、指定されたフロントエンドアドレスが使われる。
    + 例: `FRONTEND_BASE_URL=https://your-domain.com`
6. `SYNC_FREQUENCY`: 設定された場合、システムは定期的にデータベースからコンフィグを秒単位で同期する。設定されていない場合、同期は行われません。
    + 例: `SYNC_FREQUENCY=60`
8. `CHANNEL_UPDATE_FREQUENCY`: 設定すると、チャンネル残高を分単位で定期的に更新する。設定されていない場合、更新は行われません。
    + 例: `CHANNEL_UPDATE_FREQUENCY=1440`
9. `CHANNEL_TEST_FREQUENCY`: 設定すると、チャンネルを定期的にテストする。設定されていない場合、テストは行われません。
    + 例: `CHANNEL_TEST_FREQUENCY=1440`
10. `POLLING_INTERVAL`: チャネル残高の更新とチャネルの可用性をテストするときのリクエスト間の時間間隔 (秒)。デフォルトは間隔なし。
    + 例: `POLLING_INTERVAL=5`

### コマンドラインパラメータ
1. `--port <port_number>`: サーバがリッスンするポート番号を指定。デフォルトは `3000` です。
    + 例: `--port 3000`
2. `--log-dir <log_dir>`: ログディレクトリを指定。設定しない場合、ログは保存されません。
    + 例: `--log-dir ./logs`
3. `--version`: システムのバージョン番号を表示して終了する。
4. `--help`: コマンドの使用法ヘルプとパラメータの説明を表示。

## 注
本プロジェクトは One API (MIT) からフォークしたもので、MIT ライセンスを保持しています。
