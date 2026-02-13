#!/bin/sh
# IP归属地查询（二级回退）
# 1. ip2region 本地数据库（毫秒级，离线可用）
# 2. IP段粗略匹配（兜底）

IP="$1"
[ -z "$IP" ] && exit 1

# ===== 方法1：ip2region 本地数据库 =====
XDB_FILE="/usr/lib/proxypool/ip2region.xdb"
SEARCHER="/usr/lib/proxypool/ip2region_searcher"

if [ -x "$SEARCHER" ] && [ -f "$XDB_FILE" ]; then
    RESULT=$("$SEARCHER" search "$XDB_FILE" "$IP" 2>/dev/null)
    if [ -n "$RESULT" ]; then
        echo "$RESULT" | awk -F'|' '{
            country = $1; province = $3; city = $4
            if (country == "中国") {
                if (province != "" && province != "0") {
                    if (city != "" && city != "0") print country "-" province "-" city
                    else print country "-" province
                } else print country
            } else if (country != "" && country != "0") print country
            else print "未知"
        }'
        exit 0
    fi
fi

# ===== 方法2：IP段粗略匹配（兜底） =====
FIRST_OCTET=$(echo "$IP" | cut -d'.' -f1)
case "$FIRST_OCTET" in
    1|2|3|27|36|42|49|58|59|60|61|101|106|110|111|112|113|114|115|116|117|118|119|120|121|122|123|124|125|180|182|183|202|203|210|211|218|219|220|221|222|223)
        echo "中国"
        ;;
    *)
        echo "海外"
        ;;
esac
