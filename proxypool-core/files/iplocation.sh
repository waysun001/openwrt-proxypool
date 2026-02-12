#!/bin/sh
# 轻量级IP归属地查询 - 使用 ipip.net API（离线备用）

IP="$1"
[ -z "$IP" ] && exit 1

# 方法1：使用 ipip.net 免费API（无需认证，稳定）
if command -v wget >/dev/null 2>&1; then
    RESULT=$(wget -qO- --timeout=2 "http://freeapi.ipip.net/$IP" 2>/dev/null)
    if [ -n "$RESULT" ]; then
        # 解析JSON: ["中国","广东","深圳","电信"]
        echo "$RESULT" | sed 's/\[//g;s/\]//g;s/"//g' | awk -F',' '{print $1"-"$2"-"$3}'
        exit 0
    fi
fi

# 方法2：使用 curl（备用）
if command -v curl >/dev/null 2>&1; then
    RESULT=$(curl -s --max-time 2 "http://freeapi.ipip.net/$IP" 2>/dev/null)
    if [ -n "$RESULT" ]; then
        echo "$RESULT" | sed 's/\[//g;s/\]//g;s/"//g' | awk -F',' '{print $1"-"$2"-"$3}'
        exit 0
    fi
fi

# 方法3：简单的IP段匹配（离线，仅识别主要国家/地区）
FIRST_OCTET=$(echo "$IP" | cut -d'.' -f1)

case "$FIRST_OCTET" in
    1|2|3|27|58|59|60|61|101|106|110|111|112|113|114|115|116|117|118|119|120|121|122|123|124|125|183|202|203|210|211|218|219|220|221|222)
        echo "中国"
        ;;
    8|11)
        echo "美国"
        ;;
    *)
        echo "海外"
        ;;
esac
