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
# 公开函数：启停客户端 + 增量 firewall 操作
# 单客户端操作使用 add_client/remove_client（毫秒级）
# 批量操作仍使用 rebuild（原子化）
# ============================================================

# 探测单个客户端连通性（curl 实际请求百度，准确判断是否可上网）
_probe_client() {
    local client="$1"
    local type=$(get_config "$client" "type" "")
    case "$type" in
        socks5)
            "$SCRIPT_DIR/socks5-manager.sh" probe "$client"
            ;;
        slp)
            "$SCRIPT_DIR/slp-manager.sh" probe "$client"
            ;;
        l2tp)
            "$SCRIPT_DIR/l2tp-manager.sh" probe "$client"
            ;;
    esac
}

# 启动单个客户端（增量添加规则，替代全量 rebuild）
start_client() {
    local client="$1"
    _start_client_nofirewall "$client"
    "$SCRIPT_DIR/firewall.sh" add_client "$client"
    # DNS 代理仅 SLP 客户端需要
    local type=$(get_config "$client" "type" "")
    [ "$type" = "slp" ] && "$SCRIPT_DIR/dns-manager.sh" configure
    # 操作完成，清除 pending 状态文件
    rm -f "$RUN_DIR/pending/$client"
    # 同步探测：API 返回时状态已准确
    _probe_client "$client"
}

# 停止单个客户端（增量移除规则，替代全量 rebuild）
# 顺序：标记停止 → 移除该客户端规则 → 杀进程 → 清标记
# 防止"先杀进程后移除规则"导致的 IP 泄漏窗口期
stop_client() {
    local client="$1"
    local type=$(get_config "$client" "type" "")
    _mark_stopping "$client"
    "$SCRIPT_DIR/firewall.sh" remove_client "$client"
    _stop_client_nofirewall "$client"
    _clear_stopping "$client"
    rm -f "$RUN_DIR/pending/$client"
    # 清除探测缓存（PID 已死，状态码直接判 disconnected）
    rm -f "$RUN_DIR/probe/$client"
    [ "$type" = "slp" ] && "$SCRIPT_DIR/dns-manager.sh" check
}

# 重启单个客户端（增量移除 + 添加，替代两次全量 rebuild）
restart_client() {
    local client="$1"
    local type=$(get_config "$client" "type" "")
    _mark_stopping "$client"
    "$SCRIPT_DIR/firewall.sh" remove_client "$client"
    _stop_client_nofirewall "$client"
    _clear_stopping "$client"
    sleep 1
    _start_client_nofirewall "$client"
    "$SCRIPT_DIR/firewall.sh" add_client "$client"
    rm -f "$RUN_DIR/pending/$client"
    [ "$type" = "slp" ] && "$SCRIPT_DIR/dns-manager.sh" configure
    _probe_client "$client"
}

# 保存配置后重启
save_restart_client() {
    local client="$1"
    local type=$(get_config "$client" "type" "")
    _mark_stopping "$client"
    "$SCRIPT_DIR/firewall.sh" remove_client "$client"
    _stop_client_nofirewall "$client"
    _clear_stopping "$client"
    sleep 1
    _start_client_nofirewall "$client"
    "$SCRIPT_DIR/firewall.sh" add_client "$client"
    rm -f "$RUN_DIR/pending/$client"
    [ "$type" = "slp" ] && "$SCRIPT_DIR/dns-manager.sh" configure
    _probe_client "$client"
}

# 切换客户端启用/禁用状态（LuCI toggle 开关专用，增量操作）
toggle_client() {
    local client="$1"
    local enabled=$(get_config "$client" "enabled" "0")
    local type=$(get_config "$client" "type" "")

    if [ "$enabled" = "1" ]; then
        log_info "Toggle: enabling client $client"
        _start_client_nofirewall "$client"
        "$SCRIPT_DIR/firewall.sh" add_client "$client"
        [ "$type" = "slp" ] && "$SCRIPT_DIR/dns-manager.sh" configure
        rm -f "$RUN_DIR/pending/$client"
        _probe_client "$client"
    else
        log_info "Toggle: disabling client $client"
        _mark_stopping "$client"
        "$SCRIPT_DIR/firewall.sh" remove_client "$client"
        _stop_client_nofirewall "$client"
        _clear_stopping "$client"
        [ "$type" = "slp" ] && "$SCRIPT_DIR/dns-manager.sh" check
        rm -f "$RUN_DIR/pending/$client"
        # 停止后清除探测缓存（PID 已死，状态码直接判 disconnected）
        rm -f "$RUN_DIR/probe/$client"
    fi
}

# ============================================================
# 批量操作：循环调用 _nofirewall 变体，最后单次 rebuild
# 供 LuCI batch_action 异步调用
# ============================================================

batch_connect() {
    log_info "Batch connect: $*"
    for client in "$@"; do
        _start_client_nofirewall "$client"
    done
    "$SCRIPT_DIR/firewall.sh" rebuild
    "$SCRIPT_DIR/dns-manager.sh" configure
    for client in "$@"; do
        rm -f "$RUN_DIR/pending/$client"
    done
    # 并发探测所有客户端连通性（curl 通过代理请求百度）
    for client in "$@"; do
        _probe_client "$client" &
    done
    wait
    log_info "Batch connect done"
}

batch_disconnect() {
    log_info "Batch disconnect: $*"
    for client in "$@"; do
        _mark_stopping "$client"
    done
    "$SCRIPT_DIR/firewall.sh" rebuild
    for client in "$@"; do
        _stop_client_nofirewall "$client"
        _clear_stopping "$client"
        rm -f "$RUN_DIR/pending/$client"
        rm -f "$RUN_DIR/probe/$client"
    done
    "$SCRIPT_DIR/dns-manager.sh" check
    log_info "Batch disconnect done"
}

batch_enable() {
    log_info "Batch enable: $*"
    for client in "$@"; do
        _start_client_nofirewall "$client"
    done
    "$SCRIPT_DIR/firewall.sh" rebuild
    "$SCRIPT_DIR/dns-manager.sh" configure
    for client in "$@"; do
        rm -f "$RUN_DIR/pending/$client"
    done
    # 并发探测
    for client in "$@"; do
        _probe_client "$client" &
    done
    wait
    log_info "Batch enable done"
}

batch_disable() {
    log_info "Batch disable: $*"
    for client in "$@"; do
        _mark_stopping "$client"
    done
    "$SCRIPT_DIR/firewall.sh" rebuild
    for client in "$@"; do
        _stop_client_nofirewall "$client"
        _clear_stopping "$client"
        rm -f "$RUN_DIR/pending/$client"
        rm -f "$RUN_DIR/probe/$client"
    done
    "$SCRIPT_DIR/dns-manager.sh" check
    log_info "Batch disable done"
}

# ============================================================
# 逐个启动：和手动点"连接"完全一致
# 每个客户端依次执行：启动进程 → 增量防火墙 → 同步探测
# 完成后状态即准确（已连接/未连接），无假"已连接"
# 后台运行（由 Lua 层 setsid 调用），不阻塞 API 响应
# ============================================================
sequential_start() {
    log_info "Sequential start: $# clients"
    local _done=0
    for client in "$@"; do
        if is_client_enabled "$client"; then
            start_client "$client"
            _done=$((_done + 1))
            log_info "Sequential start: $_done done ($client)"
        fi
    done
    log_info "Sequential start complete: $_done clients started"
}

batch_delete() {
    log_info "Batch delete: $*"
    for client in "$@"; do
        _mark_stopping "$client"
    done
    "$SCRIPT_DIR/firewall.sh" rebuild
    for client in "$@"; do
        _stop_client_nofirewall "$client"
        _clear_stopping "$client"
        rm -f "$RUN_DIR/pending/$client"
        rm -f "$RUN_DIR/probe/$client"
    done
    "$SCRIPT_DIR/dns-manager.sh" check
    log_info "Batch delete done"
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

# 后台并发探测所有活跃客户端连通性
# 分批并发（每批 PROBE_BATCH_SIZE 个），防止瞬间进程风暴
# 互斥锁防止多次 status 轮询导致 probe_all 叠加运行
PROBE_BATCH_SIZE=20
PROBE_LOCK="$RUN_DIR/probe.lock"

probe_all() {
    # 互斥锁：如果已有 probe_all 在运行，直接退出
    if [ -f "$PROBE_LOCK" ]; then
        local lock_pid=$(cat "$PROBE_LOCK" 2>/dev/null)
        if [ -n "$lock_pid" ] && kill -0 "$lock_pid" 2>/dev/null; then
            return 0
        fi
        # 进程已死，清理残留锁
        rm -f "$PROBE_LOCK"
    fi
    echo "$$" > "$PROBE_LOCK"
    # 确保退出时释放锁
    trap 'rm -f "$PROBE_LOCK"' EXIT

    local clients=$(get_clients)
    local count=0
    for client in $clients; do
        if is_client_enabled "$client" && [ -f "$RUN_DIR/clients/$client" ]; then
            local type=$(get_config "$client" "type" "")
            case "$type" in
                socks5)
                    "$SCRIPT_DIR/socks5-manager.sh" probe "$client" &
                    ;;
                slp)
                    "$SCRIPT_DIR/slp-manager.sh" probe "$client" &
                    ;;
                l2tp)
                    "$SCRIPT_DIR/l2tp-manager.sh" probe "$client" &
                    ;;
                *)
                    continue
                    ;;
            esac
            count=$((count + 1))
            # 达到批次上限，等待本批完成再继续
            if [ $count -ge $PROBE_BATCH_SIZE ]; then
                wait
                count=0
            fi
        fi
    done
    wait

    rm -f "$PROBE_LOCK"
    trap - EXIT
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
            # 启动前先恢复 ISP DNS（防止上次异常退出遗留 noresolv 配置导致 DNS 不可用）
            "$SCRIPT_DIR/dns-manager.sh" restore
            "$SCRIPT_DIR/firewall.sh" init
            start_all_clients
            # 所有客户端启动后，配置 DNS 走 SLP 隧道
            "$SCRIPT_DIR/dns-manager.sh" configure
            ;;
        stop)
            # 先恢复 ISP DNS（确保停止期间路由器自身 DNS 可用）
            "$SCRIPT_DIR/dns-manager.sh" restore
            stop_all_clients
            "$SCRIPT_DIR/firewall.sh" cleanup
            ;;
        restart)
            "$SCRIPT_DIR/dns-manager.sh" restore
            stop_all_clients
            "$SCRIPT_DIR/firewall.sh" cleanup
            sleep 2
            "$SCRIPT_DIR/firewall.sh" init
            start_all_clients
            "$SCRIPT_DIR/dns-manager.sh" configure
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
        probe_all)
            probe_all
            ;;
        batch_connect)
            batch_connect "$@"
            ;;
        batch_disconnect)
            batch_disconnect "$@"
            ;;
        batch_enable)
            batch_enable "$@"
            ;;
        batch_disable)
            batch_disable "$@"
            ;;
        batch_delete)
            batch_delete "$@"
            ;;
        sequential_start)
            sequential_start "$@"
            ;;
        *)
            echo "Usage: $0 {start|stop|restart|reload|start_client|stop_client|restart_client|toggle_client|batch_connect|batch_disconnect|batch_enable|batch_disable|batch_delete|sequential_start|status} [client_id...]"
            exit 1
            ;;
    esac
}

main "$@"
