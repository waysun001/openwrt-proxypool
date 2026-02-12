#!/bin/sh
# 超时计数日轮转 - 建议在 crontab 中 00:00 执行
# 0 0 * * * /usr/lib/proxypool/timeout-rotate.sh

TIMEOUT_DIR="/var/run/proxypool/timeout"

[ -d "$TIMEOUT_DIR" ] || exit 0

# today → yesterday，清空 today
for f in "$TIMEOUT_DIR"/*.today; do
    [ -f "$f" ] || continue
    client=$(basename "$f" .today)
    cp "$f" "$TIMEOUT_DIR/${client}.yesterday"
    echo 0 > "$f"
done
