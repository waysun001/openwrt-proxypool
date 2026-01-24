# OpenWrt ProxyPool

GL-MT6000 定制固件 - 多代理客户端池管理系统

## 功能特性

- **60个代理客户端** - 支持 L2TP 和 SOCKS5 协议
- **内网设备绑定** - 指定IP通过指定客户端上网
- **严格网络隔离** - 未绑定或客户端离线时无法上网
- **LuCI管理界面** - 简易配置，状态监控
- **备份/升级** - 配置导入导出，平滑升级

## 项目结构

```
openwrt-proxypool/
├── .github/workflows/        # GitHub Actions 云编译
├── luci-app-proxypool/       # LuCI 管理界面
├── proxypool-core/           # 核心服务
├── config/                   # OpenWrt 编译配置
└── scripts/                  # 辅助脚本
```

## 快速开始

### 云编译（推荐）

1. Fork 本仓库到你的 GitHub
2. 进入 Actions 页面
3. 运行 "Build OpenWrt" 工作流
4. 下载编译好的固件

### 刷机

1. 进入 GL-MT6000 管理界面
2. 系统 -> 升级 -> 上传固件
3. 等待重启完成

## 配置说明

### 添加代理客户端

管理界面: 服务 -> 代理池 -> 客户端管理

### 绑定内网设备

管理界面: 服务 -> 代理池 -> 设备绑定

## 技术架构

- **L2TP**: xl2tpd + ppp
- **SOCKS5**: redsocks + tproxy
- **防火墙**: nftables
- **策略路由**: iproute2

## License

MIT License