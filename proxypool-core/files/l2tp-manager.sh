#!/bin/bash
# L2TP 客户端管理脚本

set -e

RUN_DIR="/var/run/proxypool"
L2TP_DIR="/var/run/xl2tpd"
PPP_DIR="/etc/ppp/peers"
LOG_FILE="/var/log/proxypool.log"

log_info() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [l2tp] $*" >> "$LOG_FILE"
}

log_error() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [l2tp] [ERROR] $*" >> "$LOG_FILE"
    echo "[ERROR] $*" >&2
}

# 读取UCI配置
get_config() {
    local section="$1"
    local option="$2"
    local default="$3"
    uci -q get "proxypool.$section.$option" || echo "$default"
}

# 生成 xl2tpd 配置
generate_xl2tpd_config() {
    local client="$1"
    local server=$(get_config "$client" "server" "")
    local port=$(get_config "$client" "port" "1701")
    local name=$(get_config "$client" "name" "$client")

    if [ -z "$server" ]; then
        log_error "No server configured for $client"
        return 1
    fi

    local lac_name="lac_${client}"
    local config_file="/etc/xl2tpd/xl2tpd-${client}.conf"
    local control_file="$L2TP_DIR/control-${client}"

    mkdir -p /etc/xl2tpd
    mkdir -p "$L2TP_DIR"

    cat > "$config_file" << EOF
[global]
port = 0
access control = no

[lac ${lac_name}]
lns = ${server}
lns port = ${port}
pppoptfile = ${PPP_DIR}/proxypool-${client}
redial = yes
redial timeout = 15
max redials = 5
require chap = yes
require authentication = no
ppp debug = no
name = ${client}
EOF

    log_info "Generated xl2tpd config for $client: $config_file"
}

# 生成 PPP 配置
generate_ppp_config() {
    local client="$1"
    local username=$(get_config "$client" "username" "")
    local password=$(get_config "$client" "password" "")
    local server=$(get_config "$client" "server" "")

    local ppp_file="${PPP_DIR}/proxypool-${client}"
    local secrets_file="/etc/ppp/chap-secrets"

    mkdir -p "$PPP_DIR"

    # 生成 peers 文件
    cat > "$ppp_file" << EOF
# ProxyPool L2TP Client: $client
plugin pppol2tp.so
connect /bin/true
pty ""

# 认证
name ${username}
password ${password}

# 接口
ifname ppp-${client}
unit $(echo "$client" | sed 's/client_//')

# 选项
noauth
refuse-eap
nodeflate
nobsdcomp
noipdefault
defaultroute
usepeerdns
persist
maxfail 0
holdoff 10
lcp-echo-interval 30
lcp-echo-failure 4

# 不添加默认路由，由策略路由处理
nodefaultroute

# IP配置由服务器分配
ipcp-accept-local
ipcp-accept-remote
EOF

    # 添加到 chap-secrets (如果不存在)
    if ! grep -q "^${username}[[:space:]]" "$secrets_file" 2>/dev/null; then
        echo "${username} * ${password} *" >> "$secrets_file"
    fi

    chmod 600 "$ppp_file"
    chmod 600 "$secrets_file"

    log_info "Generated PPP config for $client"
}

# 启动 L2TP 客户端
start() {
    local client="$1"
    local name=$(get_config "$client" "name" "$client")

    log_info "Starting L2TP client: $name"

    # 生成配置
    generate_xl2tpd_config "$client" || return 1
    generate_ppp_config "$client" || return 1

    local config_file="/etc/xl2tpd/xl2tpd-${client}.conf"
    local control_file="$L2TP_DIR/control-${client}"
    local pid_file="$RUN_DIR/xl2tpd-${client}.pid"

    # 启动 xl2tpd 实例
    xl2tpd -c "$config_file" -C "$control_file" -p "$pid_file" &

    sleep 2

    # 发送连接命令
    if [ -S "$control_file" ] || [ -p "$control_file" ]; then
        echo "c lac_${client}" > "$control_file"
        log_info "Sent connect command to $client"
    else
        # 使用文件方式
        echo "c lac_${client}" > "$control_file"
    fi

    # 保存运行状态
    local config_hash=$(uci show "proxypool.$client" | md5sum | cut -d' ' -f1)
    echo "$config_hash" > "$RUN_DIR/clients/$client"

    log_info "L2TP client started: $name"
}

# 停止 L2TP 客户端
stop() {
    local client="$1"
    local name=$(get_config "$client" "name" "$client")

    log_info "Stopping L2TP client: $name"

    local control_file="$L2TP_DIR/control-${client}"
    local pid_file="$RUN_DIR/xl2tpd-${client}.pid"

    # 发送断开命令
    if [ -e "$control_file" ]; then
        echo "d lac_${client}" > "$control_file" 2>/dev/null || true
        sleep 1
    fi

    # 终止 xl2tpd 进程
    if [ -f "$pid_file" ]; then
        local pid=$(cat "$pid_file")
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
            sleep 1
            kill -9 "$pid" 2>/dev/null || true
        fi
        rm -f "$pid_file"
    fi

    # 清理 PPP 接口
    local ppp_iface="ppp-${client}"
    if ip link show "$ppp_iface" &>/dev/null; then
        ip link set "$ppp_iface" down 2>/dev/null || true
    fi

    # 清理配置文件
    rm -f "/etc/xl2tpd/xl2tpd-${client}.conf"
    rm -f "${PPP_DIR}/proxypool-${client}"
    rm -f "$control_file"

    # 清理运行状态
    rm -f "$RUN_DIR/clients/$client"

    log_info "L2TP client stopped: $name"
}

# 获取状态
status() {
    local client="$1"
    local ppp_iface="ppp-${client}"

    if ip link show "$ppp_iface" &>/dev/null; then
        local ip=$(ip -4 addr show "$ppp_iface" 2>/dev/null | grep -oP '(?<=inet\s)\d+(\.\d+){3}')
        if [ -n "$ip" ]; then
            echo "connected"
            echo "$ip"
            return 0
        fi
    fi

    local pid_file="$RUN_DIR/xl2tpd-${client}.pid"
    if [ -f "$pid_file" ]; then
        local pid=$(cat "$pid_file")
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            echo "connecting"
            return 0
        fi
    fi

    echo "disconnected"
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
    *)
        echo "Usage: $0 {start|stop|status} <client_id>"
        exit 1
        ;;
esac
