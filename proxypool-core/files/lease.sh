#!/bin/sh
# 智联盒子 - 使用期限累计（抗系统时间篡改）
# 核心：只用内核单调时钟 /proc/uptime 的增量累计"已用运行秒数"，
#       从不依赖墙上时钟，因此改/回拨系统时间无效。
# 持久值存 UCI proxypool.global.lease_used（只增不减），
# 实时值放 tmpfs 暂存，每满 FLUSH_THRESHOLD 秒才落盘一次以减少 flash 写入。
# 兼容 busybox ash。

RUN_DIR="/var/run/proxypool"
LEASE_DIR="$RUN_DIR/lease"
USED_FILE="$LEASE_DIR/used"            # 实时累计秒数（tmpfs）
UPTIME_FILE="$LEASE_DIR/last_uptime"   # 上次累计时的 uptime 取样
COMMITTED_FILE="$LEASE_DIR/committed"  # 上次落盘时的 used 值

MAX_DELTA=86400      # 单次累计上限，防止异常大跳变
FLUSH_THRESHOLD=3600 # 累计满 1 小时才写一次 UCI（降低 flash 写入）

uci_get() { uci -q get "proxypool.global.$1"; }

cur_uptime() {
    # /proc/uptime 形如 "12345.67 8901.23"，取第一个整数部分
    local v
    v=$(cut -d' ' -f1 /proc/uptime 2>/dev/null | cut -d. -f1)
    [ -n "$v" ] && echo "$v" || echo 0
}

committed_used() {
    local v
    v=$(uci_get lease_used)
    case "$v" in
        ''|*[!0-9]*) echo 0 ;;
        *) echo "$v" ;;
    esac
}

read_num() {
    # 读文件中的整数，无效则 0
    local v
    v=$(cat "$1" 2>/dev/null)
    case "$v" in
        ''|*[!0-9]*) echo 0 ;;
        *) echo "$v" ;;
    esac
}

init_scratch() {
    mkdir -p "$LEASE_DIR" 2>/dev/null
    if [ ! -f "$USED_FILE" ]; then
        # 新开机/暂存丢失：从已落盘的累计值恢复，并从本次开机起继续累计
        committed_used > "$USED_FILE"
        echo 0 > "$UPTIME_FILE"
        cat "$USED_FILE" > "$COMMITTED_FILE"
    fi
}

accrue() {
    init_scratch
    local U last used delta committed
    U=$(cur_uptime)
    last=$(read_num "$UPTIME_FILE")
    used=$(read_num "$USED_FILE")

    if [ "$U" -ge "$last" ]; then
        delta=$((U - last))
    else
        delta=$U   # uptime 回退视为重启，从开机累计
    fi
    [ "$delta" -gt "$MAX_DELTA" ] && delta=$MAX_DELTA

    used=$((used + delta))
    echo "$used" > "$USED_FILE"
    echo "$U" > "$UPTIME_FILE"

    committed=$(read_num "$COMMITTED_FILE")
    if [ $((used - committed)) -ge "$FLUSH_THRESHOLD" ]; then
        uci set "proxypool.global.lease_used=$used"
        uci commit proxypool
        echo "$used" > "$COMMITTED_FILE"
    fi
}

flush() {
    [ -f "$USED_FILE" ] || return 0
    local used
    used=$(read_num "$USED_FILE")
    uci set "proxypool.global.lease_used=$used"
    uci commit proxypool
    echo "$used" > "$COMMITTED_FILE"
}

reset() {
    # 续期：累计清零
    rm -rf "$LEASE_DIR" 2>/dev/null
    uci set "proxypool.global.lease_used=0"
    uci commit proxypool
    init_scratch
}

used_now() {
    if [ -f "$USED_FILE" ]; then
        read_num "$USED_FILE"
    else
        committed_used
    fi
}

case "$1" in
    accrue) accrue ;;
    boot)   init_scratch; accrue ;;
    flush)  flush ;;
    reset)  reset ;;
    used)   used_now ;;
    *) echo "usage: $0 {accrue|boot|flush|reset|used}" ;;
esac
