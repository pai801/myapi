#!/usr/bin/env python3
"""affinity_smoke_upstream.py —— 渠道亲和冒烟测试用的假上游（mock upstream）。

作用
====
渠道亲和是「同一业务会话连续打到同一上游渠道」的选路优化。要验证它，需要
让 myapi 真正把请求转发出去，再统计「哪个上游收到了请求」。本脚本就是一个
可被 myapi 当作 OpenAI 兼容上游的极简 HTTP 服务：

  * 监听 127.0.0.1:<port>，任意路径都以 HTTP 200 返回一个**合法的 OpenAI
    非流式 chat completion** JSON（myapi 的计费/解析链路能正常消费）；
  * 每收到一个请求，就往 <log> 追加一行 JSON（含时间、方法、路径），
    这是冒烟脚本统计「本组请求落到了哪个上游」的唯一判据。

设计约束
========
  * 只用 Python 标准库，不装任何第三方依赖（CI / 干净机器可直接跑）；
  * 用 ThreadingHTTPServer 以便并发请求不会互相阻塞；
  * 显式设置 Content-Length，兼容 HTTP/1.1 keep-alive 的 Go http.Client；
  * 收到 SIGTERM/SIGINT 时正常退出，避免冒烟脚本清理阶段留下僵尸端口。

用法
====
    python3 affinity_smoke_upstream.py --port 18301 --log /tmp/upA.log --name A
"""

import argparse
import json
import signal
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# 进程级日志写锁：并发请求追加同一文件时必须串行，否则行会交错。
_LOG_LOCK = threading.Lock()


def _build_chat_completion() -> bytes:
    """返回一个最小但合法的 OpenAI 非流式 chat completion 响应体。"""
    payload = {
        "id": "chatcmpl-affinity-smoke",
        "object": "chat.completion",
        "created": int(time.time()),
        "model": "gpt-3.5-turbo",
        "choices": [
            {
                "index": 0,
                "message": {"role": "assistant", "content": "ok"},
                "finish_reason": "stop",
            }
        ],
        "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
    }
    return json.dumps(payload).encode("utf-8")


def make_handler(name: str, log_path: str):
    response_body = _build_chat_completion()

    class Handler(BaseHTTPRequestHandler):
        # HTTP/1.1 + 显式 Content-Length，保证 Go http.Client 的 keep-alive 正常复用连接
        protocol_version = "HTTP/1.1"

        def _record(self):
            length = int(self.headers.get("Content-Length") or 0)
            if length > 0:
                # 必须读完 body，否则 keep-alive 下连接会错位
                self.rfile.read(length)
            record = {
                "ts": time.strftime("%Y-%m-%dT%H:%M:%S"),
                "upstream": name,
                "method": self.command,
                "path": self.path,
            }
            with _LOG_LOCK:
                with open(log_path, "a", encoding="utf-8") as fh:
                    fh.write(json.dumps(record) + "\n")

        def _respond(self, body: bytes):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_POST(self):  # noqa: N802 (stdlib 约定的方法名)
            self._record()
            self._respond(response_body)

        def do_GET(self):  # noqa: N802
            self._record()
            self._respond(b'{"ok":true}')

        def log_message(self, *args):  # 静默默认的 stderr 访问日志
            return

    return Handler


def main() -> int:
    parser = argparse.ArgumentParser(description="渠道亲和冒烟测试用的假上游")
    parser.add_argument("--port", type=int, required=True, help="监听端口")
    parser.add_argument("--log", required=True, help="请求记录追加到的文件路径")
    parser.add_argument("--name", default="upstream", help="上游标识（写入日志用）")
    args = parser.parse_args()

    # 启动时清空日志，避免上一轮残留污染统计
    open(args.log, "w", encoding="utf-8").close()

    server = ThreadingHTTPServer(("127.0.0.1", args.port), make_handler(args.name, args.log))
    server.daemon_threads = True

    def _shutdown(signum, _frame):
        # 在信号处理里直接 shutdown 会与 serve_forever 同线程死锁，
        # 故放到单独线程执行。
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, _shutdown)
    signal.signal(signal.SIGINT, _shutdown)

    try:
        server.serve_forever()
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
