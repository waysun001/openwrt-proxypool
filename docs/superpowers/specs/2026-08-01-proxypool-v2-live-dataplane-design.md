# ProxyPool V2 正式数据面开放设计

日期：2026-08-01
状态：用户已确认
目标平台：OpenWrt 23.05.3，GL-MT6000（mediatek/filogic）

## 1. 决策与范围

当前均为测试环境，不再故意保持 DNS 数据面不可用，也不再把所有节点连接长期阻断在 Phase 1 安全门禁后。下一阶段直接实现正式 V2 数据面，并在测试固件中开放真实 L2TP、SOCKS5 和 SLP 节点。

该决策只取消“功能故意关闭”，不放宽以下安全不变量：

- 已绑定设备只能经过其指定节点访问外部网络，任何状态下都不能回退到本地主 WAN。
- 节点未就绪、DNS 未就绪、daemon/协议进程崩溃、配置不一致、路由或防火墙重载时保持 fail closed。
- 设备之间始终禁止互访；允许设备访问路由器 `192.168.9.1` 的 TCP 80/443 管理页面和必要 DHCP/DNS 服务。
- 客户端外部 UDP 与 IPv6 上网继续禁止。DHCP 和到路由器自有 DNS 的 TCP/UDP 53 是本地服务例外。
- L2TP UDP 1701 和 SLP QUIC 是路由器建立节点隧道所需的控制/承载流量，只允许路由器进程访问经过验证的精确节点目的地址和端口。
- 旧 V1 运行链路不重新启用。正式功能只能由 V2 单写者和有所有权证据的协议适配器发布。

## 2. 实现策略

采用正式 V2 纵向切片，而不是恢复 V1 或一次性大爆炸式接入全部功能：

1. 先完成一个设备绑定一个真实节点的配置、DNS、协议连接、健康验证和动态授权闭环。
2. 将闭环分别接入 L2TP、SOCKS5 和 SLP 适配器。
3. 最后将同一闭环扩展到 40～60 节点持久批量任务、限并发调度和自动恢复。

这样每一阶段都能在真实测试节点上独立判断连接问题、DNS 问题、授权问题或调度问题，避免多种故障互相掩盖。

## 3. 单写者与组件边界

`proxypoold` 是正式 V2 运行态唯一写入者，负责：

- 加载并验证 V2 期望配置；
- 持久设备、节点、绑定和批量任务；
- 驱动协议适配器并维护节点状态机；
- 创建和撤销节点路由、DNS 通道及 nft 动态授权；
- 对账实际进程、接口、路由和防火墙状态；
- 通过 root-only Unix socket 提供结构化状态和命令接口。

LuCI、DHCP/ubus 事件、netifd/PPP 事件和 procd 回调只提交经过版本化验证的命令或事件，不直接启动进程、修改 UCI、路由或 nftables。旧 Shell manager 继续由 legacy gate 拒绝写操作。

正式组件边界如下：

- 配置存储：原子保存 V2 节点、设备、绑定和全局上限。
- 设备发现：只读取 DHCP/ubus 邻居信息，生成可供用户选择的设备候选。
- 作业存储与 scheduler：持久化批量任务、节点子任务、deadline、退避和取消意图。
- L2TP 适配器：共享 xl2tpd、每节点 netifd 接口、PPP options、generation 和所有权 manifest。
- SOCKS5/SLP 适配器：每节点受管本地转发进程、端口、PID/start-time/config-path 所有权和探测。
- DNS 控制器：每节点独立 DNS 通道与设备到通道的选择映射。
- 授权发布器：只发布经过完整验证的短时 nft 租约，不拥有静态 guardian 策略。

## 4. 节点状态机与授权

节点状态固定为：

```text
disabled -> queued -> starting -> validating -> online
                                  |             |
                                  v             v
                               failed <-> degraded
                                  |
                                  v
                               backoff -> queued

online/degraded/failed -> stopping -> disabled
任意不一致状态 -> recovering -> queued/disabled/failed
```

每个动作携带持久 job ID、节点 generation、开始时间和 deadline。PID 存在、接口存在或 xl2tpd 存活都不能单独代表节点在线。

进入 `online` 必须同时满足：

- 协议进程和配置文件所有权全部匹配；
- 对应接口/本地代理端口属于当前 generation；
- 节点路由和禁止主 WAN 回退的规则验证通过；
- DNS 通道已就绪并通过节点路径验证；
- 健康探测在 deadline 内成功；
- 设备绑定的 MAC 与 IPv4 当前一致。

在线授权采用短时 nft 元素租约，包含设备 MAC、IPv4、节点 generation、协议出口接口或透明转发端口。daemon 周期刷新租约；daemon 被 SIGKILL、节点停止刷新或任何验证失败时租约自动过期。显式故障处理仍先撤授权，再清理进程、接口和路由。

## 5. DNS 数据面

客户端只能访问路由器自有 TCP/UDP 53，不能直接访问外部 DNS。`proxypoold` 内置一个受管 DNS listener，按查询源 IPv4 查找已验证的 `MAC + IPv4 + node ID` 绑定，并为不同节点维护独立上游连接与缓存：

- L2TP：DNS TCP 连接进入该节点策略路由表，从对应 PPP 接口离开。
- SOCKS5：通过对应 SOCKS5 节点建立 TCP DNS 或 DoH 连接。
- SLP：通过对应 SLP 本地代理建立 TCP DNS 或 DoH 连接。

不同节点的 DNS 上游连接和缓存所有权必须独立，禁止把一个设备查询发送到其他节点。guardian 只为当前在线绑定发布到路由器 53 的短时源 tuple；DNS listener、上游连接或路径验证失败时，节点不能进入 `online`，对应设备保持断网。

节点服务器允许使用域名。解析节点域名属于路由器建立隧道的控制流量，使用配置中固定 bootstrap IP 的受限解析器。ProxyPool 控制流按精确 bootstrap/节点目的地址、协议和端口发布到 router-output 集合；不向客户端开放通用 WAN DNS 或本地 WAN 转发，也不把 hostile root 进程隔离作为本项目威胁模型。

## 6. 协议适配器

### 6.1 L2TP

系统只运行一个由 procd 管理、带 respawn 的共享 xl2tpd。每个节点拥有独立 LAC、netifd 接口、PPP options、短接口名、策略路由表和 generation。PPP 回调验证 session token、接口名、节点 ID 和 generation 后只上报事件，不直接扩大授权。

共享 xl2tpd 崩溃时，daemon 立即撤销全部 L2TP 设备授权并递增共享 generation；procd 拉起新进程后，scheduler 按默认并发 4 分批恢复节点。用户不需要手工逐个重连。

### 6.2 SOCKS5

第一阶段只使用 TCP CONNECT 能力，不依赖节点是否支持 UDP ASSOCIATE。每个节点拥有独立透明 TCP 转发端口和严格进程所有权。设备外部 UDP 始终被 guardian 丢弃。

### 6.3 SLP

每个节点拥有独立受管 `slp-client` 实例和本地代理端口。SLP 使用 QUIC 时，只允许路由器实例到该节点精确地址/端口的 UDP 承载；客户端不能直接发送外部 UDP。

## 7. 设备发现与绑定

设备发现器从 DHCP leases、ubus 和邻居状态读取 MAC、当前 IPv4、主机名、接入方式和最后在线时间。用户不手工输入 MAC。

一个节点可以绑定多个设备；一个设备同一时间只能绑定一个节点。绑定后系统创建稳定 DHCP 地址并持久化 `MAC + IPv4 + node ID`。MAC/IP 不一致、随机 MAC 变化、重复地址或租约冲突时立即撤销授权，并把设备重新标为待确认。

LAN 与 Wi-Fi 使用同一设备模型和授权条件，二层隔离策略不因设备已绑定或节点在线而放宽。

## 8. 批量导入与持久 scheduler

浏览器每次批量导入只提交一个请求。daemon 使用同一严格 schema 完成解析、存储、脱敏导出和逐项错误报告，必须完整保留 SLP token/transport/obfs/remark 等字段。

合法节点先通过原子配置事务保存，再创建一个持久批量作业。作业包含每个节点的排队状态、generation、deadline、失败类别、尝试次数和下次重试时间。默认并发：L2TP 4，SOCKS5/SLP 8；全局资源上限可进一步降低实际并发。

单节点失败不阻塞其他节点，也不能获得流量授权。页面刷新、LuCI 进程退出或路由器重启后，作业从持久确认边界恢复。手工重连只更新节点意图；已有 stop/recovery 未完成时合并为一次后续 generation，禁止启动重叠协议进程。

## 9. 错误处理与可观测性

每个节点必须在 deadline 内进入下一明确状态，不允许永久 `connecting`。状态接口返回结构化错误类别：配置、bootstrap/DNS、认证、协议连接、接口、路由、健康检查、所有权冲突、资源上限和超时。

关键状态变更写入有界事件日志，凭据永不写日志。状态页显示 job ID、generation、当前阶段、已经过时间、下次重试和最后错误。发现所有权不一致时进入 `recovering` 或 `failed`，不猜测、不模糊 kill、不删除无法证明归属的资源。

## 10. 测试与开放顺序

实现按以下可独立验证的顺序推进：

1. 持久配置、设备发现、绑定和批量任务模型。
2. 每节点 DNS 通道与短时动态授权。
3. 共享 xl2tpd/netifd L2TP 适配器。
4. SOCKS5/SLP 适配器。
5. LuCI 批量导入、设备绑定和作业进度页面。
6. 完整固件与 GL-MT6000 四轮真机测试。

每个阶段先用真实代码的 host/fake-platform 测试覆盖成功、deadline、进程崩溃、重启恢复、所有权冲突和无 WAN 回退，再进入目标 rootfs、完整固件和真机。测试环境可以使用真实节点，但任何“节点可用”结论都必须同时验证设备出口 IP 属于节点、DNS 经过该节点、外部 UDP 被阻断、节点掉线后没有主 WAN 回退。

## 11. 验收标准

- L2TP、SOCKS5、SLP 至少各有一个真实节点能在 deadline 内进入明确在线或失败状态。
- 已绑定设备能通过在线节点进行普通 TCP 浏览和支持 TCP 回退的应用通信；不承诺语音、视频、游戏或 QUIC 客户端业务。
- 一个节点可服务多个设备，一个设备不能同时获得两个节点授权。
- 40～60 节点导入是单一持久任务，页面刷新和路由器重启不丢进度。
- 节点、daemon、共享 xl2tpd 或单节点代理进程崩溃后自动撤权并按上限恢复，不要求人工重连。
- 任意异常、重载或启动窗口都不能让设备访问本地主 WAN、其他局域网设备或未白名单路由器服务。
