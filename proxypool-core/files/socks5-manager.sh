#!/bin/sh
# 智联盒子 - SOCKS5 客户端管理脚本

RUN_DIR="/var/run/proxypool"
REDSOCKS_DIR="/var/run/proxypool/redsocks"
LOG_FILE="/var/log/proxypool.log"

BASE_TCP_PORT=12300
BASE_UDP_PORT=12400

log_info() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [socks5] $*" >> "$LOG_FILE"
}

log_error() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [socks5] [ERROR] $*" >> "$LOG_FILE"
    echo "[ERROR] $*" >&2
}

get_config() {
    local section="$1"
    local option="$2"
    local default="$3"
    uci -q get "proxypool.$section.$option" || echo "$default"
}

get_client_port() {
    local client="$1"
    local type="$2"
    local num=$(echo "$client" | sed 's/[^0-9]//g')
    [ -z "$num" ] && num=0

    if [ "$type" = "tcp" ]; then
        echo $((BASE_TCP_PORT + num))
    else
        echo $((BASE_UDP_PORT + num))
    fi
}

generate_redsocks_config() {
    local client="$1"
    local server=$(get_config "$client" "server" "" | tr -d ' \t\n\r')
    local port=$(get_config "$client" "port" "1080" | tr -d ' \t\n\r')
    local username=$(get_config "$client" "username" "" | tr -d ' \t\n\r')
    local password=$(get_config "$client" "password" "" | tr -d ' \t\n\r')

    if [ -z "$server" ]; then
        log_error "No server configured for $client"
        return 1
    fi

    mkdir -p "$REDSOCKS_DIR"

    local config_file="$REDSOCKS_DIR/${client}.conf"
    local local_tcp_port=$(get_client_port "$client" "tcp")
    local local_udp_port=$(get_client_port "$client" "udp")

    local login_line=""
    local password_line=""
    if [ -n "$username" ]; then
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

start() {
    local client="$1"
    local name=$(get_config "$client" "name" "$client")

    log_info "Starting SOCKS5 client: $name"

    local config_file="$REDSOCKS_DIR/${client}.conf"
    local pid_file="$REDSOCKS_DIR/${client}.pid"

    # 防重复：如果进程已存活，跳过而非杀掉（只有 stop() 才有权杀进程）
    if [ -f "$pid_file" ]; then
        local old_pid=$(cat "$pid_file")
        if [ -n "$old_pid" ] && kill -0 "$old_pid" 2>/dev/null; then
            log_info "SOCKS5 client $name already running (PID: $old_pid), skipping"
            return 0
        fi
        # 进程已死，清理残留 pid 文件
        rm -f "$pid_file"
    fi

    generate_redsocks_config "$client" || return 1

    redsocks -c "$config_file" -p "$pid_file"

    sleep 1

    if [ -f "$pid_file" ]; then
        local pid=$(cat "$pid_file")
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            log_info "SOCKS5 client started: $name (PID: $pid)"

            mkdir -p "$RUN_DIR/clients"
            local config_hash=$(uci show "proxypool.$client" | md5sum | cut -d' ' -f1)
            echo "$config_hash" > "$RUN_DIR/clients/$client"

            local tcp_port=$(get_client_port "$client" "tcp")
            local udp_port=$(get_client_port "$client" "udp")
            echo "${tcp_port}:${udp_port}" > "$REDSOCKS_DIR/${client}.ports"

            return 0
        fi
    fi

    log_error "Failed to start SOCKS5 client: $name"
    return 1
}

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

    rm -f "$REDSOCKS_DIR/${client}.conf"
    rm -f "$REDSOCKS_DIR/${client}.log"
    rm -f "$REDSOCKS_DIR/${client}.ports"
    rm -f "$RUN_DIR/clients/$client"

    log_info "SOCKS5 client stopped: $name"
}

status() {
    local client="$1"
    local pid_file="$REDSOCKS_DIR/${client}.pid"

    if [ -f "$pid_file" ]; then
        local pid=$(cat "$pid_file")
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            local conn_result=$(test_connection "$client")
            if [ "$conn_result" = "ok" ]; then
                echo "connected"
                if [ -f "$REDSOCKS_DIR/${client}.ports" ]; then
                    cat "$REDSOCKS_DIR/${client}.ports"
                fi
                return 0
            else
                echo "disconnected"
                return 1
            fi
        fi
    fi

    echo "disconnected"
}

test_connection() {
    local client="$1"
    local server=$(get_config "$client" "server" "" | tr -d ' \t\n\r')
    local port=$(get_config "$client" "port" "1080" | tr -d ' \t\n\r')

    if echo "" | nc -w 3 "$server" "$port" >/dev/null 2>&1; then
        echo "ok"
    else
        echo "fail"
    fi
}

get_local_port() {
    local client="$1"
    local type="${2:-tcp}"
    get_client_port "$client" "$type"
}

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
