# ProxyPool L2TP 恢复闭环设计

日期：2026-08-19

状态：用户已确认
目标平台：OpenWrt 23.05.3 / GL-MT6000

## 目标

在不放宽既有 fail-closed、安全隔离和管理白名单的前提下，解决以下已经确认的运行缺口：

- 在线节点缺少周期性真实健康验证，丢失 netifd hotplug 后可能不能自动恢复；
- 批量任务把可重试的 `backoff` 永久视为未完成，坏节点会让任务长期运行；
- OpenWrt 23.05 的 L2TP teardown 和 `xl2tpd-control` 调用没有完整的时间上限；
- WAN down/up 没有生产态监督器，状态机中的 WAN 恢复事件不能可靠触发。

## 安全不变量

- 普通 LAN/Wi-Fi 设备始终不能访问其他局域网设备。
- 只允许客户端访问 DHCP、路由器 DNS 和 TCP 80/443 管理页面。
- 客户端外部 UDP 和 IPv6 始终禁止。
- 已绑定设备只能经当前节点的精确出口接口访问公网 TCP；节点异常时不得回落本地 WAN。
- 撤销设备授权必须先于路由、DNS、PPP 或 LAC 清理。
- 所有等待、清理、重试和批量任务都必须有界。

## 1. L2TP 有界生命周期

完整固件覆盖固定版本的 `/lib/netifd/proto/l2tp.sh`，保持 OpenWrt netifd + 单共享 xl2tpd 架构，但收紧以下边界：

- `xl2tpd-control` 的 add/connect/disconnect/remove 都带硬超时；
- setup 前只清理同名、可证明归属的 LAC 和 PPP 进程；
- teardown 等待 PPP 接口消失有固定上限，先 TERM 后 KILL，只匹配当前接口的 options 文件；
- 清理失败明确返回错误，不允许无限循环占住 netifd worker；
- 不修改全局 chap-secrets，不启动每节点 xl2tpd，不模糊杀死其他 PPP 会话。

## 2. 周期健康监督与恢复

Scheduler 增加单一健康监督循环。它只观察当前进程拥有的在线 session，并按固定节拍执行：

1. 校验节点 generation 和 session 所有权；
2. 校验 netifd 接口、PPP IPv4 和共享 xl2tpd；
3. 校验策略规则和精确 PPP 默认路由；
4. 通过当前节点绑定的 DoH 通道进行小型真实数据面探测；
5. 探测失败后先关闭授权 gate，再清理 DNS、路由和 L2TP session；
6. 节点进入有界退避，由后台恢复作业重新连接。

健康检查必须限并发，不能为 60 个节点同时创建大量外部请求。hotplug 只作为快速唤醒提示；即使事件丢失，周期监督仍能收敛。

共享 xl2tpd 重启通过进程身份变化或全体 L2TP session 探测失败被识别。受影响节点先全部撤权，再按 L2TP 并发上限（默认 4）恢复。

## 3. 任务终态与后台重试分离

一次用户操作的 job 与节点长期 desired state 分开：

- 导入、保存、绑定、重连等 job 只等待本轮有界连接尝试；
- 本轮成功记为 job succeeded；本轮失败记为 job failed，并保留明确错误；
- 节点 runtime 可以同时处于 `backoff`，后续由独立 `system.recover` 作业继续恢复；
- 原 job 一旦终态不可再被后台恢复改写；
- 40～60 节点中一个失败不阻塞其他节点，本轮任务必须在每节点 deadline 之和受并发上限约束的总上限内结束。

## 4. WAN 监督

新增只读 WAN 状态源，读取 `network.interface.wan status` 的权威状态：

- WAN down 时暂停新的 L2TP连接尝试，当前设备授权仍按节点健康结果 fail closed；
- WAN up 边沿唤醒等待 WAN 的节点并创建独立恢复作业；
- 轮询作为权威兜底，hotplug 只降低恢复延迟；
- 状态不可读按 WAN 不可用处理，不猜测为在线。

## 5. 测试与发布门禁

整包编译前必须依次通过：

1. patched `l2tp.sh` 的真实 shell 夹具：控制管道卡死、PPP 接口残留、TERM/KILL、teardown deadline；
2. Scheduler Go 测试：漏 hotplug、PPP 消失、DoH 失败、共享 xl2tpd 重启、撤权顺序；
3. Job 测试：60 个失败节点的批量任务有界终止，后台恢复使用新 job；
4. WAN down/up 测试：暂停、恢复、重启后重新装载；
5. 完整 host、race、vet、包构建和 payload 检查。

固件完成后真机按 1、5、20、40～60 节点逐级测试。任何阶段失败都保留证据并停止扩大规模，不用手工重连掩盖自动恢复问题。

## 当前协议范围

本轮正式运行范围仍为 L2TP。SOCKS5/SLP 只保留配置迁移能力，不把它们伪装为可用协议。L2TP 单节点和恢复闭环通过真机验收后，再接入 SOCKS5 TCP CONNECT。
