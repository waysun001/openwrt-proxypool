#!/bin/sh
# 智联盒子 - L2TP 客户端管理脚本（独立实例模式）
# 每个客户端独立 xl2tpd 进程，互不影响
# 禁用系统 xl2tpd，避免端口冲突

RUN_DIR="/var/run/proxypool"
L2TP_RUN_DIR="/var/run/proxypool/l2tp"
PROBE_DIR="/var/run/proxypool/probe"
PPP_DIR="/etc/ppp/peers"
LOG_FILE="/var/log/proxypool.log"

# 端口设置（0 = 系统自动分配随机端口）
LOCAL_PORT=0

log_info() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [l2tp] $*" >> "$LOG_FILE"
}

log_error() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [l2tp] [ERROR] $*" >> "$LOG_FILE"
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

# 确保系统 xl2tpd 已禁用（避免占用 1701 端口）
ensure_system_xl2tpd_disabled() {
    if /etc/init.d/xl2tpd enabled 2>/dev/null; then
        /etc/init.d/xl2tpd stop 2>/dev/null
        /etc/init.d/xl2tpd disable 2>/dev/null
        log_info "Disabled system xl2tpd to avoid port conflict"
    fi
    # 确保没有残留的系统 xl2tpd 进程占用 1701 端口
    local sys_pid=$(cat /var/run/xl2tpd.pid 2>/dev/null)
    if [ -n "$sys_pid" ] && kill -0 "$sys_pid" 2>/dev/null; then
        kill "$sys_pid" 2>/dev/null
        sleep 1
    fi
}

# 生成独立 xl2tpd 配置
generate_xl2tpd_config() {
    local client="$1"
    local server=$(get_config "$client" "server" "" | tr -d ' \t\n\r')
    local port=$(get_config "$client" "port" "1701" | tr -d ' \t\n\r')
    local username=$(get_config "$client" "username" "" | tr -d ' \t\n\r')

    if [ -z "$server" ]; then
        log_error "No server configured for $client"
        return 1
    fi

    local client_dir="$L2TP_RUN_DIR/$client"
    mkdir -p "$client_dir"

    local lac_name="lac_${client}"

    # 如果端口不是默认的1701，则附加到服务器地址
    local lns_addr="${server}"
    if [ -n "$port" ] && [ "$port" != "1701" ]; then
        lns_addr="${server}:${port}"
    fi

    cat > "$client_dir/xl2tpd.conf" << EOF
[global]
port = ${LOCAL_PORT}
access control = no
auth file = /etc/ppp/chap-secrets

[lac ${lac_name}]
lns = ${lns_addr}
pppoptfile = ${PPP_DIR}/proxypool-${client}
redial = yes
redial timeout = 15
max redials = 3
require chap = yes
require pap = no
require authentication = no
ppp debug = yes
name = ${username}
autodial = no
length bit = yes
flow bit = yes
EOF

    log_info "Generated xl2tpd config for $client"
}

# 生成 PPP 配置和 chap-secrets
generate_ppp_config() {
    local client="$1"
    local username=$(get_config "$client" "username" "" | tr -d ' \t\n\r')
    local password=$(get_config "$client" "password" "" | tr -d ' \t\n\r')

    local ppp_file="${PPP_DIR}/proxypool-${client}"
    local secrets_file="/etc/ppp/chap-secrets"

    mkdir -p "$PPP_DIR"

    local unit_num=$(get_client_num "$client")

    # 账号用内联 user/password（对齐参考版 netifd l2tp）：密码对 PAP 和 CHAP 都
    # 生效。原来只写 name + chap-secrets，服务器若用 PAP 会查不到密码导致认证失败、
    # PPP 起不来。去掉 refuse-eap，让 pppd 自动协商认证方式。
    cat > "$ppp_file" << EOF
# 智联盒子 L2TP Client: $client
user "${username}"
password "${password}"

ifname ppp-${client}
unit ${unit_num}

debug
logfile /var/log/proxypool.log
noauth
nodeflate
nobsdcomp
nopcomp
noaccomp
noipdefault
noipv6
usepeerdns
persist
maxfail 0
holdoff 10
lcp-echo-interval 30
lcp-echo-failure 10
nodefaultroute
mtu 1200
mru 1200
EOF

    [ -f "$secrets_file" ] || touch "$secrets_file"
    sed -i "/^${username}[ 	]/d" "$secrets_file" 2>/dev/null || true
    echo "${username} * \"${password}\" *" >> "$secrets_file"

    chmod 600 "$ppp_file"
    chmod 600 "$secrets_file"

    log_info "Generated PPP config for $client"
}

start() {
    local client="$1"
    local name=$(get_config "$client" "name" "$client")
    local lac_name="lac_${client}"
    local client_dir="$L2TP_RUN_DIR/$client"

    log_info "Starting L2TP client: $name"

    # 确保系统 xl2tpd 已禁用
    ensure_system_xl2tpd_disabled

    # 防重复：如果 PPP 接口已连接，直接跳过
    local ppp_iface="ppp-${client}"
    if ip link show "$ppp_iface" >/dev/null 2>&1; then
        local existing_ip=$(ip -4 addr show "$ppp_iface" 2>/dev/null | grep -o 'inet [0-9.]*' | cut -d' ' -f2)
        if [ -n "$existing_ip" ]; then
            log_info "Client $name already connected (IP: $existing_ip), skipping"
            return 0
        fi
    fi

    # 防重复：如果 xl2tpd 进程已在运行，直接跳过
    local pid_file="$client_dir/xl2tpd.pid"
    if [ -f "$pid_file" ]; then
        local old_pid=$(cat "$pid_file")
        if [ -n "$old_pid" ] && kill -0 "$old_pid" 2>/dev/null; then
            log_info "xl2tpd already running for $client (PID: $old_pid), skipping"
            return 0
        fi
        # 进程已死，清理残留 pid 文件
        rm -f "$pid_file"
    fi

    # 启动前清理：如果存在残留的 PPP 接口（无对应 xl2tpd 进程），先删除
    if ip link show "$ppp_iface" >/dev/null 2>&1; then
        log_info "Cleaning stale PPP interface: $ppp_iface"
        ip link delete "$ppp_iface" 2>/dev/null || true
    fi

    # 清理残留的 pppd 进程（匹配 ifname）
    local stale_pppd=$(ps w 2>/dev/null | grep "pppd" | grep "ifname $ppp_iface" | grep -v grep | awk '{print $1}')
    if [ -n "$stale_pppd" ]; then
        log_info "Killing stale pppd for $ppp_iface: $stale_pppd"
        echo "$stale_pppd" | xargs kill 2>/dev/null || true
        sleep 1
        echo "$stale_pppd" | xargs kill -9 2>/dev/null || true
    fi

    # 生成配置
    generate_xl2tpd_config "$client" || return 1
    generate_ppp_config "$client" || return 1

    local config_file="$client_dir/xl2tpd.conf"
    local control_file="$client_dir/control"

    # 清理旧的控制文件
    rm -f "$control_file"

    # 启动独立的 xl2tpd 实例（后台守护模式）
    xl2tpd -c "$config_file" -C "$control_file" -p "$pid_file"

    # 等待控制文件创建
    local wait_count=0
    while [ ! -e "$control_file" ] && [ $wait_count -lt 10 ]; do
        sleep 1
        wait_count=$((wait_count + 1))
    done

    if [ ! -e "$control_file" ]; then
        log_error "Control file not created for $client"
        # 尝试杀掉进程
        [ -f "$pid_file" ] && kill "$(cat "$pid_file")" 2>/dev/null
        return 1
    fi

    sleep 1

    # 发送连接命令
    echo "c ${lac_name}" > "$control_file"
    log_info "Sent connect command: c ${lac_name}"

    # 记录客户端状态
    mkdir -p "$RUN_DIR/clients"
    local config_hash=$(uci show "proxypool.$client" | md5sum | cut -d' ' -f1)
    echo "$config_hash" > "$RUN_DIR/clients/$client"

    local running_pid=""
    [ -f "$pid_file" ] && running_pid=$(cat "$pid_file")
    log_info "L2TP client started: $name (PID: $running_pid)"
}

stop() {
    local client="$1"
    local name=$(get_config "$client" "name" "$client")
    local lac_name="lac_${client}"
    local client_dir="$L2TP_RUN_DIR/$client"

    log_info "Stopping L2TP client: $name"

    # 发送断开命令
    local control_file="$client_dir/control"
    if [ -e "$control_file" ]; then
        echo "d ${lac_name}" > "$control_file" 2>/dev/null || true
        log_info "Sent disconnect command: d ${lac_name}"
        sleep 2
    fi

    # 停止该客户端的 xl2tpd 进程
    local pid_file="$client_dir/xl2tpd.pid"
    if [ -f "$pid_file" ]; then
        local pid=$(cat "$pid_file")
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
            sleep 1
            kill -9 "$pid" 2>/dev/null || true
        fi
        rm -f "$pid_file"
    fi

    # 杀死该客户端关联的 pppd 进程（xl2tpd 死后 pppd 可能残留）
    local ppp_iface="ppp-${client}"
    local pppd_pids=$(ps w 2>/dev/null | grep "pppd" | grep "ifname $ppp_iface" | grep -v grep | awk '{print $1}')
    if [ -n "$pppd_pids" ]; then
        log_info "Killing pppd processes for $ppp_iface: $pppd_pids"
        echo "$pppd_pids" | xargs kill 2>/dev/null || true
        sleep 1
        echo "$pppd_pids" | xargs kill -9 2>/dev/null || true
    fi

    # 强制删除 PPP 接口（确保内核层面完全清理）
    if ip link show "$ppp_iface" >/dev/null 2>&1; then
        ip link delete "$ppp_iface" 2>/dev/null || true
        log_info "Deleted PPP interface: $ppp_iface"
    fi

    # 注意：不再清理内核 L2TP 隧道
    # 原代码会删除所有隧道（影响其他客户端），且内核会在进程退出时自动清理
    # 如果出现残留隧道问题，可通过 /etc/init.d/proxypool restart 全局重启解决

    # 清理该客户端的运行文件（不影响其他客户端）
    rm -rf "$client_dir"
    rm -f "${PPP_DIR}/proxypool-${client}"
    rm -f "$RUN_DIR/clients/$client"

    # 清理 chap-secrets 中对应条目
    local username=$(get_config "$client" "username" "" | tr -d ' \t\n\r')
    if [ -n "$username" ]; then
        sed -i "/^${username}[ 	]/d" /etc/ppp/chap-secrets 2>/dev/null || true
    fi

    log_info "L2TP client stopped: $name"
}

status() {
    local client="$1"
    local ppp_iface="ppp-${client}"

    if ip link show "$ppp_iface" >/dev/null 2>&1; then
        local ip=$(ip -4 addr show "$ppp_iface" 2>/dev/null | grep -o 'inet [0-9.]*' | cut -d' ' -f2)
        if [ -n "$ip" ]; then
            # PPP 接口存活且有 IP，读探测缓存判断实际连通性
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
            echo "$ip"
            return 0
        fi
    fi

    local pid_file="$L2TP_RUN_DIR/$client/xl2tpd.pid"
    if [ -f "$pid_file" ]; then
        local pid=$(cat "$pid_file")
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            echo "connecting"
            return 0
        fi
    fi

    # PID 不存在或已死，清理过期探测缓存
    rm -f "$PROBE_DIR/${client}" 2>/dev/null
    echo "disconnected"
}

# 探测 L2TP 隧道实际连通性并写入缓存
probe() {
    local client="$1"
    mkdir -p "$PROBE_DIR"
    local result=$(test_connection "$client")
    echo "$result" > "$PROBE_DIR/${client}"
}

# 通过 PPP 接口实际发 HTTP 请求（准确判断隧道是否可上网）
test_connection() {
    local client="$1"
    local ppp_iface="ppp-${client}"

    # PPP 接口不存在或无 IP，直接判定失败
    if ! ip link show "$ppp_iface" >/dev/null 2>&1; then
        echo "fail"
        return
    fi
    local ppp_ip=$(ip -4 addr show "$ppp_iface" 2>/dev/null | grep -o 'inet [0-9.]*' | cut -d' ' -f2)
    if [ -z "$ppp_ip" ]; then
        echo "fail"
        return
    fi

    # 通过 PPP 接口发 HTTP 请求验证实际连通性
    if command -v curl >/dev/null 2>&1; then
        local http_code
        http_code=$(curl --interface "$ppp_iface" \
            --connect-timeout 3 --max-time 8 \
            -s -o /dev/null -w "%{http_code}" \
            "http://www.baidu.com" 2>/dev/null)
        if [ "$http_code" -ge 200 ] 2>/dev/null && [ "$http_code" -lt 400 ] 2>/dev/null; then
            echo "ok"
        else
            echo "fail"
        fi
    else
        # curl 不可用，回退到 ping（通过 PPP 接口）
        if ping -I "$ppp_iface" -c 1 -W 3 8.8.8.8 >/dev/null 2>&1; then
            echo "ok"
        else
            echo "fail"
        fi
    fi
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
    probe)
        probe "$2"
        ;;
    *)
        echo "Usage: $0 {start|stop|status|test|probe} <client_id>"
        exit 1
        ;;
esac
