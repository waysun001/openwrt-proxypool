# IP归属地 - 已内置，开箱即用

## 工作原理

项目已内置轻量级IP归属地查询脚本（`/usr/lib/proxypool/iplocation.sh`），**无需额外配置**。

### 查询策略（三层回退）

1. **在线API（优先）**  
   - 使用 ipip.net 免费API
   - 2秒超时，失败自动回退
   - 准确度高，支持国内外IP

2. **离线匹配（备用）**  
   - 基于IP段的简单匹配
   - 识别中国/美国/海外
   - 完全离线，无需网络

3. **容错机制**  
   - API失败时显示离线结果
   - 查询超时不影响状态刷新

## 验证

部署后查看客户端列表，服务器IP后会显示归属地：

```
103.57.27.124:1080 (中国-广东-深圳)
8.8.8.8:1080 (美国)
```

## 高级配置（可选）

### 使用纯真IP库（国内IP更准确）

1. 下载数据库：
```bash
cd /usr/share
wget https://github.com/out0fmemory/qqwry.dat/raw/master/qqwry.dat
```

2. 修改 `/usr/lib/proxypool/iplocation.sh`，添加纯真IP查询逻辑

### 禁用在线查询（纯离线）

编辑 `iplocation.sh`，注释掉 wget/curl 部分，只保留离线匹配。

## 性能

- 在线查询：~100-300ms/IP
- 离线匹配：~1ms/IP
- 已优化：只在首次加载时查询，状态刷新时使用缓存
