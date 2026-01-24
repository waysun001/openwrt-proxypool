#!/bin/bash
# ProxyPool 主控脚本
# 管理 L2TP 和 SOCKS5 代理客户端

set -e

SCRIPT_DIR="/usr/lib/proxypool"
CONFIG_FILE="/etc/config/proxypool"
RUN_DIR="/var/run/proxypool"
LOG_FILE="/var/log/proxypool.log"
STATUS_FILE="/var/run/proxypool/status.json"

# 日志函数
log() {
    local level="$1"
    shift
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [$level] $*" >> "$LOG_FILE"
    [ "$level" = "error" ] && echo "[ERROR] $*" >&2
}

log_info() { log "info" "$@"; }
log_warn() { log "warn" "$@"; }
log_error() { log "error" "$@"; }

# 初始化目录
init_dirs() {
    mkdir -p "$RUN_DIR"
    mkdir -p "/var/run/proxypool/clients"
    mkdir -p "/var/run/proxypool/redsocks"
    mkdir -p "/var/log/proxypool"
    touch "$LOG_FILE"
}

# 读取UCI配置
get_config() {
    local section="$1"
    local option="$2"
    local default="$3"
    uci -q get "proxypool.$section.$option" || echo "$default"
}

# 获取所有客户端
get_clients() {
    uci show proxypool | grep "=client" | cut -d'.' -f2 | cut -d'=' -f1
}

# 获取客户端绑定的IP列表
get_bind_ips() {
    local client="$1"
    uci -q get "proxypool.$client.bind_ip" 2>/dev/null || true
}

# 检查客户端是否启用
is_client_enabled() {
    local client="$1"
    local enabled=$(get_config "$client" "enabled" "0")
    [ "$enabled" = "1" ]
}

# 启动所有客户端
start_all_clients() {
    log_info "Starting all proxy clients..."

    local clients=$(get_clients)
    local count=0

    for client in $clients; do
        if is_client_enabled "$client"; then
            start_client "$client" &
            ((count++))
        fi
    done

    wait
    log_info "Started $count clients"
}

# 启动单个客户端
start_client() {
    local client="$1"
    local type=$(get_config "$client" "type" "")
    local name=$(get_config "$client" "name" "$client")

    log_info "Starting client: $name ($type)"

    case "$type" in
        l2tp)
            "$SCRIPT_DIR/l2tp-manager.sh" start "$client"
            ;;
        socks5)
            "$SCRIPT_DIR/socks5-manager.sh" start "$client"
            ;;
        *)
            log_error "Unknown client type: $type for $client"
            return 1
            ;;
    esac

    # 更新防火墙规则
    "$SCRIPT_DIR/firewall.sh" update_client "$client"
}

# 停止单个客户端
stop_client() {
    local client="$1"
    local type=$(get_config "$client" "type" "")
    local name=$(get_config "$client" "name" "$client")

    log_info "Stopping client: $name"

    case "$type" in
        l2tp)
            "$SCRIPT_DIR/l2tp-manager.sh" stop "$client"
            ;;
        socks5)
            "$SCRIPT_DIR/socks5-manager.sh" stop "$client"
            ;;
    esac

    # 移除防火墙规则
    "$SCRIPT_DIR/firewall.sh" remove_client "$client"
}

# 停止所有客户端
stop_all_clients() {
    log_info "Stopping all proxy clients..."

    local clients=$(get_clients)

    for client in $clients; do
        stop_client "$client" &
    done

    wait
    log_info "All clients stopped"
}

# 重启单个客户端
restart_client() {
    local client="$1"
    stop_client "$client"
    sleep 2
    start_client "$client"
}

# 重载配置
reload_config() {
    log_info "Reloading configuration..."

    # 获取当前运行的客户端
    local running_clients=""
    if [ -d "$RUN_DIR/clients" ]; then
        running_clients=$(ls "$RUN_DIR/clients" 2>/dev/null || true)
    fi

    # 获取配置中的客户端
    local config_clients=$(get_clients)

    # 停止已删除的客户端
    for client in $running_clients; do
        if ! echo "$config_clients" | grep -q "^${client}$"; then
            log_info "Removing deleted client: $client"
            stop_client "$client"
        fi
    done

    # 启动/更新配置中的客户端
    for client in $config_clients; do
        if is_client_enabled "$client"; then
            if [ -f "$RUN_DIR/clients/$client" ]; then
                # 检查配置是否变更
                local current_hash=$(cat "$RUN_DIR/clients/$client")
                local new_hash=$(uci show "proxypool.$client" | md5sum | cut -d' ' -f1)
                if [ "$current_hash" != "$new_hash" ]; then
                    log_info "Config changed for $client, restarting..."
                    restart_client "$client"
                fi
            else
                start_client "$client"
            fi
        else
            # 客户端被禁用
            if [ -f "$RUN_DIR/clients/$client" ]; then
                stop_client "$client"
            fi
        fi
    done

    # 重建防火墙规则
    "$SCRIPT_DIR/firewall.sh" rebuild

    log_info "Configuration reloaded"
}

# 获取状态
status() {
    "$SCRIPT_DIR/status.sh" get
}

# 主入口
main() {
    local action="$1"
    shift

    init_dirs

    case "$action" in
        start)
            # 初始化防火墙
            "$SCRIPT_DIR/firewall.sh" init
            start_all_clients
            ;;
        stop)
            stop_all_clients
            "$SCRIPT_DIR/firewall.sh" cleanup
            ;;
        restart)
            stop_all_clients
            "$SCRIPT_DIR/firewall.sh" cleanup
            sleep 2
            "$SCRIPT_DIR/firewall.sh" init
            start_all_clients
            ;;
        reload)
            reload_config
            ;;
        start_client)
            start_client "$1"
            ;;
        stop_client)
            stop_client "$1"
            ;;
        restart_client)
            restart_client "$1"
            ;;
        status)
            status
            ;;
        *)
            echo "Usage: $0 {start|stop|restart|reload|start_client|stop_client|restart_client|status} [client_id]"
            exit 1
            ;;
    esac
}

main "$@"
