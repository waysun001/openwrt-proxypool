#!/bin/sh
# 智联盒子 - 代理池主控脚本

SCRIPT_DIR="/usr/lib/proxypool"
CONFIG_FILE="/etc/config/proxypool"
RUN_DIR="/var/run/proxypool"
LOG_FILE="/var/log/proxypool.log"

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
    uci show proxypool 2>/dev/null | grep "=client" | cut -d'.' -f2 | cut -d'=' -f1
}

# 检查客户端是否启用
is_client_enabled() {
    local client="$1"
    local enabled=$(get_config "$client" "enabled" "0")
    [ "$enabled" = "1" ]
}

# ============================================================
# Stopping 标记：防止断开瞬间 IP 泄漏
# 标记客户端为"正在停止"，firewall rebuild 时视为离线
# ============================================================

_mark_stopping() {
    mkdir -p "$RUN_DIR/stopping"
    touch "$RUN_DIR/stopping/$1"
}

_clear_stopping() {
    rm -f "$RUN_DIR/stopping/$1"
}

# ============================================================
# 内部函数：启停客户端但不触发 firewall rebuild
# 供批量操作使用，避免 N+1 次 rebuild
# ============================================================

_start_client_nofirewall() {
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
        slp)
            "$SCRIPT_DIR/slp-manager.sh" start "$client"
            ;;
        *)
            log_error "Unknown client type: $type for $client"
            return 1
            ;;
    esac
}

_stop_client_nofirewall() {
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
        slp)
            "$SCRIPT_DIR/slp-manager.sh" stop "$client"
            ;;
    esac
}

# ============================================================
# 公开函数：启停客户端 + 单次 firewall rebuild
# ============================================================

# 启动单个客户端
start_client() {
    local client="$1"
    _start_client_nofirewall "$client"
    "$SCRIPT_DIR/firewall.sh" rebuild
}

# 停止单个客户端
# 顺序：标记停止 → 重建防火墙（移除规则） → 杀进程 → 清标记
# 防止"先杀进程后重建防火墙"导致的 IP 泄漏窗口期
stop_client() {
    local client="$1"
    _mark_stopping "$client"
    "$SCRIPT_DIR/firewall.sh" rebuild
    _stop_client_nofirewall "$client"
    _clear_stopping "$client"
}

# 重启单个客户端
# 顺序：标记停止 → 重建防火墙（阻断流量） → 杀进程 → 清标记 → 重启 → 重建防火墙（放行）
restart_client() {
    local client="$1"
    _mark_stopping "$client"
    "$SCRIPT_DIR/firewall.sh" rebuild
    _stop_client_nofirewall "$client"
    _clear_stopping "$client"
    sleep 1
    _start_client_nofirewall "$client"
    "$SCRIPT_DIR/firewall.sh" rebuild
}

save_restart_client() {
    local client="$1"
    _mark_stopping "$client"
    "$SCRIPT_DIR/firewall.sh" rebuild
    _stop_client_nofirewall "$client"
    _clear_stopping "$client"
    sleep 1
    _start_client_nofirewall "$client"
    "$SCRIPT_DIR/firewall.sh" rebuild
}

# 切换客户端启用/禁用状态（LuCI toggle 开关专用）
# 读取当前 enabled 状态，执行对应的 stop/start + 单次 rebuild
toggle_client() {
    local client="$1"
    local enabled=$(get_config "$client" "enabled" "0")

    if [ "$enabled" = "1" ]; then
        log_info "Toggle: enabling client $client"
        _start_client_nofirewall "$client"
        "$SCRIPT_DIR/firewall.sh" rebuild
    else
        log_info "Toggle: disabling client $client"
        _mark_stopping "$client"
        "$SCRIPT_DIR/firewall.sh" rebuild
        _stop_client_nofirewall "$client"
        _clear_stopping "$client"
    fi
}

# 启动所有客户端（批量启动后单次 rebuild）
start_all_clients() {
    log_info "Starting all proxy clients..."

    local clients=$(get_clients)
    local count=0

    for client in $clients; do
        if is_client_enabled "$client"; then
            _start_client_nofirewall "$client"
            count=$((count + 1))
        fi
    done

    # 批量启动完成后单次 rebuild
    "$SCRIPT_DIR/firewall.sh" rebuild

    log_info "Started $count clients"
}

# 停止所有客户端（批量停止，不单独 rebuild，由 caller 统一处理）
stop_all_clients() {
    log_info "Stopping all proxy clients..."

    local clients=$(get_clients)

    for client in $clients; do
        _stop_client_nofirewall "$client"
    done

    log_info "All clients stopped"
}

# 重载配置（所有变更用 _nofirewall，最后单次 rebuild）
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
            _stop_client_nofirewall "$client"
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
                    _stop_client_nofirewall "$client"
                    sleep 2
                    _start_client_nofirewall "$client"
                fi
            else
                _start_client_nofirewall "$client"
            fi
        else
            # 客户端被禁用
            if [ -f "$RUN_DIR/clients/$client" ]; then
                _stop_client_nofirewall "$client"
            fi
        fi
    done

    # 所有变更完成后单次 rebuild
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
        save_restart_client)
            save_restart_client "$1"
            ;;
        toggle_client)
            toggle_client "$1"
            ;;
        status)
            status
            ;;
        *)
            echo "Usage: $0 {start|stop|restart|reload|start_client|stop_client|restart_client|toggle_client|status} [client_id]"
            exit 1
            ;;
    esac
}

main "$@"
