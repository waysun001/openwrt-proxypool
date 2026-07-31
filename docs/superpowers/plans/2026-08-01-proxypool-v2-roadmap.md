# ProxyPool V2 Implementation Roadmap

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 分六个可独立审查的阶段，把现有多 Shell 并发实现替换为可恢复、严格 fail-closed、最多支持 60 个节点的 ProxyPool V2。

**Architecture:** UCI 保存期望状态，Go 常驻服务 `proxypoold` 是运行状态、netifd、共享 xl2tpd、代理、DNS、nftables 和策略路由的唯一写者。LuCI 通过本机 Unix socket 提交有版本的 JSON 请求；永久 fw4 拒绝基线保证 daemon 不存在时也不会回退本地主 WAN。

**Tech Stack:** OpenWrt 23.05.3、Go 1.20、procd、netifd、xl2tpd、PPP、nftables/fw4、iproute2、dnsmasq、LuCI Lua、原生浏览器 JavaScript、Go/Lua/Node 测试。

## Global Constraints

- 目标硬件固定为 GL-MT6000（mediatek/filogic），正式包必须由 OpenWrt 23.05.3 SDK 和 ImageBuilder 验证。
- 节点硬上限为 60，常见规模为 40～50；L2TP 默认并发 4，仅允许在 3～5 内通过真机数据调整。
- L2TP 正式架构是 netifd + 单个共享 xl2tpd + 每节点独立 PPP 接口。
- UCI 只保存期望状态；`proxypoold` 是 ProxyPool 运行对象的唯一写者。
- 未绑定、节点离线、服务崩溃、启动中、network reload 和 firewall reload 都必须 fail closed。
- 普通终端只允许访问路由器 DHCP、本地 ProxyPool DNS 和 LuCI 80/443；SSH 与其他路由器服务默认拒绝。
- 普通终端之间在 LAN-LAN、Wi-Fi-Wi-Fi、有线-无线场景始终隔离。
- SOCKS5/SLP 第一阶段只承载客户端 TCP；客户端外部 UDP 一律拒绝，但允许路由器到已配置 L2TP endpoint 的控制流量和 SLP endpoint 的 QUIC 隧道承载。
- 第一阶段只支持客户端 IPv4；阻止 IPv6 转发并抑制 AAAA。
- 用户不输入 MAC；设备必须先通过 DHCP 出现，系统自动建立 MAC、固定 IPv4 和节点绑定。
- 密码、token、Cookie 和 LuCI session 不得进入日志、状态 API 或诊断包。
- 每个功能和缺陷修复严格执行测试先行；每个阶段通过 host 测试和 OpenWrt 包构建后才能进入真机门。
- 不把 V1 与 V2 运行写者同时启用。开发期 V2 默认 shadow，切换时先安装永久拒绝基线，再停 V1，再启 V2。

---

## 1. 为什么拆成六份计划

规格同时包含控制面、L2TP、二三层隔离、透明代理、DNS、LuCI、迁移和硬件发布。把它们写成一个执行清单会让审查边界过大，也无法在失败时安全回退。因此本路线图只锁定公共接口和依赖顺序，具体测试与提交步骤放在六份阶段计划中。

| 阶段 | 计划 | 可独立验收的产物 | 真机门 |
|---|---|---|---|
| 1 | `2026-08-01-proxypool-v2-phase1-foundation.md` | 可构建的 daemon、配置模型、RPC、状态机，默认 shadow | 无，host + SDK |
| 2 | `2026-08-01-proxypool-v2-phase2-l2tp.md` | netifd + 共享 xl2tpd、deadline、分批恢复 | 第一轮 L2TP |
| 3 | `2026-08-01-proxypool-v2-phase3-isolation-binding.md` | 永久 fail-closed、设备自动绑定、L2TP 数据面、二层隔离 | 隔离预检 |
| 4 | `2026-08-01-proxypool-v2-phase4-proxy-dns.md` | SOCKS5/SLP TCP、按节点 DoH、UDP/IPv6 阻断 | 第二轮隔离/DNS |
| 5 | `2026-08-01-proxypool-v2-phase5-luci-migration.md` | 原子导入、V2 LuCI、迁移、诊断 | 第三轮批量/恢复 |
| 6 | `2026-08-01-proxypool-v2-phase6-release.md` | 12～24 小时稳定性、升级回退、清除旧路径、发布 | 第四轮 soak |

阶段必须按顺序执行。后续计划可以开发，但不能越过前一阶段的发布门激活到用户设备。

## 2. 锁定的源码布局

### 2.1 Go daemon

所有新 Go 代码位于 `proxypool-core/src/proxypoold/`：

```text
cmd/proxypoold/main.go             daemon 入口、依赖装配、信号处理
cmd/proxypoolctl/main.go           Unix socket CLI、事件入口、诊断入口
internal/model/                    UCI 期望状态和运行状态类型
internal/config/                   UCI V2 codec、验证、原子 store、V1 迁移
internal/api/                      JSON envelope、Unix socket server/client
internal/engine/                   单写者协调器、状态机、job、scheduler、retry
internal/platform/                 exec/fs/clock、ubus、netifd、nft、route、DHCP
internal/adapter/                  协议公共接口
internal/adapter/l2tp/             共享 xl2tpd/netifd L2TP adapter
internal/adapter/socks5/           redsocks SOCKS5 adapter
internal/adapter/slp/              SLP + redsocks adapter
internal/adapter/process/          procd 外协议子进程 supervisor
internal/dataplane/                nftables、policy mark、策略路由、原子发布
internal/device/                   DHCP inventory、固定租约、绑定一致性
internal/dnsproxy/                 DNS wire、按节点缓存、DoH transports
internal/importer/                 批量解析、preview、原子 commit
internal/diagnostics/              状态快照、脱敏、诊断包
internal/eventlog/                 有界结构化事件环
```

不创建单个“utils”大包。只在两个以上消费者使用并且责任明确时抽取公共代码。

### 2.2 OpenWrt 包装

```text
proxypool-core/Makefile
proxypool-core/files/proxypool.init
proxypool-core/files/proxypool.config
proxypool-core/files/proxypool-fw4.include
proxypool-core/files/99-proxypool-network-isolation
proxypool-core/files/proxypool-event.sh
proxypool-core/files/proxypool-migrate.sh
```

V1 Shell 文件在第四轮验收前保留，但 V2 激活后不得被 init、LuCI、PPP hook 或 cron 调用。最终删除清单放在第六阶段。

### 2.3 LuCI 与测试

```text
luci-app-proxypool/luasrc/controller/proxypool.lua
luci-app-proxypool/luasrc/model/proxypool_rpc.lua
luci-app-proxypool/luasrc/view/proxypool/main.htm
luci-app-proxypool/htdocs/luci-static/resources/proxypool-v2.js
luci-app-proxypool/htdocs/luci-static/resources/proxypool-v2.css
luci-app-proxypool/tests/test_rpc.lua
luci-app-proxypool/tests/ui/proxypool-v2.test.mjs
tests/integration/fake-openwrt/
scripts/test-host.sh
scripts/device-test/
.github/workflows/test.yml
```

## 3. 跨阶段公共接口

以下签名一旦在第一阶段合入，后续阶段如需破坏性修改必须单独提交迁移和所有调用点测试。

```go
package model

type Protocol string
const (
    ProtocolL2TP   Protocol = "l2tp"
    ProtocolSOCKS5 Protocol = "socks5"
    ProtocolSLP    Protocol = "slp"
)

type DesiredConfig struct {
    SchemaVersion int
    Revision      uint64
    Global        GlobalConfig
    Nodes         map[string]Node
    Devices       map[string]Device
}

type RuntimeState string
const (
    StateDisabled   RuntimeState = "disabled"
    StateQueued     RuntimeState = "queued"
    StateStarting   RuntimeState = "starting"
    StateValidating RuntimeState = "validating"
    StateOnline     RuntimeState = "online"
    StateDegraded   RuntimeState = "degraded"
    StateStopping   RuntimeState = "stopping"
    StateFailed     RuntimeState = "failed"
    StateBackoff    RuntimeState = "backoff"
    StateRecovering RuntimeState = "recovering"
)
```

```go
package adapter

type Adapter interface {
    Protocol() model.Protocol
    Start(context.Context, model.Node, uint64) (Observed, error)
    Probe(context.Context, model.Node, Observed) error
    Stop(context.Context, model.Node, Observed, uint64) error
    Inspect(context.Context, model.Node) (Observed, error)
    Events() <-chan Event
}
```

```go
package dataplane

type Snapshot struct {
    Revision uint64
    Nodes    map[string]model.NodeRuntime
    Devices  map[string]model.DeviceRuntime
}

type Reconciler interface {
    Publish(context.Context, Snapshot) (uint64, error)
    RevokeNode(context.Context, string) error
    Inspect(context.Context) (uint64, error)
}
```

```go
package config

type Store interface {
    Load(context.Context) (model.DesiredConfig, error)
    Replace(context.Context, uint64, model.DesiredConfig) (uint64, error)
}
```

`Replace` 的第二个参数是 expected revision；不相等返回 `revision_conflict`，不能覆盖新配置。

### 3.1 RPC envelope

每个连接只处理一条以换行结束的 JSON 请求，最大 1 MiB：

```json
{"version":1,"id":"req-123","method":"status.get","params":{}}
{"version":1,"id":"req-123","ok":true,"result":{}}
{"version":1,"id":"req-123","ok":false,"error":{"code":"revision_conflict","message":"配置已变化"}}
```

稳定方法名：

```text
status.get       node.save        node.delete      node.action
device.list      device.bind      device.unbind
import.preview   import.commit    job.get          job.list
system.activate  system.events    diagnostics.create
```

### 3.2 错误码

稳定错误码至少包含：

```text
invalid_request  invalid_config   revision_conflict  capacity_exceeded
duplicate        not_found        auth_failed        resolve_failed
connect_timeout  stop_timeout     probe_failed        wan_down
dataplane_failed dns_failed       unsupported         internal
```

页面可以本地化 message，但必须按 code 处理逻辑，不解析日志文本。

## 4. 每阶段通用验证

每个任务提交前执行与其相关的定点测试；每个阶段结束必须完整执行：

```bash
./scripts/test-host.sh
git diff --check
```

阶段 1 起，`scripts/test-host.sh` 固定执行：

```bash
go test ./...
go vet ./...
lua5.1 luci-app-proxypool/tests/test_rpc.lua
node --test luci-app-proxypool/tests/ui/*.test.mjs
```

不存在的测试套件在其阶段加入前由脚本明确跳过并显示 `SKIP: <reason>`，不能把命令错误吞掉。

OpenWrt 构建门：

```bash
make package/proxypool/proxypool-core/compile V=s -j1
make package/proxypool/luci-app-proxypool/compile V=s -j1
```

CI 使用官方 23.05 packages feed 的 `golang-package.mk`。`GO_PKG` 指向 `proxypoold`，构建目标固定为 `proxypoold/cmd/proxypoold` 和 `proxypoold/cmd/proxypoolctl`；安装时显式把两个二进制放到 `/usr/sbin/proxypoold` 与 `/usr/bin/proxypoolctl`。

## 5. 提交和审查规则

- 每个计划中的 Task 是最小审查单元；一个 Task 完成一个红-绿测试循环并独立提交。
- 不把生成固件、SDK、测试日志、密码或诊断包提交到 Git。
- 提交消息使用 `test:`、`feat:`、`fix:`、`refactor:`、`docs:` 前缀。
- 任何硬件门失败时只在当前阶段修复，不继续激活后续阶段。
- 删除 V1 代码必须是第四轮通过后的独立提交，便于恢复。

## 6. 激活顺序

```text
V1 active
  -> V2 shadow（只读检查，不写运行对象）
  -> 安装并验证永久 fail-closed 基线
  -> 停止 V1、清理 V1 自有对象
  -> 启动 V2 reconciliation
  -> 节点逐个 online 后发布设备授权
```

任何一步失败都停在 fail-closed。禁止出现“为了恢复管理方便临时允许 LAN -> WAN”的回退规则。

## 7. 规格覆盖矩阵

| 设计章节 | 实施计划覆盖 |
|---|---|
| 1～2 背景和已确认需求 | 路线图全局约束及全部阶段退出门 |
| 3 总体架构 | 第一阶段 daemon/RPC/engine，第三阶段永久基线和激活 |
| 4 数据模型 | 第一阶段模型/store，第三阶段 PolicyID/设备，第五阶段待学习绑定 |
| 5 设备发现与固定地址 | 第三阶段 Task 3～4，第五阶段 Task 2/5 |
| 6 网络隔离与 fail-closed 数据面 | 第三阶段全部任务，第四阶段 Task 6，第六阶段不变量检查 |
| 7 按节点 DNS | 第四阶段 Task 4～6 |
| 8 L2TP 共享架构 | 第二阶段全部任务和第一轮真机门 |
| 9 状态机、任务与重试 | 第一阶段 Task 5～6，第二阶段 Task 4～5 |
| 10 批量导入 | 第五阶段 Task 1/6/8 |
| 11 LuCI 交互 | 第五阶段 Task 3～6 |
| 12 启动、重载与恢复 | 第二阶段恢复、第三阶段激活、第六阶段故障/重启 |
| 13 配置迁移与回退 | 第五阶段 Task 2，第六阶段 Task 2 |
| 14 可观测性与诊断 | 第五阶段 Task 7，第六阶段 Task 1 |
| 15 分阶段实施 | 本路线图及六份有依赖顺序的阶段计划 |
| 16 测试策略 | 每个任务的 RED/GREEN 步骤和四轮硬件门 |
| 17 验收标准 | 第六阶段 release acceptance 和退出门 |
| 18 风险与控制 | 第二阶段共享 daemon、第三阶段硬件隔离、第四阶段 DoH、第五阶段迁移、第六阶段回退 |
| 19 架构决定 | 本路线图锁定的接口、源码布局和激活顺序 |
