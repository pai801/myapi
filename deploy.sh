#!/bin/bash
# 一键部署：构建前端(default) -> 编译后端 -> 停旧服务 -> 启动
# 用法：bash deploy.sh；前置依赖 go/node/npm，.env 需就位
# 业务日志在 logs/；nohup.out 收集 nohup 的 stdout/stderr
set -e
cd "$(dirname "$0")"

# 前端：THEMES 中 berry/air 无源码，仅构建实际存在的 default；产物经 //go:embed 嵌入二进制
(cd web/default && npm install && REACT_APP_VERSION="$(cat ../../VERSION)" npm run build)
# npm 构建失败时可能残留旧产物，必须校验，防止旧前端被静默 embed
[ -f web/build/default/index.html ] || { echo "前端产物 web/build/default/index.html 缺失，部署中止" >&2; exit 1; }

# 后端：与 Dockerfile 同款版本注入；CGO 保持开启（sqlite 依赖 cgo）
go build -trimpath -ldflags "-s -w -X 'github.com/pai801/myapi/common.Version=$(cat VERSION)'" -o myapi

# 停旧服务：仅凭 myapi.pid 定位；文件缺失或进程已死则跳过
if [ -f myapi.pid ]; then
  pid="$(cat myapi.pid)"
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid"
    waited=0
    while kill -0 "$pid" 2>/dev/null && [ "$waited" -lt 10 ]; do
      sleep 1
      waited=$((waited + 1))
    done
    if kill -0 "$pid" 2>/dev/null; then
      kill -9 "$pid"
    fi
  fi
fi

# JWT_SECRET：未设置时生成随机密钥并持久化到 .jwt_secret，重启复用同一密钥避免登录态失效
if [ -z "$JWT_SECRET" ]; then
  # umask 077 让密钥文件从创建瞬间即为 600，避免落盘到 chmod 之间的窗口期可被其他用户读取
  [ -s .jwt_secret ] || (umask 077; openssl rand -hex 32 > .jwt_secret)
  chmod 600 .jwt_secret
  export JWT_SECRET="$(cat .jwt_secret)"
fi

# 启动并记录 PID，供下次部署停旧
nohup ./myapi --log-dir ./logs &
echo $! > myapi.pid

echo "已启动 myapi，PID=$(cat myapi.pid)"
echo "业务日志：logs/ ，nohup 输出：nohup.out"
