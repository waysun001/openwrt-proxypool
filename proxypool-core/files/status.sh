#!/bin/sh
# 智联盒子 - 状态监控脚本

RUN_DIR=${PROXYPOOL_STATUS_RUN_DIR:-/var/run/proxypool}
IPLOCATION_COMMAND=${PROXYPOOL_STATUS_IPLOCATION_COMMAND:-/usr/lib/proxypool/iplocation.sh}
DNS_PATH_STATUS=dns_path_unavailable

# UCI list values are split deliberately below.  Disabling pathname expansion
# prevents an untrusted value from turning a status read into a directory scan.
set -f

# JSON 字符串转义：处理双引号、反斜杠、换行符、制表符等特殊字符
json_escape() {
    printf '%s' "$1" |
        sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/	/\\t/g' |
        LC_ALL=C tr -d '\000-\037\177'
}

json_flag() {
    if [ "${1:-}" = "1" ]; then
        printf '%s\n' 1
    else
        printf '%s\n' 0
    fi
}

json_uint_or_zero() {
    local value="${1:-}"

    case "$value" in
        ''|*[!0-9]*)
            printf '%s\n' 0
            return
            ;;
    esac

    while [ "${value#0}" != "$value" ]; do
        value=${value#0}
    done
    [ -n "$value" ] || value=0
    printf '%s\n' "$value"
}

json_word_array() {
    local values="$1"
    local result="["
    local first=1
    local value

    for value in $values; do
        if [ "$first" -eq 0 ]; then
            result="$result,"
        fi
        first=0
        result="$result\"$(json_escape "$value")\""
    done

    printf '%s]' "$result"
}

get_config() {
    local val
    val=$(uci -q get "proxypool.$1.$2" 2>/dev/null)
    if [ -z "$val" ]; then
        echo "$3"
    else
        echo "$val"
    fi
}

get_clients() {
    uci show proxypool 2>/dev/null | grep "=client" | cut -d'.' -f2 | cut -d'=' -f1
}

format_bytes() {
    local bytes="${1:-0}"
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

is_strict_ipv4_literal() {
    address=$1
    case "$address" in
        ''|.*|*.|*..*|*[!0-9.]*) return 1 ;;
    esac

    old_ifs=$IFS
    IFS=.
    set -- $address
    IFS=$old_ifs
    [ "$#" -eq 4 ] || return 1

    for octet in "$@"; do
        # Reject ambiguous leading-zero and overlong representations.
        case "$octet" in
            0|[1-9]|[1-9][0-9]|[1-9][0-9][0-9]) ;;
            *) return 1 ;;
        esac
        [ "$octet" -le 255 ] 2>/dev/null || return 1
    done
    return 0
}

endpoint_resolution_status() {
    if [ -z "$1" ]; then
        printf '%s\n' missing_endpoint
    elif is_strict_ipv4_literal "$1"; then
        printf '%s\n' literal_ipv4
    else
        printf '%s\n' "$DNS_PATH_STATUS"
    fi
}

is_safe_location_cache_key() {
    case "$1" in
        ''|*..*|*[!A-Za-z0-9._:-]*) return 1 ;;
        *) return 0 ;;
    esac
}

read_probe_status() {
    local client="$1"
    local probe_file="$RUN_DIR/probe/${client}"
    local probe_result=""

    if [ -f "$probe_file" ] && [ ! -L "$probe_file" ]; then
        probe_result=$(cat "$probe_file" 2>/dev/null)
    fi

    if [ "$probe_result" = "ok" ]; then
        printf '%s\n' connected
    else
        printf '%s\n' disconnected
    fi
}

pid_file_is_live() {
    local pid_file="$1"
    local pid=""

    [ -f "$pid_file" ] && [ ! -L "$pid_file" ] || return 1
    pid=$(cat "$pid_file" 2>/dev/null)
    case "$pid" in
        ''|*[!0-9]*) return 1 ;;
    esac
    kill -0 "$pid" 2>/dev/null
}

read_backend_runtime_status() {
    local type="$1"
    local client="$2"

    case "$type" in
        l2tp)
            local ppp_iface="ppp-${client}"
            if ip link show "$ppp_iface" >/dev/null 2>&1; then
                local ppp_ip=$(ip -4 addr show "$ppp_iface" 2>/dev/null | grep -o 'inet [0-9.]*' | cut -d' ' -f2 | head -1)
                if [ -n "$ppp_ip" ]; then
                    read_probe_status "$client"
                    printf '%s\n' "$ppp_ip"
                    return
                fi
            fi
            if pid_file_is_live "$RUN_DIR/l2tp/$client/xl2tpd.pid"; then
                printf '%s\n' connecting
            else
                printf '%s\n' disconnected
            fi
            ;;
        socks5)
            if pid_file_is_live "$RUN_DIR/redsocks/${client}.pid"; then
                read_probe_status "$client"
            else
                printf '%s\n' disconnected
            fi
            ;;
        slp)
            if pid_file_is_live "$RUN_DIR/slp/$client/slp.pid" &&
               pid_file_is_live "$RUN_DIR/redsocks/${client}.pid"; then
                read_probe_status "$client"
            elif pid_file_is_live "$RUN_DIR/slp/$client/slp.pid"; then
                printf '%s\n' connecting
            else
                printf '%s\n' disconnected
            fi
            ;;
        *)
            printf '%s\n' disconnected
            ;;
    esac
}

get_client_status() {
    local client="$1"
    local nft_out_cache="$2"
    local nft_in_cache="$3"
    local type=$(get_config "$client" "type" "")
    local name=$(get_config "$client" "name" "$client")
    local server=$(get_config "$client" "server" "")
    local endpoint_resolution=$(endpoint_resolution_status "$server")
    local port=$(get_config "$client" "port" "")
    local username=$(get_config "$client" "username" "")
    local password=$(get_config "$client" "password" "")
    local expiry=$(get_config "$client" "expiry" "")
    local remark=$(get_config "$client" "remark" "")
    
    # IP归属地查询（使用内置脚本，开箱即用，带缓存）
    local location=""
    if [ -n "$server" ]; then
        local cache_file=""
        if is_safe_location_cache_key "$server"; then
            cache_file="$RUN_DIR/location_cache/${server}.txt"
        fi
        if [ -n "$cache_file" ] && [ -f "$cache_file" ] && [ ! -L "$cache_file" ]; then
            # 使用缓存（5分钟有效期）
            local now=$(json_uint_or_zero "$(date +%s 2>/dev/null)")
            local modified=$(json_uint_or_zero "$(stat -c %Y "$cache_file" 2>/dev/null)")
            local cache_age=300
            if [ "$now" -ge "$modified" ] 2>/dev/null; then
                cache_age=$((now - modified))
            fi
            if [ $cache_age -lt 300 ]; then
                location=$(cat "$cache_file" 2>/dev/null)
            fi
        fi
        
        # 缓存未命中或过期，重新查询
        if [ -z "$location" ]; then
            location=$("$IPLOCATION_COMMAND" "$server" 2>/dev/null)
        fi
    fi
    local enabled=$(json_flag "$(get_config "$client" "enabled" "0")")
    local bind_ips=$(uci -q get "proxypool.$client.bind_ip" 2>/dev/null)
    local status="offline"
    local rx=0
    local tx=0
    local ip_addr=""

    if [ "$enabled" = "1" ]; then
        local result=$(read_backend_runtime_status "$type" "$client")
        status=$(echo "$result" | head -1)
        if [ "$type" = "l2tp" ] && [ "$status" = "connected" ]; then
            ip_addr=$(echo "$result" | sed -n '2p')
        fi
    else
        status="disabled"
    fi

    # 读取流量统计：持久化累加值 + 当前 nftables counter（适用于所有客户端类型）
    # 从专用计数链 count_out / count_in 读取（捕获 TCP/UDP/ICMP 全协议）
    local counter_dir="$RUN_DIR/counters"
    for bip in $bind_ips; do
        # 持久化累加值（rebuild 前保存的历史流量）
        local saved_out=$(json_uint_or_zero "$(cat "$counter_dir/${bip}.out" 2>/dev/null)")
        local saved_in=$(json_uint_or_zero "$(cat "$counter_dir/${bip}.in" 2>/dev/null)")
        # 当前 nftables counter（本次 rebuild 后的增量，从专用计数链读取）
        local cur_out=$(json_uint_or_zero "$(echo "$nft_out_cache" | grep "comment \"out_$bip\"" | grep -o 'bytes [0-9]*' | awk '{print $2}')")
        local cur_in=$(json_uint_or_zero "$(echo "$nft_in_cache" | grep "comment \"in_$bip\"" | grep -o 'bytes [0-9]*' | awk '{print $2}')")
        tx=$((tx + saved_out + cur_out))
        rx=$((rx + saved_in + cur_in))
    done

    local bind_ips_json=$(json_word_array "$bind_ips")

    # 超时计数
    local timeout_dir="$RUN_DIR/timeout"
    local timeout_today=$(json_uint_or_zero "$(cat "$timeout_dir/${client}.today" 2>/dev/null)")
    local timeout_yesterday=$(json_uint_or_zero "$(cat "$timeout_dir/${client}.yesterday" 2>/dev/null)")

    # 确保数值变量非空（空值会导致 JSON 断裂）
    enabled=$(json_flag "$enabled")
    rx=$(json_uint_or_zero "$rx")
    tx=$(json_uint_or_zero "$tx")
    timeout_today=$(json_uint_or_zero "$timeout_today")
    timeout_yesterday=$(json_uint_or_zero "$timeout_yesterday")

    # 转义所有字符串字段，防止特殊字符破坏 JSON
    local j_client=$(json_escape "$client")
    local j_name=$(json_escape "$name")
    local j_type=$(json_escape "$type")
    local j_server=$(json_escape "$server")
    local j_endpoint_resolution=$(json_escape "$endpoint_resolution")
    local j_port=$(json_escape "$port")
    local j_username=$(json_escape "$username")
    local j_password=$(json_escape "$password")
    local j_expiry=$(json_escape "$expiry")
    local j_remark=$(json_escape "$remark")
    local j_location=$(json_escape "$location")
    local j_status=$(json_escape "$status")
    local j_ip_addr=$(json_escape "$ip_addr")

    cat << EOF
{
  "id": "$j_client",
  "name": "$j_name",
  "type": "$j_type",
  "server": "$j_server",
  "endpoint_resolution": "$j_endpoint_resolution",
  "port": "$j_port",
  "username": "$j_username",
  "password": "$j_password",
  "expiry": "$j_expiry",
  "remark": "$j_remark",
  "location": "$j_location",
  "enabled": $enabled,
  "status": "$j_status",
  "ip_addr": "$j_ip_addr",
  "bind_ips": $bind_ips_json,
  "rx_bytes": $rx,
  "tx_bytes": $tx,
  "rx_human": "$(format_bytes $rx)",
  "tx_human": "$(format_bytes $tx)",
  "timeout_today": $timeout_today,
  "timeout_yesterday": $timeout_yesterday
}
EOF
}

get_bound_devices() {
    local clients=$(get_clients)
    local devices="["
    local first=1

    # 单次 ip neigh show 缓存全表（替代每个 IP 单独查询，212 次 → 1 次）
    local NEIGH_CACHE=$(ip neigh show 2>/dev/null)

    for client in $clients; do
        local bind_ips=$(uci -q get "proxypool.$client.bind_ip" 2>/dev/null || true)
        local client_name=$(get_config "$client" "name" "$client")

        for ip in $bind_ips; do
            local neigh_entry=$(echo "$NEIGH_CACHE" | grep "^$ip " | head -1)
            local mac=$(echo "$neigh_entry" | awk '{print $5}')
            local state=$(echo "$neigh_entry" | awk '{print $NF}')
            local online="false"
            # 只有 REACHABLE 和 DELAY 状态才算在线
            # STALE=缓存过期 FAILED=不可达 INCOMPLETE=未完成 都不算在线
            if [ -n "$mac" ] && [ "$mac" != "FAILED" ]; then
                case "$state" in
                    REACHABLE|DELAY|PROBE) online="true" ;;
                esac
            fi

            if [ $first -eq 0 ]; then
                devices="$devices,"
            fi
            first=0
            local j_ip=$(json_escape "$ip")
            local j_mac=$(json_escape "$mac")
            local j_client=$(json_escape "$client")
            local j_client_name=$(json_escape "$client_name")
            devices="$devices{\"ip\":\"$j_ip\",\"mac\":\"$j_mac\",\"online\":$online,\"client\":\"$j_client\",\"client_name\":\"$j_client_name\"}"
        done
    done

    devices="$devices]"
    echo "$devices"
}

get_full_status() {
    local clients=$(get_clients)
    local global_enabled=$(json_flag "$(get_config "global" "enabled" "1")")
    local total=0
    local connected=0
    local enabled_count=0
    local clients_json="["
    local first=1

    # 循环前查询一次 nft，缓存结果避免 N 次系统调用
    # 从专用计数链读取（count_out/count_in 捕获全协议流量）
    local NFT_OUT_CACHE=$(nft list chain inet proxypool count_out 2>/dev/null)
    local NFT_IN_CACHE=$(nft list chain inet proxypool count_in 2>/dev/null)

    for client in $clients; do
        local status_json=$(get_client_status "$client" "$NFT_OUT_CACHE" "$NFT_IN_CACHE")

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
    [ "$disconnected" -lt 0 ] 2>/dev/null && disconnected=0
    local timestamp=$(json_uint_or_zero "$(date +%s 2>/dev/null)")
    local datetime=$(date '+%Y-%m-%d %H:%M:%S' 2>/dev/null)
    local j_datetime=$(json_escape "$datetime")

    cat << EOF
{
  "timestamp": $timestamp,
  "datetime": "$j_datetime",
  "global_enabled": $global_enabled,
  "dns_path_status": "$DNS_PATH_STATUS",
  "internet_ready": false,
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
        # 捕获输出，确保即使脚本内部出错也返回有效 JSON
        _output=$(get_full_status 2>/dev/null)
        if [ -z "$_output" ]; then
            echo '{"timestamp":0,"datetime":"","global_enabled":0,"dns_path_status":"dns_path_unavailable","internet_ready":false,"summary":{"total":0,"enabled":0,"connected":0,"disconnected":0},"clients":[],"devices":[],"error":"get_full_status returned empty"}'
        else
            echo "$_output"
        fi
        ;;
    client)
        get_client_status "$2" "$(nft list chain inet proxypool count_out 2>/dev/null)" "$(nft list chain inet proxypool count_in 2>/dev/null)"
        ;;
    devices)
        get_bound_devices
        ;;
    *)
        echo "Usage: $0 {get|client|devices} [client_id]"
        exit 1
        ;;
esac
