# 全局菜单安装指南

## 功能说明

将智联盒子的快捷导航和统计信息显示在 LuCI 全局顶部菜单，方便在任何页面快速访问。

### 效果预览

```
┌────────────────────────────────────────────────────────────────┐
│ [智联盒子] [信道分析] [备份与升级] [无线] [重启]  总 5  已连接 3  未连接 2 │
└────────────────────────────────────────────────────────────────┘
│ 状态 | 系统 | 服务 | 网络 | 注销          （LuCI 原有菜单）      │
└────────────────────────────────────────────────────────────────┘
```

## 安装步骤

### 1. 上传脚本到 OpenWrt

将以下文件上传到路由器的 `/tmp/` 目录：

```bash
proxypool-core/files/install-global-menu.sh
```

### 2. 执行安装脚本

```bash
# SSH 登录到 OpenWrt
ssh root@192.168.1.1

# 添加执行权限
chmod +x /tmp/install-global-menu.sh

# 执行安装
/tmp/install-global-menu.sh
```

### 3. 重启 uhttpd 服务

```bash
/etc/init.d/uhttpd restart
```

### 4. 刷新浏览器

清空浏览器缓存或强制刷新（Ctrl+F5），查看全局菜单是否显示。

## 验证

安装成功后，在任何 LuCI 页面顶部都能看到：

- **快捷导航**：智联盒子、信道分析、备份与升级、无线、重启
- **统计信息**：总数、已连接、未连接（仅在智联盒子页面更新）

## 卸载

如需恢复原始 LuCI 界面：

```bash
# 上传卸载脚本
chmod +x /tmp/uninstall-global-menu.sh

# 执行卸载
/tmp/uninstall-global-menu.sh
```

卸载后会：
- 恢复原始 `header.htm`
- 自动重启 uhttpd 服务
- 保留备份文件（`.proxypool-backup`）

## 手动恢复

如果卸载脚本失败，可以手动恢复：

```bash
cp /usr/lib/lua/luci/view/header.htm.proxypool-backup \
   /usr/lib/lua/luci/view/header.htm

/etc/init.d/uhttpd restart
```

## 注意事项

1. **备份重要**：脚本会自动备份原始 `header.htm`，请勿删除备份文件
2. **升级影响**：升级 LuCI 后可能覆盖修改，需重新执行安装脚本
3. **兼容性**：适用于标准 LuCI 界面，自定义主题可能不兼容
4. **统计更新**：统计数字仅在智联盒子页面自动刷新（10秒一次）

## 故障排除

### 菜单未显示

1. 检查 `header.htm` 是否被修改成功：
```bash
grep "proxypool-global-menu" /usr/lib/lua/luci/view/header.htm
```

2. 清空浏览器缓存并强制刷新（Ctrl+Shift+R）

3. 检查 uhttpd 服务状态：
```bash
/etc/init.d/uhttpd status
```

### 统计不更新

1. 确认智联盒子页面可以正常访问
2. 按 F12 打开浏览器控制台，查看是否有 JS 错误
3. 检查 API 请求是否正常：
```bash
curl http://localhost/cgi-bin/luci/admin/services/proxypool?action=status
```

## 高级定制

如需修改菜单样式或链接，编辑安装脚本中的 HTML/CSS 部分：

```bash
vi /tmp/install-global-menu.sh
```

修改后重新执行安装即可。
