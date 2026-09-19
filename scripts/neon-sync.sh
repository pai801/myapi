#!/usr/bin/env bash
#
# neon-sync.sh —— 把 Neon（云端 Postgres）一键同步到本地 docker postgres 容器
#
# 说明：
#   * 宿主机没有 postgres 客户端，所有 pg_* 命令都通过 docker exec 在容器内执行。
#   * 默认导出自定义格式 -Fc；并行度 JOBS>1（-j 或环境变量）时改用目录格式 -Fd -j N。
#   * Neon 免费层会 suspend，首次连接常失败，导出带冷启动重试。
#   * 日志里的连接串一律脱敏，密码不会打印。
#   * 兼容 macOS 自带 bash 3.2，不使用 bash 4+ 特性。
#   * 中文文案里引用变量一律写 ${VAR} 而不是 $VAR：UTF-8 locale 下 bash 会把紧跟
#     其后的中文标点算进变量名（"$LOCAL_DB。" 会被当成变量 "LOCAL_DB。"），在
#     set -u 下直接触发 unbound variable。C/POSIX locale 下不报错，极易漏测。
#
set -euo pipefail

# =============================================================================
# ① 配置区（改这里的默认值即可；环境变量同名可覆盖；命令行参数优先级最高）
# =============================================================================

# Neon 连接串，形如：
#   postgresql://user:pass@ep-xxx-123456.us-east-2.aws.neon.tech/neondb?sslmode=require
# 留空则必须用 -d/--dsn 或环境变量 NEON_DSN 传入。
NEON_DSN="${NEON_DSN:-}"

# 本地 docker postgres 容器名
LOCAL_CONTAINER="${LOCAL_CONTAINER:-app-pg}"
# 容器内的数据库用户
LOCAL_USER="${LOCAL_USER:-myapi_owner}"
# 本地目标库名（不存在会自动创建）
LOCAL_DB="${LOCAL_DB:-neon_copy}"
# 宿主机上的导出缓存目录（保留一份离线快照）
DUMP_DIR="${DUMP_DIR:-${HOME}/.cache/neon-sync}"
# 并行度：>1 时用目录格式 (-Fd -j N) 并行导出，否则用自定义格式 -Fc
JOBS="${JOBS:-1}"
# 只同步这些表（逗号或空格分隔）；留空 = 全库
TABLES="${TABLES:-}"

# =============================================================================
# ② 常量与退出码
# =============================================================================
EXIT_CONFIG=2      # 配置缺失或非法
EXIT_PRECHECK=3    # 前置检查失败（docker / 容器 / 建库）
EXIT_EXPORT=4      # 导出失败
EXIT_IMPORT=5      # 导入失败

CONTAINER_TMP="/tmp/neon-sync"   # 容器内固定临时目录，避免依赖宿主机挂载
CONTAINER_TMP_CREATED=0
START_TS="$(date +%s)"

# =============================================================================
# ③ 日志（非 TTY 或 TERM=dumb 时禁用颜色）
# =============================================================================
if [ -t 1 ] && [ "${TERM:-}" != "dumb" ]; then
  C_RESET=$'\033[0m'
  C_RED=$'\033[0;31m'
  C_GREEN=$'\033[0;32m'
  C_YELLOW=$'\033[0;33m'
  C_BLUE=$'\033[0;36m'
else
  C_RESET=''; C_RED=''; C_GREEN=''; C_YELLOW=''; C_BLUE=''
fi

info()  { printf '%s[信息]%s %s\n' "$C_BLUE"   "$C_RESET" "$*"; }
ok()    { printf '%s[完成]%s %s\n' "$C_GREEN"  "$C_RESET" "$*"; }
warn()  { printf '%s[警告]%s %s\n' "$C_YELLOW" "$C_RESET" "$*" >&2; }
error() { printf '%s[错误]%s %s\n' "$C_RED"    "$C_RESET" "$*" >&2; }

# =============================================================================
# ④ 工具函数
# =============================================================================

usage() {
  cat <<'EOF'
用法: neon-sync.sh [选项]

把 Neon（云端 Postgres）的数据同步到本地 docker postgres 容器。
默认导出为自定义格式 -Fc；并行度 >1（-j N 或环境变量 JOBS）时改用目录格式 -Fd 并行导出。

选项:
  -d, --dsn <dsn>        Neon 连接串（也可用环境变量 NEON_DSN）
  -c, --container <名>   本地容器名（默认 app-pg）
  -U, --user <名>        容器内数据库用户（默认 myapi_owner）
  -D, --database <库>    本地目标库名（默认 neon_copy，不存在会自动创建）
  -t, --table <表>       只同步指定表，可重复多次
  -s, --schema-only      只同步结构
  -a, --data-only        只同步数据
  -j, --jobs <n>         并行度，>1 时用目录格式（默认 1，即自定义格式 -Fc）
  -f, --force            跳过交互确认
  -h, --help             显示本帮助

退出码: 0 成功 / 2 配置错误 / 3 前置检查失败 / 4 导出失败 / 5 导入失败
EOF
}

# 选项缺参时统一报错退出
need_arg() {
  # $1: 选项名；$2: 当前剩余参数个数
  if [ "$2" -lt 2 ]; then
    error "选项 $1 缺少参数"
    usage >&2
    exit "$EXIT_CONFIG"
  fi
}

# 把 DSN 里的密码替换成 ***，避免泄漏到日志/终端
# 规则 1：贪婪匹配到最后一个 @，密码中含 @ 时不会漏出后半截
# 规则 2：兜底畸形 DSN（形如 scheme://user:pass 无 @/host），避免密码原样打印
mask_dsn() {
  printf '%s' "$1" | sed -E \
    -e 's|(://[^:/@]+):.*@|\1:***@|' \
    -e 's|(://[^:/@]+):[^@/]*$|\1:***|'
}

# 从 DSN 里取出数据库名（路径最后一段，去掉 query）
dsn_dbname() {
  local rest="${1#*://}"
  case "$rest" in
    */*) rest="${rest#*/}" ;;
    *)   rest="" ;;
  esac
  rest="${rest%%\?*}"
  rest="${rest%%/*}"
  printf '%s' "$rest"
}

# 把秒数格式化成 XmYs
fmt_dur() {
  printf '%dm%ds' "$(($1 / 60))" "$(($1 % 60))"
}

# 清理容器内临时 dump（只在真正创建过时执行）
cleanup() {
  if [ "$CONTAINER_TMP_CREATED" -eq 1 ]; then
    docker exec "$LOCAL_CONTAINER" rm -rf "$CONTAINER_TMP" >/dev/null 2>&1 || true
  fi
  if [ -n "${IMPORT_LOG:-}" ] && [ -f "$IMPORT_LOG" ]; then
    rm -f "$IMPORT_LOG" || true
  fi
}

# 目标库是否已存在（不拼 SQL，库名含引号也不会出问题）
# 注：macOS 自带 BSD grep 不支持 -w 与 -x 同时使用，故用 -qxF（awk 已 trim，整行精确匹配即可）
db_exists() {
  docker exec "$LOCAL_CONTAINER" psql -U "$LOCAL_USER" -d postgres -lqt 2>/dev/null \
    | awk -F'|' '{gsub(/ /,"",$1); print $1}' \
    | grep -qxF -- "$LOCAL_DB"
}

# 判断一行 pg_restore 输出是否属可忽略错误（Neon 常见：extension/角色/schema 本地缺失）。
# 不区分大小写；匹配返回 0，不匹配返回 1。
is_ignorable_error() {
  printf '%s\n' "$1" | grep -qiE \
    'extension|role ".*" does not exist|must be owner of|schema ".*" does not exist'
}

# =============================================================================
# ⑤ 命令行参数解析
# =============================================================================
SCHEMA_ONLY=0
DATA_ONLY=0
FORCE=0
TABLE_FROM_CLI=0
TABLE_LIST=()

while [ $# -gt 0 ]; do
  case "$1" in
    -d|--dsn)        need_arg "$1" "$#"; NEON_DSN="$2"; shift 2 ;;
    --dsn=*)         NEON_DSN="${1#*=}"; shift ;;
    -c|--container)  need_arg "$1" "$#"; LOCAL_CONTAINER="$2"; shift 2 ;;
    --container=*)   LOCAL_CONTAINER="${1#*=}"; shift ;;
    -U|--user)       need_arg "$1" "$#"; LOCAL_USER="$2"; shift 2 ;;
    --user=*)        LOCAL_USER="${1#*=}"; shift ;;
    -D|--database)   need_arg "$1" "$#"; LOCAL_DB="$2"; shift 2 ;;
    --database=*)    LOCAL_DB="${1#*=}"; shift ;;
    -t|--table)      need_arg "$1" "$#"; TABLE_LIST[${#TABLE_LIST[@]}]="$2"; TABLE_FROM_CLI=1; shift 2 ;;
    --table=*)       TABLE_LIST[${#TABLE_LIST[@]}]="${1#*=}"; TABLE_FROM_CLI=1; shift ;;
    -s|--schema-only) SCHEMA_ONLY=1; shift ;;
    -a|--data-only)   DATA_ONLY=1; shift ;;
    -j|--jobs)       need_arg "$1" "$#"; JOBS="$2"; shift 2 ;;
    --jobs=*)        JOBS="${1#*=}"; shift ;;
    -f|--force)      FORCE=1; shift ;;
    -h|--help)       usage; exit 0 ;;
    --)              shift; break ;;
    -*)              error "未知选项: $1"; usage >&2; exit "$EXIT_CONFIG" ;;
    *)               error "多余的位置参数: $1"; usage >&2; exit "$EXIT_CONFIG" ;;
  esac
done

if [ "$#" -gt 0 ]; then
  error "多余的位置参数: $*"
  usage >&2
  exit "$EXIT_CONFIG"
fi

# =============================================================================
# ⑥ 配置校验（退出码 2）
# =============================================================================
if [ -z "$NEON_DSN" ]; then
  error "未配置 Neon 连接串 NEON_DSN。"
  error "请用 -d/--dsn 传入，或填写脚本顶部配置区，或设置环境变量 NEON_DSN。"
  exit "$EXIT_CONFIG"
fi

case "$NEON_DSN" in
  postgresql://*|postgres://*) : ;;
  *) error "NEON_DSN 看起来不是 postgres 连接串（应以 postgresql:// 或 postgres:// 开头）"; exit "$EXIT_CONFIG" ;;
esac

if [ "$SCHEMA_ONLY" -eq 1 ] && [ "$DATA_ONLY" -eq 1 ]; then
  error "--schema-only 与 --data-only 不能同时使用"
  exit "$EXIT_CONFIG"
fi

case "$JOBS" in
  ''|*[!0-9]*) error "-j/--jobs 必须是正整数，当前值: $JOBS"; exit "$EXIT_CONFIG" ;;
esac
if [ "$JOBS" -lt 1 ]; then
  error "-j/--jobs 必须 >= 1，当前值: $JOBS"
  exit "$EXIT_CONFIG"
fi

# DSN 缺少 sslmode 时自动补上（Neon 必须走 SSL）
case "$NEON_DSN" in
  *sslmode=*) : ;;
  *\?*) info "DSN 未指定 sslmode，已自动补上 &sslmode=require"; NEON_DSN="${NEON_DSN}&sslmode=require" ;;
  *)    info "DSN 未指定 sslmode，已自动补上 ?sslmode=require"; NEON_DSN="${NEON_DSN}?sslmode=require" ;;
esac

NEON_DB="$(dsn_dbname "$NEON_DSN")"

# 表过滤：命令行 -t 优先；否则用环境变量 TABLES（逗号/空格分隔）
if [ "$TABLE_FROM_CLI" -eq 0 ] && [ -n "$TABLES" ]; then
  for t in $(printf '%s' "$TABLES" | tr ',' ' '); do
    TABLE_LIST[${#TABLE_LIST[@]}]="$t"
  done
fi

# JOBS > 1 时用目录格式并行导出，否则默认 -Fc
USE_DIR=0
if [ "$JOBS" -gt 1 ]; then
  USE_DIR=1
fi

# =============================================================================
# ⑦ 前置检查（退出码 3）
# =============================================================================
if ! command -v docker >/dev/null 2>&1; then
  error "找不到 docker 命令，请先安装并启动 Docker Desktop。"
  exit "$EXIT_PRECHECK"
fi

container_running="$(docker inspect -f '{{.State.Running}}' "$LOCAL_CONTAINER" 2>/dev/null || true)"
if [ "$container_running" != "true" ]; then
  error "容器 $LOCAL_CONTAINER 未处于运行状态（docker inspect 返回: ${container_running:-<不存在>}）。"
  error "可尝试: docker start $LOCAL_CONTAINER"
  exit "$EXIT_PRECHECK"
fi

trap cleanup EXIT
trap 'exit 130' INT TERM

info "Neon   : $(mask_dsn "$NEON_DSN")"
info "本地   : $LOCAL_CONTAINER / $LOCAL_USER / $LOCAL_DB"
if [ "$USE_DIR" -eq 1 ]; then
  info "导出   : 目录格式 (-Fd -j $JOBS)"
else
  info "导出   : 自定义格式 (-Fc)"
fi
if [ "${#TABLE_LIST[@]}" -gt 0 ]; then
  info "同步表 : ${TABLE_LIST[*]}"
else
  info "同步表 : 全库"
fi

# =============================================================================
# ⑧ 确认（非 --force 且交互时）——放在建库之前，拒绝执行时零副作用
# =============================================================================
warn "即将把 Neon 库 [${NEON_DB:-?}] 导入本地 [$LOCAL_CONTAINER/$LOCAL_DB]，会覆盖同名对象。"
CONFIRMED=0
if [ "$FORCE" -eq 1 ]; then
  info "--force 已指定，跳过确认。"
  CONFIRMED=1
elif [ ! -t 0 ]; then
  error "非交互环境下必须显式指定 -f/--force 才会执行覆盖导入（会 DROP 同名对象）。"
  exit "$EXIT_CONFIG"
else
  printf '继续? [y/N] '
  ans=""
  read -r ans || ans=""
  ans_lc="$(printf '%s' "$ans" | tr '[:upper:]' '[:lower:]')"
  case "$ans_lc" in
    y|yes) CONFIRMED=1 ;;
    *) info "已取消，未做任何改动。"; exit 0 ;;
  esac
fi

# 目标库不存在则创建（确认通过后才建，避免取消/拒绝时留下空库）
if db_exists; then
  info "本地库 $LOCAL_DB 已存在。"
else
  info "本地库 $LOCAL_DB 不存在，正在创建…"
  if ! docker exec "$LOCAL_CONTAINER" createdb -U "$LOCAL_USER" "$LOCAL_DB"; then
    error "创建数据库 $LOCAL_DB 失败。"
    exit "$EXIT_PRECHECK"
  fi
  ok "已创建本地库 ${LOCAL_DB}。"
fi

# =============================================================================
# ⑨ 导出（退出码 4）
# =============================================================================
if ! mkdir -p "$DUMP_DIR"; then
  error "无法创建导出缓存目录: $DUMP_DIR"
  exit "$EXIT_PRECHECK"
fi

if ! docker exec "$LOCAL_CONTAINER" sh -c "rm -rf '$CONTAINER_TMP' && mkdir -p '$CONTAINER_TMP'"; then
  error "无法在容器内准备临时目录 $CONTAINER_TMP"
  exit "$EXIT_EXPORT"
fi
CONTAINER_TMP_CREATED=1

# 组装容器内 pg_dump 的其余参数（不含 DSN：DSN 走 stdin，避免密码出现在宿主进程表）
DUMP_ARGS=( --no-owner --no-privileges )
if [ "$SCHEMA_ONLY" -eq 1 ]; then DUMP_ARGS+=( --schema-only ); fi
if [ "$DATA_ONLY" -eq 1 ]; then DUMP_ARGS+=( --data-only ); fi

if [ "$USE_DIR" -eq 1 ]; then
  CONTAINER_DUMP="$CONTAINER_TMP/dumpdir"
  DUMP_ARGS+=( -Fd -j "$JOBS" -f "$CONTAINER_DUMP" )
else
  CONTAINER_DUMP="$CONTAINER_TMP/dump.pgc"
  DUMP_ARGS+=( -Fc -f "$CONTAINER_DUMP" )
fi

# 表过滤（pg_dump -t）
ti=0
while [ "$ti" -lt "${#TABLE_LIST[@]}" ]; do
  DUMP_ARGS+=( -t "${TABLE_LIST[$ti]}" )
  ti=$((ti + 1))
done

# 冷启动重试：Neon suspend 后首次连接常失败，最多 3 次（首次 + 2 次重试）
attempt=1
export_rc=0
while [ "$attempt" -le 3 ]; do
  if [ "$attempt" -eq 1 ]; then
    info "开始从 Neon 导出…"
  fi
  export_rc=0
  # DSN 经 stdin 传入容器，不进 docker exec 的 argv；容器内 sh 读取首行后交给 pg_dump
  printf '%s\n' "$NEON_DSN" \
    | docker exec -i "$LOCAL_CONTAINER" sh -c 'read -r dsn; exec pg_dump "$dsn" "$@"' sh "${DUMP_ARGS[@]}" \
    || export_rc=$?
  if [ "$export_rc" -eq 0 ]; then
    break
  fi
  if [ "$attempt" -lt 3 ]; then
    attempt=$((attempt + 1))
    warn "导出失败（返回码 ${export_rc}）。Neon 可能处于冷启动，5 秒后重试 (${attempt}/3)"
    sleep 5
  else
    break
  fi
done

if [ "$export_rc" -ne 0 ]; then
  error "导出失败（已尝试 3 次，最后返回码 ${export_rc}）。"
  error "请检查 NEON_DSN 是否正确、Neon 项目是否已唤醒、网络是否可达。"
  exit "$EXIT_EXPORT"
fi
ok "导出完成。"

# 把 dump 取回宿主机（留一份离线快照）
if [ "$USE_DIR" -eq 1 ]; then
  HOST_DUMP="$DUMP_DIR/neon_dump.dir"
  rm -rf "$HOST_DUMP"
else
  HOST_DUMP="$DUMP_DIR/neon_dump.pgc"
  rm -f "$HOST_DUMP"
fi

if ! docker cp "$LOCAL_CONTAINER:$CONTAINER_DUMP" "$HOST_DUMP"; then
  error "把 dump 复制到宿主机失败: $HOST_DUMP"
  exit "$EXIT_EXPORT"
fi
info "快照已保存: $HOST_DUMP"

# =============================================================================
# ⑩ 导入（退出码 5）
# =============================================================================
RESTORE_CMD=( pg_restore -U "$LOCAL_USER" -d "$LOCAL_DB" --no-owner --no-privileges )
# 覆盖同名对象：只有确认通过或 --force 时才追加（由 CONFIRMED 统一控制）
if [ "$CONFIRMED" -eq 1 ]; then
  RESTORE_CMD+=( --clean --if-exists )
fi

IMPORT_LOG="$(mktemp "${TMPDIR:-/tmp}/neon-sync-import.XXXXXX")" || {
  error "无法创建导入日志临时文件。"
  exit "$EXIT_IMPORT"
}

import_rc=0
if [ "$USE_DIR" -eq 1 ]; then
  RESTORE_CMD+=( -j "$JOBS" "$CONTAINER_DUMP" )
  info "开始导入到 $LOCAL_CONTAINER/${LOCAL_DB}（目录格式）…"
  docker exec "$LOCAL_CONTAINER" "${RESTORE_CMD[@]}" >"$IMPORT_LOG" 2>&1 || import_rc=$?
else
  RESTORE_CMD+=( "$CONTAINER_TMP/dump.pgc" )
  info "开始导入到 $LOCAL_CONTAINER/${LOCAL_DB}…"
  # 容器内已有一份 dump，pg_restore 直接读该文件；宿主机 ~/.cache/neon-sync/ 那份仅作离线快照备份
  docker exec "$LOCAL_CONTAINER" "${RESTORE_CMD[@]}" >"$IMPORT_LOG" 2>&1 || import_rc=$?
fi

# 把 pg_restore 的输出回显到 stderr，保持可见性
cat "$IMPORT_LOG" >&2 || true

# 不用 --exit-on-error：缺失 extension/role/schema 会报错但不中断，属预期情况。
# 但 import_rc != 0 时必须逐行核查错误行：存在无法忽略的错误就判失败。
# 只把 pg_restore 自身错误前缀 "pg_restore: error:" 或服务器 "ERROR:" 行当候选；
# 收尾汇总行 "pg_restore: warning: errors ignored on restore: N" 是 warning 且小写 errors，
# 自然被排除，避免"仅有可忽略错误"时被误判为失败。
if [ "$import_rc" -ne 0 ]; then
  bad_lines=""
  while IFS= read -r line; do
    case "$line" in
      pg_restore:\ error:*|*ERROR:*) ;;
      *) continue ;;
    esac
    if ! is_ignorable_error "$line"; then
      bad_lines="${bad_lines}${line}
"
    fi
  done < "$IMPORT_LOG"

  if [ -n "$bad_lines" ]; then
    error "导入失败：pg_restore 返回码 ${import_rc}，且存在无法忽略的错误行："
    printf '%s' "$bad_lines" >&2
    exit "$EXIT_IMPORT"
  fi
  warn "pg_restore 返回码 ${import_rc}（常见于 Neon 上的 extension/角色本地不存在），已尽量导入，继续校验。"
fi

# =============================================================================
# ⑪ 收尾校验
# =============================================================================
table_count="$(docker exec "$LOCAL_CONTAINER" psql -U "$LOCAL_USER" -d "$LOCAL_DB" -tAc \
  "select count(*) from information_schema.tables where table_schema='public'" 2>/dev/null || true)"

if [ -z "$table_count" ]; then
  error "导入校验失败：无法连接本地库 ${LOCAL_DB}。"
  exit "$EXIT_IMPORT"
fi

if [ "$import_rc" -ne 0 ] && [ "$table_count" -eq 0 ]; then
  error "导入失败：pg_restore 返回 $import_rc 且本地库没有任何表。"
  exit "$EXIT_IMPORT"
fi

elapsed=$(( $(date +%s) - START_TS ))
ok "同步完成：$LOCAL_CONTAINER/$LOCAL_DB 现有 public 表 $table_count 张，耗时 $(fmt_dur "$elapsed")。"
if [ "$import_rc" -ne 0 ]; then
  warn "注意：pg_restore 曾报错（返回码 ${import_rc}），请按需检查上方输出。"
fi

# =============================================================================
# 用法示例:
#   1) 全库同步（会提示确认）:
#        NEON_DSN='postgresql://user:pass@ep-xxx.us-east-2.aws.neon.tech/neondb' ./scripts/neon-sync.sh
#   2) 只同步 orders、users 两张表到 mj_local 库，不确认:
#        ./scripts/neon-sync.sh -d "$NEON_DSN" -D mj_local -t orders -t users -f
#   3) 只同步结构，并用 4 路并行目录格式:
#        ./scripts/neon-sync.sh -d "$NEON_DSN" -s -j 4
# =============================================================================
