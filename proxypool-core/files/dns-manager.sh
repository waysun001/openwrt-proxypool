#!/bin/sh
# 智联盒子 - DNS 代理管理脚本
# 通过 SLP 客户端内置 DNS 代理（redir-host 模式）绕过 DNS 污染
# 不再依赖 dnscrypt-proxy2，DNS 查询直接通过 QUIC 隧道转发
#
# 链路: dnsmasq(:53) → SLP DNS Proxy(:dns_port) → [QUIC tunnel] → 8.8.8.8:53

SLP_RUN_DIR="/var/run/proxypool/slp"
RUN_DIR="/var/run/proxypool"
DNS_PORT_FILE="$RUN_DIR/dns-proxy-port"
LOG_FILE="/var/log/proxypool.log"

log_info() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [dns] $*" >> "$LOG_FILE"
}

log_error() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [dns] [ERROR] $*" >> "$LOG_FILE"
}

# 查找第一个存活的 SLP 客户端 DNS 代理端口
find_dns_port() {
    for client_dir in "$SLP_RUN_DIR"/*/; do
        [ -d "$client_dir" ] || continue
        local pid_file="${client_dir}slp.pid"
        local port_file="${client_dir}dns.port"
        if [ -f "$pid_file" ] && [ -f "$port_file" ]; then
            local pid=$(cat "$pid_file")
            if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
                cat "$port_file"
                return 0
            fi
        fi
    done
    return 1
}

# ============================================================
# 配置 DNS 代理（SLP 客户端启动后调用）
# ============================================================
configure() {
    local dns_port="$1"

    # 未指定端口则自动查找
    if [ -z "$dns_port" ]; then
        dns_port=$(find_dns_port)
        if [ -z "$dns_port" ]; then
            log_info "No SLP client available, skipping DNS configuration"
            return 1
        fi
    fi

    # 已配置相同端口则跳过
    if [ -f "$DNS_PORT_FILE" ]; then
        local current_port=$(cat "$DNS_PORT_FILE")
        if [ "$current_port" = "$dns_port" ]; then
            # 确认 SLP DNS 代理仍在监听
            if nc -z -w 1 127.0.0.1 "$dns_port" >/dev/null 2>&1; then
                return 0
            fi
        fi
    fi

    log_info "Configuring DNS: dnsmasq → SLP DNS Proxy(:$dns_port) → [QUIC tunnel] → 8.8.8.8:53"

    echo "$dns_port" > "$DNS_PORT_FILE"

    # 配置 dnsmasq 指向 SLP 内置 DNS 代理
    _configure_dnsmasq "$dns_port"

    log_info "DNS proxy active on port $dns_port"
}

# ============================================================
# 恢复 ISP DNS（proxypool 停止或无 SLP 客户端时调用）
# ============================================================
restore() {
    log_info "Restoring DNS to ISP default"

    # 恢复 dnsmasq 为系统默认 DNS
    _restore_dnsmasq

    rm -f "$DNS_PORT_FILE"
}

# ============================================================
# 检查 DNS 代理状态，必要时切换端口或恢复
# （SLP 客户端断开时调用）
# ============================================================
check() {
    # 未配置过则尝试配置
    if [ ! -f "$DNS_PORT_FILE" ]; then
        configure
        return
    fi

    local current_port=$(cat "$DNS_PORT_FILE")

    # 当前端口仍然存活则无需操作
    if nc -z -w 1 127.0.0.1 "$current_port" >/dev/null 2>&1; then
        return 0
    fi

    # 当前端口失效，寻找替代
    local new_port=$(find_dns_port)
    if [ -n "$new_port" ]; then
        log_info "DNS proxy switching: port $current_port → $new_port"
        configure "$new_port"
    else
        log_info "No SLP clients available, restoring ISP DNS"
        restore
    fi
}

# ============================================================
# dnsmasq 配置管理
# ============================================================

_configure_dnsmasq() {
    local dns_port="$1"
    uci set dhcp.@dnsmasq[0].noresolv='1'
    uci delete dhcp.@dnsmasq[0].server 2>/dev/null
    uci add_list dhcp.@dnsmasq[0].server="127.0.0.1#${dns_port}"
    uci commit dhcp
    /etc/init.d/dnsmasq restart 2>/dev/null
}

_restore_dnsmasq() {
    uci set dhcp.@dnsmasq[0].noresolv='0'
    uci delete dhcp.@dnsmasq[0].server 2>/dev/null
    uci commit dhcp
    /etc/init.d/dnsmasq restart 2>/dev/null
}

case "$1" in
    configure)
        configure "$2"
        ;;
    restore)
        restore
        ;;
    check)
        check
        ;;
    *)
        echo "Usage: $0 {configure|restore|check} [dns_port]"
        exit 1
        ;;
esac
