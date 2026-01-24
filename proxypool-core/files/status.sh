#!/bin/bash
# ProxyPool 状态监控脚本

set -e

RUN_DIR="/var/run/proxypool"
STATUS_FILE="/var/run/proxypool/status.json"
STATS_DIR="/var/run/proxypool/stats"
LOG_FILE="/var/log/proxypool.log"

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

# 获取接口流量统计
get_interface_stats() {
    local iface="$1"

    if [ -d "/sys/class/net/$iface" ]; then
        local rx=$(cat "/sys/class/net/$iface/statistics/rx_bytes" 2>/dev/null || echo 0)
        local tx=$(cat "/sys/class/net/$iface/statistics/tx_bytes" 2>/dev/null || echo 0)
        echo "$rx:$tx"
    else
        echo "0:0"
    fi
}

# 格式化字节
format_bytes() {
    local bytes="$1"
    if [ "$bytes" -ge 1073741824 ]; then
        echo "$(awk "BEGIN {printf \"%.2f\", $bytes/1073741824}") GB"
    elif [ "$bytes" -ge 1048576 ]; then
        echo "$(awk "BEGIN {printf \"%.2f\", $bytes/1048576}") MB"
    elif [ "$bytes" -ge 1024 ]; then
        echo "$(awk "BEGIN {printf \"%.2f\", $bytes/1024}") KB"
    else
        echo "$bytes B"
    fi
}

# 获取单个客户端状态
get_client_status() {
    local client="$1"
    local type=$(get_config "$client" "type" "")
    local name=$(get_config "$client" "name" "$client")
    local server=$(get_config "$client" "server" "")
    local port=$(get_config "$client" "port" "")
    local enabled=$(get_config "$client" "enabled" "0")

    # 获取绑定的IP列表
    local bind_ips=$(uci -q get "proxypool.$client.bind_ip" 2>/dev/null | tr ' ' ',')

    # 获取连接状态
    local status="offline"
    local ip=""
    local rx=0
    local tx=0

    if [ "$enabled" = "1" ]; then
        case "$type" in
            l2tp)
                local result=$(/usr/lib/proxypool/l2tp-manager.sh status "$client" 2>/dev/null)
                status=$(echo "$result" | head -1)
                ip=$(echo "$result" | tail -1)

                if [ "$status" = "connected" ]; then
                    local stats=$(get_interface_stats "ppp-${client}")
                    rx=$(echo "$stats" | cut -d: -f1)
                    tx=$(echo "$stats" | cut -d: -f2)
                fi
                ;;
            socks5)
                local result=$(/usr/lib/proxypool/socks5-manager.sh status "$client" 2>/dev/null)
                status=$(echo "$result" | head -1)

                if [ "$status" = "connected" ]; then
                    # SOCKS5 流量统计需要从 redsocks 日志获取
                    local log_file="/var/run/proxypool/redsocks/${client}.log"
                    if [ -f "$log_file" ]; then
                        # 简化统计
                        rx=$(stat -c%s "$log_file" 2>/dev/null || echo 0)
                        tx=$rx
                    fi
                fi
                ;;
        esac
    else
        status="disabled"
    fi

    # 输出 JSON
    cat << EOF
{
  "id": "$client",
  "name": "$name",
  "type": "$type",
  "server": "$server",
  "port": "$port",
  "enabled": $enabled,
  "status": "$status",
  "ip": "$ip",
  "bind_ips": "$(echo $bind_ips | sed 's/,/", "/g' | sed 's/^/"/' | sed 's/$/"/' | sed 's/^""$//')",
  "rx_bytes": $rx,
  "tx_bytes": $tx,
  "rx_human": "$(format_bytes $rx)",
  "tx_human": "$(format_bytes $tx)"
}
EOF
}

# 获取绑定设备的在线状态
get_bound_devices() {
    local clients=$(get_clients)
    local devices="["
    local first=1

    for client in $clients; do
        local bind_ips=$(uci -q get "proxypool.$client.bind_ip" 2>/dev/null || true)
        local client_name=$(get_config "$client" "name" "$client")

        for ip in $bind_ips; do
            # 检查设备是否在线 (通过ARP表)
            local mac=$(ip neigh show | grep "^$ip " | awk '{print $5}')
            local online="false"

            if [ -n "$mac" ] && [ "$mac" != "FAILED" ]; then
                online="true"
            fi

            [ $first -eq 0 ] && devices="$devices,"
            first=0

            devices="$devices{\"ip\":\"$ip\",\"mac\":\"$mac\",\"online\":$online,\"client\":\"$client\",\"client_name\":\"$client_name\"}"
        done
    done

    devices="$devices]"
    echo "$devices"
}

# 获取完整状态
get_full_status() {
    mkdir -p "$RUN_DIR"

    local clients=$(get_clients)
    local global_enabled=$(get_config "global" "enabled" "1")

    # 统计
    local total=0
    local connected=0
    local enabled_count=0

    local clients_json="["
    local first=1

    for client in $clients; do
        local status_json=$(get_client_status "$client")

        [ $first -eq 0 ] && clients_json="$clients_json,"
        first=0
        clients_json="$clients_json$status_json"

        ((total++))

        local enabled=$(get_config "$client" "enabled" "0")
        [ "$enabled" = "1" ] && ((enabled_count++))

        local status=$(echo "$status_json" | grep -oP '(?<="status": ")[^"]+')
        [ "$status" = "connected" ] && ((connected++))
    done

    clients_json="$clients_json]"

    # 获取在线设备
    local devices=$(get_bound_devices)

    # 生成完整状态JSON
    cat << EOF
{
  "timestamp": $(date +%s),
  "datetime": "$(date '+%Y-%m-%d %H:%M:%S')",
  "global_enabled": $global_enabled,
  "summary": {
    "total": $total,
    "enabled": $enabled_count,
    "connected": $connected,
    "disconnected": $((enabled_count - connected))
  },
  "clients": $clients_json,
  "devices": $devices
}
EOF
}

# 监控循环
monitor_loop() {
    local interval=$(get_config "global" "status_interval" "30")

    while true; do
        get_full_status > "$STATUS_FILE.tmp"
        mv "$STATUS_FILE.tmp" "$STATUS_FILE"
        sleep "$interval"
    done
}

# 主入口
case "$1" in
    get)
        get_full_status
        ;;
    client)
        get_client_status "$2"
        ;;
    devices)
        get_bound_devices
        ;;
    monitor)
        monitor_loop
        ;;
    *)
        echo "Usage: $0 {get|client|devices|monitor} [client_id]"
        exit 1
        ;;
esac
