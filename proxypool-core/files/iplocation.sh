#!/bin/sh
# 本地IP归属地查询 - 使用 ip2region v2.0（轻量级、准确、开源）

IP="$1"
[ -z "$IP" ] && exit 1

# ip2region 数据库和查询工具路径
XDB_FILE="/usr/lib/proxypool/ip2region.xdb"
SEARCHER="/usr/lib/proxypool/ip2region_searcher"

# 方法1：使用 ip2region 查询工具（推荐）
if [ -f "$SEARCHER" ] && [ -f "$XDB_FILE" ]; then
    RESULT=$("$SEARCHER" search "$XDB_FILE" "$IP" 2>/dev/null)
    if [ -n "$RESULT" ]; then
        # ip2region 输出格式: 国家|区域|省份|城市|ISP
        # 提取前3段作为归属地
        echo "$RESULT" | awk -F'|' '{
            country = $1
            province = $3
            city = $4
            
            # 构建归属地字符串
            if (country == "中国") {
                if (province != "" && province != "0") {
                    if (city != "" && city != "0") {
                        print country "-" province "-" city
                    } else {
                        print country "-" province
                    }
                } else {
                    print country
                }
            } else if (country != "" && country != "0") {
                print country
            } else {
                print "未知"
            }
        }'
        exit 0
    fi
fi

# 方法2：简单IP段匹配（回退方案）
FIRST_OCTET=$(echo "$IP" | cut -d'.' -f1)
case "$FIRST_OCTET" in
    1|2|3|27|58|59|60|61|101|106|110|111|112|113|114|115|116|117|118|119|120|121|122|123|124|125|183|202|203|210|211|218|219|220|221|222)
        echo "中国"
        ;;
    8|11|12|13|15|16|17|18|19|20|21|22|23|24|25|26|28|29|30|32|33|34|35|36|37|38|39|40|41|42|43|44|45|46|47|48|49|50|51|52|53|54|55|56|57|63|64|65|66|67|68|69|70|71|72|73|74|75|76|96|97|98|99|100|104|107|108|128|129|130|131|132|134|135|136|137|138|139|140|142|143|144|145|146|147|148|149|150|151|152|153|154|155|156|157|158|159|160|161|162|163|164|165|166|167|168|169|170|171|172|173|174|175|176|184|192|198|199|204|205|206|207|208|209)
        echo "美国"
        ;;
    *)
        echo "其他"
        ;;
esac
