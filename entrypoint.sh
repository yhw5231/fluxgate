#!/bin/sh
# Fluxgate 容器入口脚本。
#
# 容器默认短暂以 root 启动（仅保留 chown 能力），把数据目录的所有权交给运行
# 用户后立即降权执行网关。这样即使用户没有提前在宿主机上 chown 数据目录
# （Docker 自动创建的目录归 root），部署也能开箱即用。
# 若以非 root 用户启动（如 docker run --user 或 Kubernetes securityContext），
# 则跳过准备阶段直接运行，此时数据目录必须已可写。
#
# 另外，若设置了 FLUXGATE_ADMIN_PASSWORD 且尚未创建管理员账号，则在启动网关
# 前自动创建账号（仅首次生效）。未设置该变量时跳过，需自行执行
# `fluxgate admin create`。
set -eu

run_uid="${GATEWAY_UID:-10001}"
run_gid="${GATEWAY_GID:-10001}"
data_dir="$(dirname "${FLUXGATE_DATABASE_PATH:-/data/hub.db}")"
admin_user="${FLUXGATE_ADMIN_USERNAME:-admin}"

if [ "$(id -u)" = "0" ]; then
    mkdir -p "$data_dir"
    if ! chown -R "$run_uid:$run_gid" "$data_dir"; then
        echo "entrypoint: cannot grant $data_dir to uid $run_uid; check the volume mount and SELinux labels (mount :Z)" >&2
        exit 1
    fi
    # SQLite 之后创建的 hub.db、hub.db-wal、hub.db-shm 等文件继承 0600 权限
    umask 077
    # 在降权后的运行用户下创建账号，数据库属主才会与网关运行时一致。
    # 账号已存在（重复启动）不是错误，因此忽略该结果。
    if [ -n "${FLUXGATE_ADMIN_PASSWORD:-}" ]; then
        su-exec "$run_uid:$run_gid" /app/fluxgate admin create --username "$admin_user" >/dev/null 2>&1 || true
    fi
    exec su-exec "$run_uid:$run_gid" /app/fluxgate "$@"
fi

if [ -n "${FLUXGATE_ADMIN_PASSWORD:-}" ]; then
    /app/fluxgate admin create --username "$admin_user" >/dev/null 2>&1 || true
fi

exec /app/fluxgate "$@"
