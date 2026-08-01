#!/bin/sh
# 智联盒子 - SLP (SmartLink Protocol) 客户端管理脚本
# 单进程多隧道模式，每个隧道独立协程，一个挂了不影响其他

RUN_DIR="/var/run/proxypool"
SLP_RUN_DIR="/var/run/proxypool/slp"
REDSOCKS_DIR="/var/run/proxypool/redsocks"
PROBE_DIR="/var/run/proxypool/probe"
SLP_BIN="/usr/bin/slp-client"
LOG_FILE="/var/log/proxypool.log"
LEGACY_GATE="${PROXYPOOL_LEGACY_GATE:-/usr/lib/proxypool/legacy-gate.sh}"

legacy_quarantine() {
    /bin/sh "$LEGACY_GATE" mutation "$1" >/dev/null 2>&1 || true
    printf '%s\n' 'legacy_runtime_quarantined'
    return 125
}

# SLP 本地 SOCKS5 端口范围: 10801-10999
BASE_SOCKS5_PORT=10800
PORT_RANGE=199

# SLP 内置 DNS 代理端口范围: 5301-5499（与 SOCKS5 端口哈希计算复用）
BASE_DNS_PORT=5300

# redsocks 本地端口（透明代理 → SLP SOCKS5，避免与 socks5-manager 的 12300/12400 冲突）
BASE_REDSOCKS_TCP_PORT=12500
BASE_REDSOCKS_UDP_PORT=12600

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

# 基于客户端 ID 的稳定哈希计算端口，避免不同 ID 提取相同数字导致冲突
get_socks5_port() {
    local client="$1"
    # 优先使用已分配的端口（持久化）
    local port_file="$SLP_RUN_DIR/$client/socks5.port"
    if [ -f "$port_file" ]; then
        cat "$port_file"
        return
    fi
    # 用 md5sum 对客户端 ID 做哈希，取前 8 位十六进制转十进制
    local hash=$(echo -n "$client" | md5sum | cut -c1-8)
    hash=$((0x$hash))
    local offset=$(( (hash % PORT_RANGE) + 1 ))
    echo $((BASE_SOCKS5_PORT + offset))
}

# 基于客户端 ID 计算 DNS 代理端口（与 SOCKS5 端口哈希复用）
get_dns_port() {
    local client="$1"
    local port_file="$SLP_RUN_DIR/$client/dns.port"
    if [ -f "$port_file" ]; then
        cat "$port_file"
        return
    fi
    local hash=$(echo -n "$client" | md5sum | cut -c1-8)
    hash=$((0x$hash))
    local offset=$(( (hash % PORT_RANGE) + 1 ))
    echo $((BASE_DNS_PORT + offset))
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

    # 校验传输方式（仅支持 quic）
    case "$transport" in
        quic) ;;
        *)
            log_error "Invalid transport '$transport' for $client, fallback to quic"
            transport="quic"
            ;;
    esac
    
    local socks5_port=$(get_socks5_port "$client")
    local dns_port=$(get_dns_port "$client")
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
    dns_port: $dns_port
    insecure: $([ "$insecure" = "1" ] && echo "true" || echo "false")
    obfs: $([ "$obfs" = "1" ] && echo "true" || echo "false")
    obfs_key: "$obfs_key"
    keepalive: 15
EOF
    
    log_info "Generated SLP config for $client: $config_file"
    return 0
}

# ============================================================
# redsocks 透明代理管理（将 nftables 重定向的流量转为 SOCKS5）
# ============================================================

get_redsocks_port() {
    local client="$1"
    local type="$2"
    local num=$(echo "$client" | sed 's/[^0-9]//g')
    [ -z "$num" ] && num=0
    if [ "$type" = "tcp" ]; then
        echo $((BASE_REDSOCKS_TCP_PORT + num))
    else
        echo $((BASE_REDSOCKS_UDP_PORT + num))
    fi
}

# 生成 redsocks 配置（指向 SLP 本地 SOCKS5 端口）
generate_redsocks_config() {
    local client="$1"
    local socks5_port="$2"

    mkdir -p "$REDSOCKS_DIR"

    local config_file="$REDSOCKS_DIR/${client}.conf"
    local local_tcp_port=$(get_redsocks_port "$client" "tcp")

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
    ip = 127.0.0.1;
    port = ${socks5_port};
    type = socks5;
}
EOF

    log_info "Generated redsocks config for SLP $client: tcp=$local_tcp_port -> 127.0.0.1:$socks5_port"
}

start_redsocks() {
    local client="$1"
    local socks5_port="$2"
    local pid_file="$REDSOCKS_DIR/${client}.pid"

    # 防重复
    if [ -f "$pid_file" ]; then
        local old_pid=$(cat "$pid_file")
        if [ -n "$old_pid" ] && kill -0 "$old_pid" 2>/dev/null; then
            log_info "Redsocks for SLP $client already running (PID: $old_pid)"
            return 0
        fi
        rm -f "$pid_file"
    fi

    generate_redsocks_config "$client" "$socks5_port" || return 1

    redsocks -c "$REDSOCKS_DIR/${client}.conf" -p "$pid_file"

    # PID 文件轮询（替代固定 sleep 1，最长 2s，通常 <200ms）
    local _wait=0
    while [ $_wait -lt 20 ]; do
        [ -f "$pid_file" ] && break
        sleep 0.1
        _wait=$((_wait + 1))
    done

    if [ -f "$pid_file" ]; then
        local pid=$(cat "$pid_file")
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            local tcp_port=$(get_redsocks_port "$client" "tcp")
            echo "${tcp_port}:0" > "$REDSOCKS_DIR/${client}.ports"
            log_info "Redsocks started for SLP $client (PID: $pid, TCP: $tcp_port)"
            return 0
        fi
    fi

    log_error "Failed to start redsocks for SLP $client"
    return 1
}

stop_redsocks() {
    local client="$1"
    local pid_file="$REDSOCKS_DIR/${client}.pid"

    if [ -f "$pid_file" ]; then
        local pid=$(cat "$pid_file")
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
            local _w=0
            while [ $_w -lt 5 ] && kill -0 "$pid" 2>/dev/null; do
                sleep 0.1; _w=$((_w + 1))
            done
            kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null || true
        fi
        rm -f "$pid_file"
    fi

    rm -f "$REDSOCKS_DIR/${client}.conf"
    rm -f "$REDSOCKS_DIR/${client}.log"
    rm -f "$REDSOCKS_DIR/${client}.ports"
}

# ============================================================
# SLP 客户端生命周期
# ============================================================

start() {
    local client="$1"
    local name=$(get_config "$client" "name" "$client")
    
    log_info "Starting SLP client: $name"
    
    # 检查二进制
    check_slp_binary || return 1
    
    local config_dir="$SLP_RUN_DIR/$client"
    local pid_file="$config_dir/slp.pid"
    local config_file="$config_dir/config.yaml"
    
    # 防重复：如果进程已存活，只确保 redsocks 也在运行
    if [ -f "$pid_file" ]; then
        local old_pid=$(cat "$pid_file")
        if [ -n "$old_pid" ] && kill -0 "$old_pid" 2>/dev/null; then
            log_info "SLP client $name already running (PID: $old_pid), ensuring redsocks"
            local socks5_port=$(get_socks5_port "$client")
            start_redsocks "$client" "$socks5_port"
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

    # 端口轮询（替代固定 sleep 2，最长 2s，端口可达立即退出）
    local socks5_port_check=$(get_socks5_port "$client")
    local _wait=0
    while [ $_wait -lt 10 ]; do
        if ! kill -0 "$pid" 2>/dev/null; then
            break
        fi
        # 检查本地 SOCKS5 端口是否已监听
        if nc -z -w 1 127.0.0.1 "$socks5_port_check" >/dev/null 2>&1; then
            break
        fi
        sleep 0.2
        _wait=$((_wait + 1))
    done

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

        local dns_port=$(get_dns_port "$client")
        echo "$dns_port" > "$config_dir/dns.port"

        # 启动 redsocks 透明代理（nftables redirect → redsocks → SLP SOCKS5）
        start_redsocks "$client" "$socks5_port"

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

    # 先停 redsocks 透明代理
    stop_redsocks "$client"

    # 再停 slp-client
    local config_dir="$SLP_RUN_DIR/$client"
    local pid_file="$config_dir/slp.pid"

    if [ -f "$pid_file" ]; then
        local pid=$(cat "$pid_file")
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
            local _w=0
            while [ $_w -lt 5 ] && kill -0 "$pid" 2>/dev/null; do
                sleep 0.1; _w=$((_w + 1))
            done
            kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null || true
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
    local redsocks_pid_file="$REDSOCKS_DIR/${client}.pid"

    if [ -f "$pid_file" ]; then
        local pid=$(cat "$pid_file")
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            # slp-client 进程存活，检查 redsocks 是否也在运行
            local rs_ok="false"
            if [ -f "$redsocks_pid_file" ]; then
                local rs_pid=$(cat "$redsocks_pid_file")
                if [ -n "$rs_pid" ] && kill -0 "$rs_pid" 2>/dev/null; then
                    rs_ok="true"
                fi
            fi
            if [ "$rs_ok" = "true" ]; then
                # 双进程存活，读探测缓存判断实际连通性
                local probe_file="$PROBE_DIR/${client}"
                if [ -f "$probe_file" ]; then
                    local probe_result=$(cat "$probe_file")
                    if [ "$probe_result" = "ok" ]; then
                        echo "connected"
                    else
                        echo "disconnected"
                    fi
                else
                    # 无缓存（探测尚未完成），保守显示 disconnected
                    echo "disconnected"
                fi
            else
                echo "connecting"
            fi
            if [ -f "$config_dir/socks5.port" ]; then
                cat "$config_dir/socks5.port"
            fi
            return 0
        fi
    fi

    rm -f "$PROBE_DIR/${client}" 2>/dev/null
    echo "disconnected"
}

# 探测本地 SOCKS5 端口连通性并写入缓存（供 probe_all 后台并发调用）
probe() {
    local client="$1"
    mkdir -p "$PROBE_DIR"
    local result=$(test_connection "$client")
    echo "$result" > "$PROBE_DIR/${client}"
}

test_connection() {
    local client="$1"
    local socks5_port=$(get_socks5_port "$client")

    # 通过 SLP 本地 SOCKS5 端口实际发 HTTP 请求（准确判断隧道是否可上网）
    if command -v curl >/dev/null 2>&1; then
        local http_code
        http_code=$(curl --socks5-hostname "127.0.0.1:${socks5_port}" \
            --connect-timeout 3 --max-time 8 \
            -s -o /dev/null -w "%{http_code}" \
            "http://www.baidu.com" 2>/dev/null)
        if [ "$http_code" -ge 200 ] 2>/dev/null && [ "$http_code" -lt 400 ] 2>/dev/null; then
            echo "ok"
        else
            echo "fail"
        fi
    else
        # curl 不可用，回退到端口检测
        if nc -z -w 1 127.0.0.1 "$socks5_port" >/dev/null 2>&1; then
            echo "ok"
        else
            echo "fail"
        fi
    fi
}

get_local_port() {
    local client="$1"
    get_socks5_port "$client"
}

case "$1" in
    port)
        ;;
    start|stop|status|test|probe)
        legacy_quarantine "slp-manager:$1"
        exit $?
        ;;
    *)
        echo "Usage: $0 {start|stop|status|test|probe|port} <client_id>"
        exit 1
        ;;
esac

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
    probe)
        probe "$2"
        ;;
    port)
        get_local_port "$2"
        ;;
    *) exit 1 ;;
esac
