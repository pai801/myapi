#!/usr/bin/env bash
#
# affinity_smoke.sh —— 渠道亲和「真实流量」一键冒烟验证
#
# 作用
# ====
# 渠道亲和 = 同一业务会话连续打同一上游渠道；键分三层 turn → session → user。
# 此前所有验证都在单元/中间件层，没有真实流量验证。本脚本把「起真实服务、起假上游、
# 发真实 HTTP 请求、统计落点」这一整套过程固化下来，让以后每次改亲和都能一键验证：
#
#   构建二进制 → 建临时工作区 → 准备临时 DB → 起两个 mock 上游 → 起真实服务
#   → 发四组请求 → 统计每个上游收到的请求数 → 打印结果表与四条结论 → 清理进程。
#
# 四组请求（默认每组重复 5 次）与预期
# ==================================
#   组1  固定 X-Session-Id + 固定 X-Conversation-Request-Id → 应集中在同一上游（turn 层命中）
#   组2  固定 X-Session-Id + 每轮换新 turn 头           → 仍应集中在同一上游（session 层兜底）
#   组3  不带任何会话头                                 → 仍应集中在同一上游（user 层兜底，
#                                                          因为成功转发会写入全部可用层）
#   组4  X-Session-Id: sess-B（不同会话）               → 允许分散到不同上游（削热点的预期行为，
#                                                          不是缺陷）
#
# 关键实现约束（都是踩过的坑，勿改）
# ==================================
#   * 临时 DB **默认走「空库自迁移 + 自播种」**，不依赖仓库里被 .gitignore 忽略的 myapi.db，
#     全新 clone 上也能一键跑。做法（两段式启动）：先用 INITIAL_ROOT_TOKEN 让服务自己在
#     空库上建表并创建 root 用户（自带 quota，不会 403）与该 token → 优雅停掉 → 用 sqlite3
#     播种两个 mock 渠道与 abilities → 再正式启动（服务启动时构建 channel/token 缓存，
#     启动后写库不生效，所以必须先播完再起）。
#     想沿用既有库时，用 AFFINITY_SMOKE_BASE_DB=<path> 显式指定（旧行为）。
#     （注：以前「必须用未迁移过的新副本」是为了绕开 SQLite 第二次启动必 FATAL 的缺陷，
#      该缺陷已于 2026-09-20 修复，见 model/sqlite_migrate_fix.go，现不再需要绕。）
#   * 必须显式 `SQL_DSN=`（空）：仓库根有 .env 且 app 通过 godotenv/autoload 加载它，
#     其中 SQL_DSN 指向 Postgres；不置空会把服务指到 Postgres，SQLITE_PATH 被忽略，
#     测试 token 查不到 → 401 "无效的令牌"。脚本同时把工作目录切到临时目录（无 .env）。
#   * 服务必须设 JWT_SECRET，否则 FATAL 拒绝启动（默认密钥可被伪造 root JWT）。
#   * 渠道候选集依赖 abilities 表；只插 channels 会得到 "no channels available"。
#   * 渠道匹配走 models_alias（= 模型名的简化别名），只插 models 会得到
#     "no channel found for model"。
#   * 沙箱会拦回环网络：curl 必须加 --noproxy '*'，且脚本要在禁用沙箱的方式下运行。
#
# 用法
# ====
#   ./scripts/affinity_smoke.sh                 # 一键运行，零配置
#   AFFINITY_SMOKE_REPEATS=10 ./scripts/affinity_smoke.sh
#
# 可覆盖的环境变量（默认值见配置区）：
#   AFFINITY_SMOKE_REPEATS      每组重复次数（默认 5）
#   AFFINITY_SMOKE_MODEL        请求模型名（默认 gpt-3.5-turbo）
#   AFFINITY_SMOKE_SVC_PORT     服务端口（默认 0 = 自动挑空闲端口）
#   AFFINITY_SMOKE_A_PORT       上游 A 端口（默认 0 = 自动挑）
#   AFFINITY_SMOKE_B_PORT       上游 B 端口（默认 0 = 自动挑）
#   AFFINITY_SMOKE_BASE_DB      用作副本来源的 SQLite 库（默认 <repo>/myapi.db）
#   AFFINITY_KEY_MODE           透传给服务；=user 时亲和退化为只用 user 层。
#                               这是一个**反向证伪开关**：设成 user 后 turn/session 层
#                               命中数必然为 0，本脚本必须判 FAIL（用来证明层级断言不是恒真）
#   AFFINITY_SMOKE_TOKEN        指定测试 token（默认从库中读取 status=1 的首个 token）
#   AFFINITY_SMOKE_READY_TIMEOUT 服务就绪等待秒数（默认 40）
#   GO / PYTHON / SQLITE3       对应可执行文件（默认 go / python3 / sqlite3）
#
# 退出码：0 全部预期成立 / 1 有预期不成立 / 2 环境或前置条件错误 / 3 执行过程出错
#
set -euo pipefail

# =============================================================================
# ① 配置区
# =============================================================================
REPEATS="${AFFINITY_SMOKE_REPEATS:-5}"
MODEL="${AFFINITY_SMOKE_MODEL:-gpt-3.5-turbo}"
SVC_PORT="${AFFINITY_SMOKE_SVC_PORT:-0}"
A_PORT="${AFFINITY_SMOKE_A_PORT:-0}"
B_PORT="${AFFINITY_SMOKE_B_PORT:-0}"
READY_TIMEOUT="${AFFINITY_SMOKE_READY_TIMEOUT:-40}"

GO_BIN="${GO:-go}"
PYTHON_BIN="${PYTHON:-python3}"
SQLITE3_BIN="${SQLITE3:-sqlite3}"

# 本机 go 可能不在默认 PATH
if ! command -v "$GO_BIN" >/dev/null 2>&1 && [ -x /opt/homebrew/bin/go ]; then
  GO_BIN=/opt/homebrew/bin/go
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BASE_DB="${AFFINITY_SMOKE_BASE_DB:-$REPO_ROOT/myapi.db}"
MOCK_PY="$SCRIPT_DIR/affinity_smoke_upstream.py"

# =============================================================================
# ② 日志与退出码
# =============================================================================
EXIT_FAIL=1     # 预期不成立
EXIT_CONFIG=2   # 环境/前置错误
EXIT_RUN=3      # 执行出错

# 哨兵：只有正常走完「四条预期判定」并准备 exit 0 时才置 1。
# 用途：bash 3.2 在 UTF-8 locale 下遇「$VAR 紧跟多字节字符」会 unbound variable 崩溃，
# 而 cleanup() 的 EXIT trap 会把崩溃的退出码吞成 0（虚假通过）。cleanup 靠本变量
# 把「rc=0 但没走完判定」的退出强制成 EXIT_FAIL，避免冒烟假绿。
COMPLETED=0

if [ -t 1 ] && [ "${TERM:-}" != "dumb" ]; then
  C_RESET=$'\033[0m'; C_RED=$'\033[0;31m'; C_GREEN=$'\033[0;32m'
  C_YELLOW=$'\033[0;33m'; C_BLUE=$'\033[0;36m'; C_BOLD=$'\033[1m'
else
  C_RESET=''; C_RED=''; C_GREEN=''; C_YELLOW=''; C_BLUE=''; C_BOLD=''
fi
info()  { printf '%s[信息]%s %s\n' "$C_BLUE"   "$C_RESET" "$*"; }
ok()    { printf '%s[完成]%s %s\n' "$C_GREEN"  "$C_RESET" "$*"; }
warn()  { printf '%s[警告]%s %s\n' "$C_YELLOW" "$C_RESET" "$*" >&2; }
error() { printf '%s[错误]%s %s\n' "$C_RED"    "$C_RESET" "$*" >&2; }

# =============================================================================
# ③ 清理
# =============================================================================
WORK=""
SVC_PID=""
MOCK_A_PID=""
MOCK_B_PID=""

cleanup() {
  local rc=$?
  # 哨兵校验：rc 为 0 但没走完判定 = 中途崩溃（如 unbound variable）被 trap 吞成 0，
  # 必须翻成失败，否则冒烟会「假绿」。rc 非 0 时（正常失败 EXIT_FAIL/EXIT_CONFIG/EXIT_RUN，
  # 或 SIGINT/SIGTERM 的 130）一律保持原值，绝不覆盖。
  if [ "$rc" -eq 0 ] && [ "${COMPLETED:-0}" -ne 1 ]; then
    error "脚本未正常走完判定即退出（rc=0 但 COMPLETED!=1），判定为失败" || true
    rc="$EXIT_FAIL"
  fi
  # 优雅停止（SIGTERM）后等待退出，避免留下半写状态的临时库/端口
  for pid in "$SVC_PID" "$MOCK_A_PID" "$MOCK_B_PID"; do
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill -TERM "$pid" 2>/dev/null || true
    fi
  done
  for pid in "$SVC_PID" "$MOCK_A_PID" "$MOCK_B_PID"; do
    if [ -n "$pid" ]; then
      local i=0
      while [ "$i" -lt 20 ] && kill -0 "$pid" 2>/dev/null; do
        sleep 0.1; i=$((i + 1))
      done
      kill -9 "$pid" 2>/dev/null || true
    fi
  done
  if [ -n "$WORK" ] && [ -d "$WORK" ]; then
    rm -rf "$WORK" 2>/dev/null || true
  fi
  # 必须用 exit 而非 return 落地退出码：bash 3.2 的 EXIT trap 会忽略处理函数的 return 值
  # （实测 `cleanup(){ return 5; }` + `true` 仍退出 0），用 return 会让上面的哨兵只打印不生效，
  # 崩溃照样被吞成 exit 0。exit 不会递归触发本 trap（实测只进一次）。
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# =============================================================================
# ④ 前置检查
# =============================================================================
die_config() { error "$*"; exit "$EXIT_CONFIG"; }

# 等待服务就绪：轮询 /api/status，进程先退出或超时都算失败
# 用法：wait_service_ready <pid> <logfile>
wait_service_ready() {
  local pid="$1" log="$2" i=0 code
  while [ "$i" -lt "$READY_TIMEOUT" ]; do
    if ! kill -0 "$pid" 2>/dev/null; then
      return 1
    fi
    code="$(curl -s --noproxy '*' -o /dev/null -w '%{http_code}' "http://127.0.0.1:$SVC_PORT/api/status" 2>/dev/null || true)"
    if [ "$code" = "200" ]; then
      return 0
    fi
    sleep 0.5; i=$((i + 1))
  done
  warn "等待服务就绪超时（${READY_TIMEOUT}s），日志: $log"
  return 1
}

case "$REPEATS" in ''|*[!0-9]*) die_config "AFFINITY_SMOKE_REPEATS 必须是正整数，当前: $REPEATS";; esac
# 层级断言要求每组至少 2 次：每组第 1 次必然是 miss（还没有任何亲和键），
# 只有第 2 次起才能证明「命中了某个层级」。REPEATS=1 时层级断言无从判断。
[ "$REPEATS" -ge 2 ] || die_config "AFFINITY_SMOKE_REPEATS 必须 >= 2（层级断言需要每组至少 2 次），当前: $REPEATS"

command -v "$GO_BIN" >/dev/null 2>&1 || die_config "找不到 go（可用 GO=/path/to/go 指定）"
command -v "$PYTHON_BIN" >/dev/null 2>&1 || die_config "找不到 python3（可用 PYTHON=/path 指定）"
command -v "$SQLITE3_BIN" >/dev/null 2>&1 || die_config "找不到 sqlite3（用于准备临时 DB）"
command -v curl >/dev/null 2>&1 || die_config "找不到 curl"
[ -f "$MOCK_PY" ] || die_config "缺少 mock 上游脚本: $MOCK_PY"
# 基准库只在显式指定时才要求存在；默认走空库自播种，不依赖它
if [ -n "${AFFINITY_SMOKE_BASE_DB:-}" ]; then
  [ -f "$BASE_DB" ] || die_config "基准 SQLite 库不存在: $BASE_DB"
fi

pick_port() {
  "$PYTHON_BIN" -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}
[ "$SVC_PORT" = "0" ] && SVC_PORT="$(pick_port)"
[ "$A_PORT" = "0" ] && A_PORT="$(pick_port)"
[ "$B_PORT" = "0" ] && B_PORT="$(pick_port)"
[ "$SVC_PORT" != "$A_PORT" ] && [ "$SVC_PORT" != "$B_PORT" ] && [ "$A_PORT" != "$B_PORT" ] \
  || die_config "端口冲突: svc=$SVC_PORT A=$A_PORT B=$B_PORT"

# 模型名 → 渠道 models_alias（与 model.SimplifyModelName 同构：取 / 之后、去非字母数字、转小写）
simplify_model() {
  local m="$1"
  m="${m##*/}"
  printf '%s' "$m" | tr 'A-Z' 'a-z' | tr -cd 'a-z0-9'
}
ALIAS="$(simplify_model "$MODEL")"
[ -n "$ALIAS" ] || die_config "无法从模型名 $MODEL 推导别名"

# =============================================================================
# ⑤ 建临时工作区
# =============================================================================
WORK="$(mktemp -d "${TMPDIR:-/tmp}/affinity-smoke.XXXXXX")" || die_config "无法创建临时目录"
DB="$WORK/affinity.db"
SVC_LOG="$WORK/service.log"
UP_A_LOG="$WORK/upstream_a.log"
UP_B_LOG="$WORK/upstream_b.log"
RESP_FILE="$WORK/last_response.json"
BIN="$WORK/myapi"

info "工作区: $WORK"
info "模型:   $MODEL (alias=$ALIAS)   重复: $REPEATS 次/组"
info "端口:   svc=$SVC_PORT  A=$A_PORT  B=$B_PORT"

# =============================================================================
# ⑥ 构建
# =============================================================================
info "构建二进制…"
if ! ( cd "$REPO_ROOT" && "$GO_BIN" build -o "$BIN" . ) >"$WORK/build.log" 2>&1; then
  error "构建失败。输出："
  cat "$WORK/build.log" >&2
  exit "$EXIT_RUN"
fi
ok "构建完成: $BIN"

# =============================================================================
# ⑦ 准备临时 DB
#    默认「空库自迁移 + 自播种」；设了 AFFINITY_SMOKE_BASE_DB 才复制既有库。
# =============================================================================
SELF_SEED=1
if [ -n "${AFFINITY_SMOKE_BASE_DB:-}" ]; then
  SELF_SEED=0
  [ -f "$BASE_DB" ] || die_config "基准 SQLite 库不存在: $BASE_DB"
  cp "$BASE_DB" "$DB" || { error "复制基准库失败"; exit "$EXIT_RUN"; }
  rm -f "$DB-wal" "$DB-shm"
  info "DB 来源: 基准库副本 $BASE_DB"
else
  : >"$DB"   # 空库，交给服务自己迁移
  info "DB 来源: 空库自迁移 + 自播种（不依赖仓库 myapi.db）"
fi

# --- 自播种模式：先让服务把表建好、把 root 用户和 token 创建出来，再停掉 ---
if [ "$SELF_SEED" -eq 1 ]; then
  # 注意：库里存的 key **不能带 sk- 前缀** —— middleware/auth.go:146 会把请求里的 "sk-"
  # 剥掉再查库（key = strings.TrimPrefix(key, "sk-")），带前缀存进去反而永远查不到、全部 401。
  SMOKE_TOKEN="affinitysmoketoken000000000000000000000000000000000"   # 48 位，对齐 Token.Key 的 char(48)
  BOOT_LOG="$WORK/bootstrap.log"
  info "空库引导启动（建表 + 创建 root 用户/token）…"
  # exec 必须有：否则 $! 拿到的是子 shell 的 PID，而真正的服务是 env 起的孙进程，
  # 后面 kill -TERM 只会杀掉子 shell、把服务留成孤儿继续占着端口，正式启动那次就会
  # "bind: address already in use" 直接 FATAL（本次改造踩到的就是这个）。
  (
    cd "$WORK" || exit 1
    exec env -i \
      PATH="$PATH" \
      HOME="$HOME" \
      JWT_SECRET="affinity-smoke-jwt-secret" \
      SESSION_SECRET="affinity-smoke-session-secret" \
      SQL_DSN="" \
      LOG_SQL_DSN="" \
      SQLITE_PATH="$DB" \
      INITIAL_ROOT_TOKEN="$SMOKE_TOKEN" \
      PORT="$SVC_PORT" \
      "$BIN"
  ) >"$BOOT_LOG" 2>&1 &
  BOOT_PID=$!
  if ! wait_service_ready "$BOOT_PID" "$BOOT_LOG"; then
    error "空库引导启动失败。日志尾部："
    tail -n 30 "$BOOT_LOG" >&2 || true
    exit "$EXIT_RUN"
  fi
  # 优雅停止后再播种：服务在启动时构建 channel/token 缓存，启动后写库不生效
  kill -TERM "$BOOT_PID" 2>/dev/null || true
  bi=0
  while [ "$bi" -lt 40 ] && kill -0 "$BOOT_PID" 2>/dev/null; do
    sleep 0.1; bi=$((bi + 1))
  done
  kill -9 "$BOOT_PID" 2>/dev/null || true
  wait "$BOOT_PID" 2>/dev/null || true
  # 必须确认真的死了：它还活着就会占着端口，正式启动那次会 "bind: address already in use" FATAL，
  # 而请求会被这个残留进程接走、日志写进 bootstrap.log，表现为「命中全 0」这种极难排查的假象。
  if kill -0 "$BOOT_PID" 2>/dev/null; then
    error "空库引导服务未能停止（pid=${BOOT_PID}），端口仍被占用"
    exit "$EXIT_RUN"
  fi
  ok "空库引导完成（已建表并创建 root 用户/token）"
fi

if ! "$SQLITE3_BIN" "$DB" >"$WORK/dbprep.log" 2>&1 <<SQL
-- 清空渠道/能力，让候选集只含本次两个 mock 上游，测试完全自洽
DELETE FROM abilities;
DELETE FROM channels;
INSERT INTO channels (id,type,key,status,name,weight,created_time,base_url,models,"group",priority,models_alias)
  VALUES (101,1,'sk-mock-a',1,'smoke-upstream-A',1,0,'http://127.0.0.1:${A_PORT}','${MODEL}','default',1,'${ALIAS}');
INSERT INTO channels (id,type,key,status,name,weight,created_time,base_url,models,"group",priority,models_alias)
  VALUES (102,1,'sk-mock-b',1,'smoke-upstream-B',1,0,'http://127.0.0.1:${B_PORT}','${MODEL}','default',1,'${ALIAS}');
INSERT INTO abilities ("group",model,channel_id,enabled,priority) VALUES ('default','${MODEL}',101,1,1);
INSERT INTO abilities ("group",model,channel_id,enabled,priority) VALUES ('default','${MODEL}',102,1,1);
SQL
then
  error "准备临时 DB 失败。输出："
  cat "$WORK/dbprep.log" >&2
  exit "$EXIT_RUN"
fi

# 解析测试 token：优先环境变量；其次自播种模式用引导时写入的那个；其次库里 status=1 的首个 token
TOKEN="${AFFINITY_SMOKE_TOKEN:-}"
if [ -z "$TOKEN" ] && [ "$SELF_SEED" -eq 1 ]; then
  TOKEN="$SMOKE_TOKEN"
fi
if [ -z "$TOKEN" ]; then
  TOKEN="$("$SQLITE3_BIN" "$DB" "SELECT key FROM tokens WHERE status=1 ORDER BY id LIMIT 1;" 2>/dev/null | head -n1)"
fi
if [ -z "$TOKEN" ]; then
  TOKEN="affinitysmoketoken000000000000000000000000000000000"
  "$SQLITE3_BIN" "$DB" "INSERT INTO tokens (id,user_id,key,status,name,created_time,accessed_time,models,subnet) VALUES (1,1,'$TOKEN',1,'smoke',0,0,'','');" \
    || { error "库中无可用 token 且兜底插入失败"; exit "$EXIT_RUN"; }
  warn "库中无可用 token，已兜底插入一个测试 token"
fi
# 确认用户可用（status=1 且存在）
USER_OK="$("$SQLITE3_BIN" "$DB" "SELECT count(*) FROM users WHERE status=1;" 2>/dev/null)"
[ "${USER_OK:-0}" -ge 1 ] || { error "库中没有 status=1 的用户，无法通过鉴权"; exit "$EXIT_RUN"; }
ok "临时 DB 就绪（channels 101/102 + abilities，token=${TOKEN:0:8}…）"

# =============================================================================
# ⑧ 起两个 mock 上游
# =============================================================================
"$PYTHON_BIN" "$MOCK_PY" --port "$A_PORT" --log "$UP_A_LOG" --name A >"$WORK/upA.stderr" 2>&1 &
MOCK_A_PID=$!
"$PYTHON_BIN" "$MOCK_PY" --port "$B_PORT" --log "$UP_B_LOG" --name B >"$WORK/upB.stderr" 2>&1 &
MOCK_B_PID=$!
sleep 1
for pid in "$MOCK_A_PID" "$MOCK_B_PID"; do
  kill -0 "$pid" 2>/dev/null || { error "mock 上游启动失败 (pid=$pid)，见 $WORK/up*.stderr"; cat "$WORK/upA.stderr" "$WORK/upB.stderr" >&2 || true; exit "$EXIT_RUN"; }
done
ok "mock 上游已启动 (A:$A_PORT  B:$B_PORT)"

# =============================================================================
# ⑨ 起真实服务
# =============================================================================
# 关键：SQL_DSN= 置空以压过仓库 .env 的 autoload；工作目录切到无 .env 的临时目录
# exec 必须有（理由同空库引导那处）：让 $! 就是服务进程本身，停止/清理才杀得准。
(
  cd "$WORK" || exit 1
  exec env -i \
    PATH="$PATH" \
    HOME="$HOME" \
    JWT_SECRET="affinity-smoke-jwt-secret" \
    SESSION_SECRET="affinity-smoke-session-secret" \
    SQL_DSN="" \
    LOG_SQL_DSN="" \
    SQLITE_PATH="$DB" \
    MEMORY_CACHE_ENABLED=true \
    DEBUG=true \
    AFFINITY_STATS_INTERVAL_SECONDS=1 \
    AFFINITY_KEY_MODE="${AFFINITY_KEY_MODE:-}" \
    GIN_MODE=debug \
    PORT="$SVC_PORT" \
    "$BIN"
) >"$SVC_LOG" 2>&1 &
SVC_PID=$!

info "等待服务就绪…"
if ! wait_service_ready "$SVC_PID" "$SVC_LOG"; then
  error "服务未在 ${READY_TIMEOUT}s 内就绪（/api/status 未返回 200）。服务日志尾部："
  tail -n 30 "$SVC_LOG" >&2 || true
  exit "$EXIT_RUN"
fi
ok "服务已就绪: http://127.0.0.1:$SVC_PORT"

# =============================================================================
# ⑩ 发请求
# =============================================================================
PAYLOAD="{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}"

count_log() {  # 统计某 mock 日志行数
  if [ -f "$1" ]; then wc -l <"$1" | tr -d ' '; else echo 0; fi
}

send_one() {  # 参数：附加请求头…  输出：HTTP 状态码
  local code
  code="$(curl -s --noproxy '*' -o "$RESP_FILE" -w '%{http_code}' \
    -X POST "http://127.0.0.1:$SVC_PORT/v1/chat/completions" \
    -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' \
    "$@" \
    --data-binary "$PAYLOAD" 2>/dev/null || echo "000")"
  printf '%s' "$code"
}

# 结果数组（bash 3.2：用普通数组 + 计数变量）
GRP_NAME=(); GRP_A=(); GRP_B=(); GRP_CODES=(); GRP_NON200=()
# 每组各自的服务日志命中计数（取组内增量，避免把上一组的命中算进来——
# 之前只统计全局总数，导致「turn 层命中成立」这句在 turn 实际为 0 时也照样打印）
GRP_TURN=(); GRP_SESSION=(); GRP_USER=(); GRP_MISS=()

# 日志行数统计（某条 pattern 在当前服务日志里出现的累计次数）
count_svc() {  # $1=ERE pattern
  if [ -f "$SVC_LOG" ]; then grep -cE "$1" "$SVC_LOG" 2>/dev/null || true; else echo 0; fi
}

run_group() {
  local label="$1"; shift
  local mode="$1"; shift   # mode 决定 turn 头如何构造
  local a0 b0 a1 b1 code i codes=""
  local non200=0
  local t0 s0 u0 m0 t1 s1 u1 m1
  a0="$(count_log "$UP_A_LOG")"; b0="$(count_log "$UP_B_LOG")"
  t0="$(count_svc 'affinity hit level=turn')"
  s0="$(count_svc 'affinity hit level=session')"
  u0="$(count_svc 'affinity hit level=user')"
  m0="$(count_svc 'no affinity for user')"

  i=1
  while [ "$i" -le "$REPEATS" ]; do
    case "$mode" in
      turn_fixed)
        code="$(send_one -H "X-Session-Id: sess-A" -H "X-Conversation-Request-Id: turn-1")" ;;
      turn_rotate)
        code="$(send_one -H "X-Session-Id: sess-A" -H "X-Conversation-Request-Id: turn-2-$i")" ;;
      no_headers)
        code="$(send_one)" ;;
      sess_b)
        code="$(send_one -H "X-Session-Id: sess-B" -H "X-Conversation-Request-Id: turn-4-$i")" ;;
      *)
        code="000" ;;
    esac
    codes="${codes}${code} "
    [ "$code" = "200" ] || non200=$((non200 + 1))
    i=$((i + 1))
  done

  a1="$(count_log "$UP_A_LOG")"; b1="$(count_log "$UP_B_LOG")"
  t1="$(count_svc 'affinity hit level=turn')"
  s1="$(count_svc 'affinity hit level=session')"
  u1="$(count_svc 'affinity hit level=user')"
  m1="$(count_svc 'no affinity for user')"
  GRP_NAME[${#GRP_NAME[@]}]="$label"
  GRP_A[${#GRP_A[@]}]=$((a1 - a0))
  GRP_B[${#GRP_B[@]}]=$((b1 - b0))
  GRP_CODES[${#GRP_CODES[@]}]="$codes"
  GRP_NON200[${#GRP_NON200[@]}]="$non200"
  GRP_TURN[${#GRP_TURN[@]}]=$((t1 - t0))
  GRP_SESSION[${#GRP_SESSION[@]}]=$((s1 - s0))
  GRP_USER[${#GRP_USER[@]}]=$((u1 - u0))
  GRP_MISS[${#GRP_MISS[@]}]=$((m1 - m0))

  if [ "$non200" -ne 0 ]; then
    warn "「${label}」出现非 200 响应：${codes}"
    if [ -s "$RESP_FILE" ]; then
      warn "最后一次响应体: $(head -c 300 "$RESP_FILE")"
    fi
  fi
}

info "发送四组请求（每组 $REPEATS 次）…"
run_group "组1 turn固定 + session固定" turn_fixed
run_group "组2 turn轮换 + session固定" turn_rotate
run_group "组3 无任何会话头"           no_headers
run_group "组4 另一会话 sess-B"        sess_b

# =============================================================================
# ⑪ 亲和日志统计
# =============================================================================
# 全局汇总 = 各组增量之和（不再直接 grep 全量日志，保证与分组口径一致）
sum_arr() {  # $1=数组名
  local name="$1" total=0 i=0 n
  eval "n=\${#${name}[@]}"
  while [ "$i" -lt "$n" ]; do
    eval "total=\$((total + ${name}[i]))"
    i=$((i + 1))
  done
  printf '%s' "$total"
}
L_TURN="$(sum_arr GRP_TURN)"
L_SESSION="$(sum_arr GRP_SESSION)"
L_USER="$(sum_arr GRP_USER)"
L_MISS="$(sum_arr GRP_MISS)"
STATS_LINE="$(grep -E 'affinity stats \(window' "$SVC_LOG" 2>/dev/null | tail -n1 || true)"

# =============================================================================
# ⑫ 打印结果表
# =============================================================================
echo
printf '%s==== 渠道亲和冒烟结果 ====%s\n' "$C_BOLD" "$C_RESET"
printf '%-30s %6s %6s %-14s %s\n' "分组" "上游A" "上游B" "命中 t/s/u" "HTTP状态"
printf -- '--------------------------------------------------------------------------------\n'
gi=0
while [ "$gi" -lt "${#GRP_NAME[@]}" ]; do
  printf '%-30s %6s %6s %-14s %s\n' \
    "${GRP_NAME[$gi]}" "${GRP_A[$gi]}" "${GRP_B[$gi]}" \
    "${GRP_TURN[$gi]}/${GRP_SESSION[$gi]}/${GRP_USER[$gi]}" "${GRP_CODES[$gi]}"
  gi=$((gi + 1))
done
printf -- '--------------------------------------------------------------------------------\n'
printf '  （t/s/u = 该组请求各自产生的 turn / session / user 层命中次数，取自服务日志增量）\n'

# 找组1命中的上游，作为「同一上游」基准
BASE_UP=""
if [ "${GRP_A[0]}" -gt 0 ] && [ "${GRP_B[0]}" -eq 0 ]; then BASE_UP="A"; fi
if [ "${GRP_B[0]}" -gt 0 ] && [ "${GRP_A[0]}" -eq 0 ]; then BASE_UP="B"; fi

# 判定：某组是否集中在单一上游
concentrated() {  # $1=组下标；集中返回 0
  local idx="$1"
  if [ "${GRP_A[$idx]}" -eq 0 ] && [ "${GRP_B[$idx]}" -gt 0 ]; then return 0; fi
  if [ "${GRP_B[$idx]}" -eq 0 ] && [ "${GRP_A[$idx]}" -gt 0 ]; then return 0; fi
  return 1
}
# 某组命中的上游名（用于和组1比对）
hit_upstream() {
  local idx="$1"
  if [ "${GRP_A[$idx]}" -ge "${GRP_B[$idx]}" ] && [ "${GRP_A[$idx]}" -gt 0 ]; then printf 'A'; else printf 'B'; fi
}

FAILED=0
verdict() {  # $1=是否成立(0/1) $2=文本
  if [ "$1" -eq 0 ]; then printf '  %s[PASS]%s %s\n' "$C_GREEN" "$C_RESET" "$2"; else printf '  %s[FAIL]%s %s\n' "$C_RED" "$C_RESET" "$2"; fi
}

echo
printf '%s==== 四条预期判定 ====%s\n' "$C_BOLD" "$C_RESET"

# 组1：集中 **且** turn 层确有命中。
# 只判断「集中」是不够的：把 AFFINITY_KEY_MODE 设成 user 时请求照样集中，
# 但 turn 层命中数是 0，原来那句「turn 层命中成立」就成了误导性通过。
# 每组第 1 次必然 miss（尚无任何亲和键），故期望命中数 = REPEATS-1。
G1_TURN_EXPECT=$((REPEATS - 1))
if concentrated 0 && [ "${GRP_TURN[0]}" -ge "$G1_TURN_EXPECT" ]; then
  verdict 0 "组1 集中在同一上游（$(hit_upstream 0)）且 turn 层命中 ${GRP_TURN[0]} 次（期望 >= ${G1_TURN_EXPECT}）——turn 层命中成立"
else
  verdict 1 "组1 turn 层命中不成立（集中=$(concentrated 0 && echo yes || echo no)，A=${GRP_A[0]} B=${GRP_B[0]}，turn=${GRP_TURN[0]} 期望 >= ${G1_TURN_EXPECT}）"
  FAILED=1
fi
# 组2：集中 **且** session 层确有命中（turn 每轮换新，只能靠 session 兜底）
G2_SESS_EXPECT=$((REPEATS - 1))
if concentrated 1 && [ "${GRP_SESSION[1]}" -ge "$G2_SESS_EXPECT" ]; then
  verdict 0 "组2 集中在同一上游（$(hit_upstream 1)）且 session 层命中 ${GRP_SESSION[1]} 次（期望 >= ${G2_SESS_EXPECT}）——session 层兜底成立"
else
  verdict 1 "组2 session 层兜底不成立（集中=$(concentrated 1 && echo yes || echo no)，A=${GRP_A[1]} B=${GRP_B[1]}，session=${GRP_SESSION[1]} 期望 >= ${G2_SESS_EXPECT}）"
  FAILED=1
fi
# 组3：集中 **且** user 层确有命中（无任何会话头，只能靠 user 兜底）
G3_USER_EXPECT=$((REPEATS - 1))
if concentrated 2 && [ "${GRP_USER[2]}" -ge "$G3_USER_EXPECT" ]; then
  verdict 0 "组3 集中在同一上游（$(hit_upstream 2)）且 user 层命中 ${GRP_USER[2]} 次（期望 >= ${G3_USER_EXPECT}）——user 层兜底成立"
else
  verdict 1 "组3 user 层兜底不成立（集中=$(concentrated 2 && echo yes || echo no)，A=${GRP_A[2]} B=${GRP_B[2]}，user=${GRP_USER[2]} 期望 >= ${G3_USER_EXPECT}）"
  FAILED=1
fi
# 组4：不要求与组1同上游；记录观测
if concentrated 3; then
  if [ -n "$BASE_UP" ] && [ "$(hit_upstream 3)" != "$BASE_UP" ]; then
    verdict 0 "组4 落在上游 $(hit_upstream 3)（与组1 的上游 $BASE_UP 不同）——不同会话允许分散，符合预期"
  else
    verdict 0 "组4 落在上游 $(hit_upstream 3)（与组1 相同）——不同会话不要求分散，未违反预期"
  fi
else
  verdict 0 "组4 分散在两个上游（A=${GRP_A[3]} B=${GRP_B[3]}）——削热点预期行为，不是缺陷"
fi

# 附加硬性条件：全部请求必须 200
TOTAL_NON200=0
gi=0
while [ "$gi" -lt "${#GRP_NON200[@]}" ]; do TOTAL_NON200=$((TOTAL_NON200 + GRP_NON200[gi])); gi=$((gi + 1)); done
if [ "$TOTAL_NON200" -eq 0 ]; then
  verdict 0 "全部请求返回 HTTP 200"
else
  verdict 1 "存在 $TOTAL_NON200 个非 200 响应（见上表与警告）"; FAILED=1
fi

echo
printf '%s==== 服务侧亲和命中统计（DEBUG 日志）====%s\n' "$C_BOLD" "$C_RESET"
printf '  affinity hit level=turn    : %s\n' "${L_TURN:-0}"
printf '  affinity hit level=session : %s\n' "${L_SESSION:-0}"
printf '  affinity hit level=user    : %s\n' "${L_USER:-0}"
printf '  affinity miss(no affinity) : %s\n' "${L_MISS:-0}"
if [ -n "$STATS_LINE" ]; then
  printf '  周期汇总: %s\n' "$STATS_LINE"
else
  printf '  周期汇总: （本窗口未触发输出，属正常；命中计数以上表为准）\n'
fi

echo
if [ "$FAILED" -eq 0 ]; then
  ok "渠道亲和冒烟通过：四组预期均成立。"
  COMPLETED=1
  exit 0
else
  error "渠道亲和冒烟未通过：存在不成立的预期（详见上表）。"
  error "服务日志: ${SVC_LOG}（脚本退出后临时目录会清理，如需保留请手动重跑并观察输出）"
  exit "$EXIT_FAIL"
fi
