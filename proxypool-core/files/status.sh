#!/bin/bash
# 智联盒子 - 状态监控脚本

RUN_DIR="/var/run/proxypool"
LOG_FILE="/var/log/proxypool.log"

get_config() {
    uci -q get "proxypool.$1.$2" || echo "$3"
}

get_clients() {
    uci show proxypool 2>/dev/null | grep "=client" | cut -d'.' -f2 | cut -d'=' -f1
}

format_bytes() {
    local bytes="$1"
    if [ "$bytes" -ge 1073741824 ] 2>/dev/null; then
        awk "BEGIN {printf \"%.2f GB\", $bytes/1073741824}"
    elif [ "$bytes" -ge 1048576 ] 2>/dev/null; then
        awk "BEGIN {printf \"%.2f MB\", $bytes/1048576}"
    elif [ "$bytes" -ge 1024 ] 2>/dev/null; then
        awk "BEGIN {printf \"%.2f KB\", $bytes/1024}"
    else
        echo "${bytes} B"
    fi
}

get_client_status() {
    local client="$1"
    local type=$(get_config "$client" "type" "")
    local name=$(get_config "$client" "name" "$client")
    local server=$(get_config "$client" "server" "")
    local port=$(get_config "$client" "port" "")
    local enabled=$(get_config "$client" "enabled" "0")
    local bind_ips=$(uci -q get "proxypool.$client.bind_ip" 2>/dev/null | tr ' ' ',')
    local status="offline"
    local rx=0
    local tx=0

    if [ "$enabled" = "1" ]; then
        case "$type" in
            l2tp)
                local result=$(/usr/lib/proxypool/l2tp-manager.sh status "$client" 2>/dev/null || echo "disconnected")
                status=$(echo "$result" | head -1)
                ;;
            socks5)
                local result=$(/usr/lib/proxypool/socks5-manager.sh status "$client" 2>/dev/null || echo "disconnected")
                status=$(echo "$result" | head -1)
                ;;
            *)
                status="disconnected"
                ;;
        esac
    else
        status="disabled"
    fi

    local bind_ips_json="[]"
    if [ -n "$bind_ips" ]; then
        bind_ips_json="[\"$(echo "$bind_ips" | sed 's/,/","/g')\"]"
    fi

    cat << EOF
{
  "id": "$client",
  "name": "$name",
  "type": "$type",
  "server": "$server",
  "port": "$port",
  "enabled": $enabled,
  "status": "$status",
  "bind_ips": $bind_ips_json,
  "rx_bytes": $rx,
  "tx_bytes": $tx,
  "rx_human": "$(format_bytes $rx)",
  "tx_human": "$(format_bytes $tx)"
}
EOF
}

get_bound_devices() {
    local clients=$(get_clients)
    local devices="["
    local first=1

    for client in $clients; do
        local bind_ips=$(uci -q get "proxypool.$client.bind_ip" 2>/dev/null || true)
        local client_name=$(get_config "$client" "name" "$client")

        for ip in $bind_ips; do
            local mac=$(ip neigh show 2>/dev/null | grep "^$ip " | awk '{print $5}')
            local online="false"
            if [ -n "$mac" ] && [ "$mac" != "FAILED" ]; then
                online="true"
            fi

            if [ $first -eq 0 ]; then
                devices="$devices,"
            fi
            first=0
            devices="$devices{\"ip\":\"$ip\",\"mac\":\"$mac\",\"online\":$online,\"client\":\"$client\",\"client_name\":\"$client_name\"}"
        done
    done

    devices="$devices]"
    echo "$devices"
}

get_full_status() {
    mkdir -p "$RUN_DIR"

    local clients=$(get_clients)
    local global_enabled=$(get_config "global" "enabled" "1")
    local total=0
    local connected=0
    local enabled_count=0
    local clients_json="["
    local first=1

    for client in $clients; do
        local status_json=$(get_client_status "$client")

        if [ $first -eq 0 ]; then
            clients_json="$clients_json,"
        fi
        first=0
        clients_json="$clients_json$status_json"

        total=$((total + 1))

        local enabled=$(get_config "$client" "enabled" "0")
        if [ "$enabled" = "1" ]; then
            enabled_count=$((enabled_count + 1))
        fi

        local client_status=$(echo "$status_json" | grep '"status"' | cut -d'"' -f4)
        if [ "$client_status" = "connected" ]; then
            connected=$((connected + 1))
        fi
    done

    clients_json="$clients_json]"
    local devices=$(get_bound_devices)
    local disconnected=$((enabled_count - connected))

    cat << EOF
{
  "timestamp": $(date +%s),
  "datetime": "$(date '+%Y-%m-%d %H:%M:%S')",
  "global_enabled": $global_enabled,
  "summary": {
    "total": $total,
    "enabled": $enabled_count,
    "connected": $connected,
    "disconnected": $disconnected
  },
  "clients": $clients_json,
  "devices": $devices
}
EOF
}

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
    *)
        echo "Usage: $0 {get|client|devices} [client_id]"
        exit 1
        ;;
esac
