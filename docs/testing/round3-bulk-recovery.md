# Round 3：40–60 节点批量与故障恢复测试

该测试用于 GL-MT6000 / OpenWrt 23.05.3 测试机，验证批量导入不会卡住、守护进程或 L2TP 组件重启后可自动恢复，并确认任何故障期间都没有本地 WAN 回退。

## 准备

1. 从 LAN 口连接管理电脑，确认可访问 `192.168.9.1`。不要从 WAN 侧运行，测试会短暂关闭 WAN。
2. 使用空的 V2 节点配置；脚本发现已有节点会停止，不会覆盖现有配置。
3. 将 `scripts/device-test` 目录复制到路由器。内置 60 节点文件仅使用保留测试地址，用于失败/退避与调度压力测试。要验证真实上线，把 `PROXYPOOL_VALID_IMPORT` 指向包含 40–60 个真实 L2TP 节点的文件。
4. 确保路由器时间正确，并预留至少 32 MiB `/tmp` 空间。

## 执行

先运行无故障预检：

```sh
sh scripts/device-test/round3-bulk-recovery.sh
```

在确认管理电脑通过 LAN 接入后运行完整故障注入。完整模式必须指定一个已自动发现的测试设备 ID，以及一个可执行的 LAN 客户端探针；缺少任一项会直接失败，不能形成“假通过”。

```sh
PROXYPOOL_ALLOW_FAULTS=1 \
PROXYPOOL_VALID_IMPORT=/root/import-real-l2tp.txt \
PROXYPOOL_TEST_DEVICE_ID=device_001122334455 \
PROXYPOOL_CLIENT_PROBE=/root/proxypool-client-probe.sh \
sh scripts/device-test/round3-bulk-recovery.sh
```

客户端探针会收到两类参数。稳定阶段是 `baseline`、`wan-down`、`wan-recovered`、`daemon-recovered`、`xl2tp-recovered`、`pppd-recovered`、`services-reloaded`；故障窗口阶段是 `wan-window`、`daemon-window`、`xl2tp-window`、`pppd-window`、`services-window`，脚本会在故障注入和恢复期间每秒并发调用，且至少成功两次。WAN 确认 down 或被终止的旧 PID 确认消失后，恢复检查开始前必须再取得一个成功样本，否则测试失败。探针必须从被绑定的 LAN/WiFi 终端发起检查，并仅在以下条件都符合时返回 0：管理页和路由器 DNS 可访问；LAN 同伴、本地 WAN、外部 UDP 和 IPv6 均不可访问。稳定阶段还须验证网页流量经绑定节点可用；`wan-down` 须验证节点不可用时保持断网。窗口阶段允许节点网页流量短暂中断，但即使中断也绝不能回退本地 WAN。

可用 `PROXYPOOL_JOB_TIMEOUT` 调整批量任务等待上限，默认 1800 秒。报告写入 `/tmp/proxypool-round3-<UTC时间>`，不会保存导入原文。成功结束时脚本只会经 daemon RPC 删除本次报告中记录的导入节点，不会删除测试期间由其他操作新增的节点；异常退出也会对这些已跟踪节点进行尽力清理。若路由器服务本身不可用，需恢复服务后按报告中的 `imported-node-ids.txt` 确认并清理残留再重跑。

## 通过标准

- 无效、重复和第 61 个节点均不会造成部分提交；有效批次只增加一次 revision。
- 批量任务的 `total` 必须等于实际导入的 40–60 个节点，且每个节点在上限内进入终态；一个节点失败不阻塞其余节点。
- 刷新 LuCI、重启 `proxypoold`、终止 `xl2tpd`/受管 `pppd`、WAN down/up、network/firewall reload 后，无需手动重连即可继续收敛。
- 脚本实际修改节点名称，在重连过程中再次修改并核对最终名称/revision；编辑、重连、删除任务均须达到允许的有界终态，旧任务结果不能覆盖新 revision。
- `before-*` 与 `after-*` 中只有 ProxyPool 自有 nftables、策略规则和路由；故障期间终端不能互访 LAN、不能访问本地 WAN、不能使用外部 UDP/IPv6。
- LuCI 仍允许访问路由器管理页和本地 DNS/DHCP；诊断包可生成、仅下载一次且不含节点认证信息。

内置测试地址不会上线，因此其预期终态是 `failed`/`backoff`。使用真实节点时，应额外确认已绑定设备能浏览网页和使用微信，同时未绑定或节点失效的设备保持断网。
