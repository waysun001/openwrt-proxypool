# IP归属地 - ip2region v2.0（轻量级、准确、开源）

## 工作原理

使用 **ip2region v2.0** 开源IP数据库，完全离线查询，**无需网络**。

### 关于 ip2region

- **项目地址**: https://github.com/lionsoul2014/ip2region
- **数据库大小**: ~11MB（ip2region.xdb）
- **数据格式**: 国家|区域|省份|城市|ISP
- **查询速度**: <0.5ms（memory算法）
- **维护状态**: 活跃更新中（2024年持续维护）

### 查询策略（二层回退）

1. **ip2region 查询（优先）**
   - 使用 ip2region.xdb 数据库
   - 准确度高，覆盖全球IP
   - 完全离线

2. **简单IP段匹配（回退）**
   - 基于第一字节的粗略匹配
   - 仅识别中国/美国/其他
   - 当数据库未安装时启用

## 安装

### 方法1：自动安装（推荐）

运行安装脚本，自动下载数据库和查询工具：

```bash
/usr/lib/proxypool/update-ipdb.sh
```

脚本会：
1. 下载 ip2region.xdb 数据库（~11MB）
2. 下载并编译查询工具（如果系统有gcc）
3. 自动测试查询功能

### 方法2：手动安装

```bash
cd /tmp
wget https://github.com/lionsoul2014/ip2region/raw/master/data/ip2region.xdb
mv ip2region.xdb /usr/lib/proxypool/
```

## 验证

部署后查看客户端列表，服务器IP后会显示归属地：

```
103.57.27.124:1080 (中国-广东-深圳)
8.8.8.8:1080 (美国)
114.114.114.114:1080 (中国-江苏-南京)
```

## 更新数据库

定期运行安装脚本更新到最新数据：

```bash
/usr/lib/proxypool/update-ipdb.sh
```

## 性能

- 查询速度：<0.5ms/IP（带缓存 <0.1ms）
- 内存占用：~100KB（查询工具常驻）
- 数据库大小：~11MB
- 缓存机制：5分钟缓存，避免重复查询

## 优势

- ✅ **轻量级**：11MB vs 纯真IP库 50MB+
- ✅ **准确度高**：覆盖全球400万+ IP段
- ✅ **完全离线**：无需网络，不依赖第三方API
- ✅ **隐私安全**：IP信息不外泄
- ✅ **开源免费**：Apache 2.0 协议
- ✅ **持续维护**：活跃的开源社区

## 故障排除

### 如果归属地显示"其他"或"未知"

1. 检查数据库文件是否存在：
```bash
ls -lh /usr/lib/proxypool/ip2region.xdb
```

2. 如果不存在，运行安装脚本：
```bash
/usr/lib/proxypool/update-ipdb.sh
```

3. 手动测试查询：
```bash
/usr/lib/proxypool/iplocation.sh 8.8.8.8
```

### 如果查询速度慢

ip2region 已经非常快了（<0.5ms），如果仍觉得慢可能是：
- 缓存未生效（检查 `/var/run/proxypool/location_cache/`）
- 数据库文件损坏（重新运行安装脚本）
