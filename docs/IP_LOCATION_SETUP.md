# IP归属地配置说明

## 方法一：使用 GeoIP（推荐）

### 安装
```bash
opkg update
opkg install geoip-database geoip
```

### 验证
```bash
geoiplookup 8.8.8.8
# 输出: GeoIP Country Edition: US, United States
```

## 方法二：使用纯真IP库（国内IP更准确）

### 1. 下载纯真IP库
```bash
cd /usr/share
wget https://github.com/out0fmemory/qqwry.dat/raw/master/qqwry.dat
```

### 2. 安装Python脚本（可选，需要Python环境）
```bash
opkg install python3-pip
pip3 install qqwry-py3
```

### 3. 修改 status.sh（使用自定义查询脚本）

在 `status.sh` 的归属地查询部分，替换为：

```bash
local location=""
if [ -n "$server" ]; then
    # 使用自定义IP查询脚本
    location=$(/usr/lib/proxypool/iplocation.sh "$server" 2>/dev/null)
fi
```

### 4. 创建查询脚本 `/usr/lib/proxypool/iplocation.sh`
```bash
#!/bin/sh
# 简易IP归属地查询（需要根据实际IP库格式调整）
IP="$1"
# 这里实现你的查询逻辑，输出格式：中国-广东-深圳
echo "中国"  # 示例输出
```

## 方法三：使用在线API（需要网络）

修改 `status.sh`：
```bash
local location=""
if [ -n "$server" ]; then
    location=$(curl -s "http://ip-api.com/line/$server?fields=country,city" 2>/dev/null | tr '\n' '-')
fi
```

## 推荐方案

**OpenWrt推荐**：方法一（GeoIP），轻量且稳定  
**国内IP准确性要求高**：方法二（纯真IP库）+ 自定义脚本

## 注意事项

- IP归属地查询会增加状态刷新时间（约100-300ms/客户端）
- 建议缓存查询结果，避免每次刷新都查询
- 如果不需要归属地，保持默认即可（自动跳过）
