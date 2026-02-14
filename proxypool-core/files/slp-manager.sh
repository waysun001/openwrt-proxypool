#!/bin/sh
# 智联盒子 - SLP (SmartLink Protocol) 客户端管理脚本
# 单进程多隧道模式，每个隧道独立协程，一个挂了不影响其他

RUN_DIR="/var/run/proxypool"
SLP_RUN_DIR="/var/run/proxypool/slp"
SLP_BIN="/usr/bin/slp-client"
LOG_FILE="/var/log/proxypool.log"

# SOCKS5 端口基址
BASE_SOCKS5_PORT=10800

log_info() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [slp] $*" >> "$LOG_FILE"
}

log_error() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [slp] [ERROR] $*" >> "$LOG_FILE"
    echo "[ERROR] $*" >&2
}

get_config() {
    local section="$1"
    local option="$2"
    local default="$3"
    uci -q get "proxypool.$section.$option" || echo "$default"
}

# 从客户端 ID 中提取编号
get_client_num() {
    local num=$(echo "$1" | sed 's/[^0-9]//g')
    [ -z "$num" ] && num=0
    echo "$num"
}

# 计算 SOCKS5 端口
get_socks5_port() {
    local client="$1"
    local num=$(get_client_num "$client")
    echo $((BASE_SOCKS5_PORT + num))
}

# 检查 slp-client 是否已安装
check_slp_binary() {
    if [ ! -x "$SLP_BIN" ]; then
        log_error "slp-client not found at $SLP_BIN"
        return 1
    fi
    return 0
}

# 生成客户端配置文件
generate_config() {
    local client="$1"
    local server=$(get_config "$client" "server" "" | tr -d ' \t\n\r')
    local port=$(get_config "$client" "port" "443" | tr -d ' \t\n\r')
    local token=$(get_config "$client" "slp_token" "" | tr -d ' \t\n\r')
    local transport=$(get_config "$client" "slp_transport" "quic" | tr -d ' \t\n\r')
    local obfs=$(get_config "$client" "slp_obfs" "0" | tr -d ' \t\n\r')
    local obfs_key=$(get_config "$client" "slp_obfs_key" "" | tr -d ' \t\n\r')
    local insecure=$(get_config "$client" "slp_insecure" "1" | tr -d ' \t\n\r')
    
    if [ -z "$server" ] || [ -z "$token" ]; then
        log_error "Missing server or token for $client"
        return 1
    fi
    
    local socks5_port=$(get_socks5_port "$client")
    local config_dir="$SLP_RUN_DIR/$client"
    mkdir -p "$config_dir"
    
    local config_file="$config_dir/config.yaml"
    
    # 生成 YAML 配置
    cat > "$config_file" << EOF
log_level: info

tunnels:
  - name: "$client"
    enabled: true
    server: "$server"
    port: $port
    transport: "$transport"
    token: "$token"
    local_port: $socks5_port
    insecure: $([ "$insecure" = "1" ] && echo "true" || echo "false")
    obfs: $([ "$obfs" = "1" ] && echo "true" || echo "false")
    obfs_key: "$obfs_key"
    keepalive: 15
EOF
    
    log_info "Generated SLP config for $client: $config_file"
    return 0
}

start() {
    local client="$1"
    local name=$(get_config "$client" "name" "$client")
    
    log_info "Starting SLP client: $name"
    
    # 检查二进制
    check_slp_binary || return 1
    
    local config_dir="$SLP_RUN_DIR/$client"
    local pid_file="$config_dir/slp.pid"
    local config_file="$config_dir/config.yaml"
    
    # 防重复：如果进程已存活，跳过
    if [ -f "$pid_file" ]; then
        local old_pid=$(cat "$pid_file")
        if [ -n "$old_pid" ] && kill -0 "$old_pid" 2>/dev/null; then
            log_info "SLP client $name already running (PID: $old_pid), skipping"
            return 0
        fi
        rm -f "$pid_file"
    fi
    
    # 生成配置
    generate_config "$client" || return 1
    
    # 启动 slp-client（后台运行）
    $SLP_BIN -c "$config_file" > "$config_dir/slp.log" 2>&1 &
    local pid=$!
    echo "$pid" > "$pid_file"
    
    sleep 2
    
    # 检查是否启动成功
    if kill -0 "$pid" 2>/dev/null; then
        log_info "SLP client started: $name (PID: $pid)"
        
        # 记录客户端状态
        mkdir -p "$RUN_DIR/clients"
        local config_hash=$(uci show "proxypool.$client" | md5sum | cut -d' ' -f1)
        echo "$config_hash" > "$RUN_DIR/clients/$client"
        
        # 记录端口
        local socks5_port=$(get_socks5_port "$client")
        echo "$socks5_port" > "$config_dir/socks5.port"
        
        return 0
    else
        log_error "Failed to start SLP client: $name"
        rm -f "$pid_file"
        return 1
    fi
}

stop() {
    local client="$1"
    local name=$(get_config "$client" "name" "$client")
    
    log_info "Stopping SLP client: $name"
    
    local config_dir="$SLP_RUN_DIR/$client"
    local pid_file="$config_dir/slp.pid"
    
    if [ -f "$pid_file" ]; then
        local pid=$(cat "$pid_file")
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
            sleep 1
            kill -9 "$pid" 2>/dev/null || true
        fi
        rm -f "$pid_file"
    fi
    
    # 清理运行文件
    rm -rf "$config_dir"
    rm -f "$RUN_DIR/clients/$client"
    
    log_info "SLP client stopped: $name"
}

status() {
    local client="$1"
    local config_dir="$SLP_RUN_DIR/$client"
    local pid_file="$config_dir/slp.pid"
    
    if [ -f "$pid_file" ]; then
        local pid=$(cat "$pid_file")
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            # 进程存活，测试连接
            local conn_result=$(test_connection "$client")
            if [ "$conn_result" = "ok" ]; then
                echo "connected"
                if [ -f "$config_dir/socks5.port" ]; then
                    cat "$config_dir/socks5.port"
                fi
                return 0
            else
                echo "connecting"
                return 0
            fi
        fi
    fi
    
    echo "disconnected"
}

test_connection() {
    local client="$1"
    local socks5_port=$(get_socks5_port "$client")
    local curl_bin=$(command -v curl 2>/dev/null)
    
    if [ -z "$curl_bin" ]; then
        # 没有 curl，用 nc 测试端口
        if nc -z -w 3 127.0.0.1 "$socks5_port" >/dev/null 2>&1; then
            echo "ok"
        else
            echo "fail"
        fi
        return
    fi
    
    # 用 curl 测试 SOCKS5 代理
    "$curl_bin" --socks5 "127.0.0.1:${socks5_port}" \
        --max-time 5 --silent --output /dev/null --head https://ip.sb
    
    if [ $? -eq 0 ]; then
        echo "ok"
    else
        echo "fail"
    fi
}

get_local_port() {
    local client="$1"
    get_socks5_port "$client"
}

# 获取出口 IP（通过 SOCKS5 代理查询）
get_outbound_ip() {
    local client="$1"
    local socks5_port=$(get_socks5_port "$client")
    local curl_bin=$(command -v curl 2>/dev/null)
    
    if [ -z "$curl_bin" ]; then
        echo ""
        return
    fi
    
    "$curl_bin" --socks5 "127.0.0.1:${socks5_port}" \
        --max-time 10 --silent https://ip.sb 2>/dev/null
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
        get_local_port "$2"
        ;;
    ip)
        get_outbound_ip "$2"
        ;;
    *)
        echo "Usage: $0 {start|stop|status|test|port|ip} <client_id>"
        exit 1
        ;;
esac
