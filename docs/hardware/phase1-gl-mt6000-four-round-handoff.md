# ProxyPool Phase 1：GL-MT6000 四轮真机交接

目标版本：OpenWrt 23.05.3，GL-MT6000。主动操作预计 2～3 小时，第四轮另做 12～24 小时被动观察。

本轮只验收 fail-closed、安全启动、管理白名单、LAN/Wi-Fi 二层隔离、故障恢复与升级保留。Phase 1 的 DNS 数据面和 V2 节点调度尚未开放，因此“不可以普通域名上网”是预期结果；不要在本轮验收节点浏览、微信、批量导入或 40～60 节点连接成功率。

## 测试准备

- 使用 CI 完整 OpenWrt 源码任务生成的 GL-MT6000 `squashfs-sysupgrade.bin`，不要使用 ImageBuilder 产物。
- 记录固件 SHA-256、Git commit、OpenWrt commit，并保存 CI 的 `firmware-evidence`。
- 准备有线终端 A、B 和 Wi-Fi 终端 C；A、B 分别直连两个不同 LAN 口，中间不得接普通交换机。
- 测试终端关闭蜂窝网络、其他 Wi-Fi、VPN 和手工代理，避免把旁路网络误判为路由器放行。
- 管理地址固定按 `192.168.9.1` 验证，允许的客户端入站只有 DHCP 和 TCP 80/443 管理页面。

每轮开始先保存：

```sh
ubus call system board
cat /etc/openwrt_release
tr '\0' '\n' </sys/firmware/devicetree/base/compatible 2>/dev/null || true
nft list ruleset
bridge -d link show
logread
```

## 第一轮：冷启动与管理白名单

1. 完全断电 30 秒后上电，不使用软件重启代替。
2. A 直连任一 LAN 口并通过 DHCP 获取 `192.168.9.0/24` 地址。
3. A 访问 `http://192.168.9.1/` 和 `https://192.168.9.1/`；至少实际启用的管理端口必须可达。
4. A 尝试访问路由器的 SSH、DNS 和其他 TCP/UDP 端口；除 DHCP 与 80/443 外必须失败。
5. A 尝试公网 IPv4、域名、IPv6 和外部 UDP；全部必须失败，不得从本地 WAN 回退。
6. 检查 `/etc/proxypool/wireless-quarantine`、`/etc/proxypool/firewall-transaction`、服务状态和日志。任何未知/损坏状态都应保持断网并保留恢复证据，不得自动放行。

通过条件：管理页可用；非白名单路由器服务不可达；客户端没有 IPv4、IPv6、UDP 或 DNS 旁路；冷启动没有短暂可观察的 LAN-to-WAN 放行。

## 第二轮：有线与 Wi-Fi 二层隔离

1. A、B 直连两个不同 LAN 口，C 连接正常的受管 Wi-Fi SSID。
2. 三台终端都确认仍能获取 DHCP，并访问路由器管理页。
3. 双向测试 A↔B、A↔C、B↔C：ARP/邻居发现、IPv4 ping、任意 TCP 监听、IPv6 link-local、组播和广播。
4. 同一 SSID 再接一台 Wi-Fi 终端时，验证同 SSID 终端也不能互访。
5. 在路由器保存 `bridge -d link show`、`bridge fdb show`、`nft list table inet proxypool_guard` 和 `nft list table bridge proxypool_guard_bridge`。

通过条件：所有终端组合均不能互访或收到对方广播业务流量；每台终端仍能访问管理页。若物理口间任一 ARP/IPv4/IPv6 测试成功，立即停止，判定 MT7531 硬件隔离门禁失败。

## 第三轮：重载、崩溃与事务恢复

1. 在 A 持续探测管理页、本地 WAN 泄漏和 B/C 可达性的同时，依次执行 firewall reload、ProxyPool guardian restart 和 LAN isolation worker restart。
2. 在测试环境注入已定义的服务进程强杀/中断场景；每次只执行一个场景，保存命令、时间点和完整 `logread`。
3. 故障发生后检查 root-only WAL/quarantine 状态，确认系统保持断网，不需要通过手工“重连节点”才能回到安全态。
4. 对要求重启边界的状态执行一次完整断电重启，再验证恢复证据被正确消费或明确保留。

通过条件：重载和进程退出期间没有 LAN-to-WAN、终端互访或非白名单管理端口窗口；未知激活结果不重复竞争性 reload；需要重启的事务明确 fail closed。

## 第四轮：sysupgrade 保留与稳定性观察

1. 先导出备份并记录 V1/V2 配置、selector、quarantine/WAL 证据的哈希。
2. 使用同一受审固件或后续同合同固件执行保留配置的 sysupgrade。
3. 重复第一、二轮的关键检查，确认升级没有恢复旧 PPP 全局 hook、旧 LuCI 写入口、本地 WAN DNS 或匿名 firewall 修改。
4. 预置 40～60 个节点配置只用于规模与守护策略观察；Phase 1 不启动旧节点，也不以节点“在线”为通过条件。
5. 保持 A/B/C 接入 12～24 小时，记录服务重启次数、内存、日志增长、WAL/quarantine 变化，以及任何瞬时公网或终端互访成功。

通过条件：升级保留合同正确，旧运行时仍被隔离，12～24 小时内无安全泄漏、无限重启或必须人工重连才能恢复安全态的问题。

## 证据回传

每轮请回传：固件 SHA-256、Git commit、接线说明、终端 IP/MAC、开始/结束时间、实际命令、成功/失败结果、`logread`、`nft list ruleset`、`bridge -d link show`。失败时不要继续下一轮，也不要手工修改规则掩盖现场；保留 `/etc/proxypool` 和 `/tmp` 中相关状态后再交给开发分析。
