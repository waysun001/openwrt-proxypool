#!/bin/bash
# SOCKS5 客户端管理脚本
# 使用 redsocks 实现透明代理

set -e

RUN_DIR="/var/run/proxypool"
REDSOCKS_DIR="/var/run/proxypool/redsocks"
LOG_FILE="/var/log/proxypool.log"

# 基础端口，每个客户端递增
BASE_TCP_PORT=12300
BASE_UDP_PORT=12400

log_info() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [socks5] $*" >> "$LOG_FILE"
}

log_error() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [socks5] [ERROR] $*" >> "$LOG_FILE"
    echo "[ERROR] $*" >&2
}

# 读取UCI配置
get_config() {
    local section="$1"
    local option="$2"
    local default="$3"
    uci -q get "proxypool.$section.$option" || echo "$default"
}

# 计算客户端端口
get_client_port() {
    local client="$1"
    local type="$2"  # tcp or udp
    local num=$(echo "$client" | sed 's/client_//' | sed 's/^0*//')
    [ -z "$num" ] && num=0

    if [ "$type" = "tcp" ]; then
        echo $((BASE_TCP_PORT + num))
    else
        echo $((BASE_UDP_PORT + num))
    fi
}

# 生成 redsocks 配置
generate_redsocks_config() {
    local client="$1"
    local server=$(get_config "$client" "server" "")
    local port=$(get_config "$client" "port" "1080")
    local auth=$(get_config "$client" "auth" "0")
    local username=$(get_config "$client" "username" "")
    local password=$(get_config "$client" "password" "")

    if [ -z "$server" ]; then
        log_error "No server configured for $client"
        return 1
    fi

    mkdir -p "$REDSOCKS_DIR"

    local config_file="$REDSOCKS_DIR/${client}.conf"
    local local_tcp_port=$(get_client_port "$client" "tcp")
    local local_udp_port=$(get_client_port "$client" "udp")

    # 认证配置
    local login_line=""
    local password_line=""
    if [ "$auth" = "1" ] && [ -n "$username" ]; then
        login_line="login = \"${username}\";"
        password_line="password = \"${password}\";"
    fi

    cat > "$config_file" << EOF
base {
    log_debug = off;
    log_info = on;
    log = "file:$REDSOCKS_DIR/${client}.log";
    daemon = on;
    redirector = iptables;
}

redsocks {
    local_ip = 0.0.0.0;
    local_port = ${local_tcp_port};
    ip = ${server};
    port = ${port};
    type = socks5;
    ${login_line}
    ${password_line}
}

redudp {
    local_ip = 0.0.0.0;
    local_port = ${local_udp_port};
    ip = ${server};
    port = ${port};
    dest_ip = 0.0.0.0;
    dest_port = 0;
    udp_timeout = 30;
    udp_timeout_stream = 180;
}
EOF

    log_info "Generated redsocks config for $client: $config_file"
}

# 启动 SOCKS5 客户端
start() {
    local client="$1"
    local name=$(get_config "$client" "name" "$client")

    log_info "Starting SOCKS5 client: $name"

    # 生成配置
    generate_redsocks_config "$client" || return 1

    local config_file="$REDSOCKS_DIR/${client}.conf"
    local pid_file="$REDSOCKS_DIR/${client}.pid"

    # 启动 redsocks
    redsocks -c "$config_file" -p "$pid_file"

    sleep 1

    # 检查是否启动成功
    if [ -f "$pid_file" ]; then
        local pid=$(cat "$pid_file")
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            log_info "SOCKS5 client started: $name (PID: $pid)"

            # 保存运行状态
            mkdir -p "$RUN_DIR/clients"
            local config_hash=$(uci show "proxypool.$client" | md5sum | cut -d' ' -f1)
            echo "$config_hash" > "$RUN_DIR/clients/$client"

            # 保存端口信息
            local tcp_port=$(get_client_port "$client" "tcp")
            local udp_port=$(get_client_port "$client" "udp")
            echo "${tcp_port}:${udp_port}" > "$REDSOCKS_DIR/${client}.ports"

            return 0
        fi
    fi

    log_error "Failed to start SOCKS5 client: $name"
    return 1
}

# 停止 SOCKS5 客户端
stop() {
    local client="$1"
    local name=$(get_config "$client" "name" "$client")

    log_info "Stopping SOCKS5 client: $name"

    local pid_file="$REDSOCKS_DIR/${client}.pid"

    if [ -f "$pid_file" ]; then
        local pid=$(cat "$pid_file")
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
            sleep 1
            kill -9 "$pid" 2>/dev/null || true
        fi
        rm -f "$pid_file"
    fi

    # 清理文件
    rm -f "$REDSOCKS_DIR/${client}.conf"
    rm -f "$REDSOCKS_DIR/${client}.log"
    rm -f "$REDSOCKS_DIR/${client}.ports"
    rm -f "$RUN_DIR/clients/$client"

    log_info "SOCKS5 client stopped: $name"
}

# 获取状态
status() {
    local client="$1"
    local pid_file="$REDSOCKS_DIR/${client}.pid"

    if [ -f "$pid_file" ]; then
        local pid=$(cat "$pid_file")
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            echo "connected"

            # 返回本地端口信息
            if [ -f "$REDSOCKS_DIR/${client}.ports" ]; then
                cat "$REDSOCKS_DIR/${client}.ports"
            fi
            return 0
        fi
    fi

    echo "disconnected"
}

# 测试连接
test_connection() {
    local client="$1"
    local server=$(get_config "$client" "server" "")
    local port=$(get_config "$client" "port" "1080")

    # 简单的端口测试
    if timeout 5 bash -c "echo >/dev/tcp/${server}/${port}" 2>/dev/null; then
        echo "ok"
    else
        echo "fail"
    fi
}

# 获取本地端口
get_local_port() {
    local client="$1"
    local type="${2:-tcp}"  # tcp or udp
    get_client_port "$client" "$type"
}

# 主入口
case "$1" in
    start)
        start "$2"
        ;;
    stop)
        stop "$2"
        ;;
    status)
        status "$2"
        ;;
    test)
        test_connection "$2"
        ;;
    port)
        get_local_port "$2" "$3"
        ;;
    *)
        echo "Usage: $0 {start|stop|status|test|port} <client_id> [tcp|udp]"
        exit 1
        ;;
esac
