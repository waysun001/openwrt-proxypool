# ProxyPool V2 Phase 1 Safety Gate Implementation Plan

> **Execution:** use subagent-driven development, test-driven development, systematic debugging, and verification-before-completion. Implement each task with a focused failing test, a focused green run, a commit, and an independent review before moving on.

**Goal:** 在不依赖 V1/V2 daemon 存活的前提下，建立永久 fail-closed 安全边界；服务停止、配置损坏、节点掉线、防火墙重载或后端状态不确定时，普通终端都只能访问明确的路由器白名单，绝不回退本地 WAN。

**Target:** OpenWrt 23.05.3, GL-MT6000, IPv4 正式支持，最多 60 个节点。客户端外部 UDP 和 IPv6 转发全部拒绝。路由器自身到节点端点的隧道承载流量不属于客户端 UDP。

**Non-goals for this gate:** 本计划不实现共享 xl2tpd、经过节点的完整 DNS 数据面、MAC 自动学习、V2 正式数据面发布、批量任务队列或新 LuCI 页面。这些功能在后续阶段实现；缺失时保持断网或明确报告“不支持”，不能走主 WAN。本 gate 不宣称普通域名浏览、40–60 节点批量稳定、成功双配置 restore 或硬件级永久 L2 隔离已经完成，也不解除跨 backend 切换阻断。

## Safety invariants

1. 静态 guardian 独立于 `inet proxypool`、`ip proxypool_nat` 和任一 backend；ProxyPool 正常 stop/reload/cleanup 永远不能删除 guardian 表。
2. guardian 在 inet input/forward hook 的 fw4/V1 之后执行最终否决；较早 base chain 的 `accept` 不能绕过它。
3. 空动态授权集合就是默认状态；发布失败、读取不确定、缺 MAC/IP/出口或节点未 ready 时不生成授权。
4. 本 gate 固化 V2 L2TP `(MAC, IPv4, intended PPP ifname)` 和 V2 SOCKS5/SLP `(MAC, IPv4, active TCP listener)` 集合 schema，并证明默认空集合不会误授权；实际 V2 tuple 发布必须等 MAC 学习和正式 adapter 完成。未来发布时还必须匹配 ProxyPool magic mark，透明代理还必须匹配 `ct status dnat`。
5. V1 回滚兼容只允许 `(IPv4, exact PPP ifname)` 或 `(IPv4, exact TCP listener)`，仍必须匹配 magic mark。此兼容路径不解决恶意 MAC/IP 双重仿冒，V1 删除后一起删除。
6. 所有 br-lan 客户端 forward：IPv6 拒绝、UDP 拒绝、RFC1918/CGNAT/link-local/multicast/reserved 目标拒绝、未知出口拒绝。
7. Phase 1 input 只允许 DHCP、TCP 80/443 管理页及经过精确透明重定向授权的 TCP；安全 DNS 数据面尚未实现，因此客户端 TCP/UDP 53 也必须由 guardian 最终拒绝。SSH 和其他路由器 listener 默认拒绝。
8. guardian 先撤权再停节点；防火墙重载先重建空集合再有界 resync。任何失败结果必须是断网。
9. flow offload 关闭；二层隔离同时依赖 AP isolation、bridge-port isolation、MT7531 强制 CPU-only bridge 和 bridge-family nft 软件兜底。OpenWrt 23.05.3 原始 5.15.150 不会把 `BR_ISOLATED` 通知给 MT7531，而且端口加入 bridge 到 userspace 设置 flag 之间存在窗口；本项目固件默认拒绝 MT7531 bridge offload，使五个 LAN 口的硬件矩阵始终只到 CPU，再由 bridge guardian 拒绝客户端互转。必须使用固定源码和补丁完整构建，禁止把官方 ImageBuilder 固件当作安全产物。
10. DNS 无安全上游时返回失败；禁止恢复 ISP/WAN resolv.conf。

---

### Task 6.R: 修复独立评审发现的启动/升级合同缺口

**Files:**
- Modify: `proxypool-core/files/proxypool.init`
- Modify: `proxypool-core/files/proxypool.keep`
- Modify: `proxypool-core/src/proxypoold/internal/config/classify.go`
- Modify: `proxypool-core/src/proxypoold/internal/config/classify_test.go`
- Modify: `proxypool-core/src/proxypoold/cmd/proxypoolctl/main.go`
- Modify: `proxypool-core/src/proxypoold/cmd/proxypoolctl/main_test.go`
- Modify: `files/etc/config/proxypool_runtime`
- Modify: `scripts/test-proxypool-init.sh`
- Modify: `scripts/test-image-files.sh`
- Modify: `scripts/test-inspect-ipk.sh`
- Modify: `scripts/test-release-contracts.sh`

- [ ] 真实旧 V1 `remark` 配置必须分类为 V1；用 LuCI 实际可生成的字段全集测试，未知字段仍拒绝。
- [ ] 修正测试 harness 以匹配 OpenWrt 23.05.3 `rc.common`：`stop_service` 的返回值会被后续 `procd_kill/service_stopped` 覆盖，生产 init 必须在 `service_stopped` 显式传播失败。
- [ ] 旧 V1 upgrade 在 marker 缺失但 owned legacy runtime 存在时，`stop` 和 `enabled=0` 都必须实际 teardown；失败保留证据并返回非零。
- [ ] generation snapshot 构建失败、V1/disabled/阻断路径立即安全删除；V2 成功 reload 只保留当前一代。GC pointer 不是 backend 权威证据，只能删除 `$SNAPSHOT_ROOT/generation.<safe-token>`；不确定 procd 状态时宁可保留，禁止路径穿越或误删。
- [ ] 新增 root-only 持久 `/etc/proxypool/activated-backend` 与 `/etc/proxypool/cleanup-required`，并由 keep.d 精确保留。activated 缺失时只允许 `selector=missing/v1 + strict V1` bootstrap；任何 V1 mutation 前先原子预占 v1。selector=v2 且 activated 缺失一律阻断，V2 只能由后续显式安全激活事务预占。
- [ ] stop、disable、reboot 和保留配置 sysupgrade 永远不清 activated；selector 与 activated 不一致立即阻断。cleanup-required 存在、损坏、symlink 或与 activated 不一致时优先视为 corrupt；所有 destructive transition 先写持久 WAL，只有实际状态确认后才清。
- [ ] Phase 1 ImageBuilder selector 默认改为 `v1`。模拟旧 V1 sysupgrade 时，新 ROM 不得引入 `v2_shadow` 选择；已存在的 selector/activated 仍按 keep.d 恢复。
- [ ] `procd-state --instance` 只有结构正确且 `running:true` 才能作为启动成功；`running:false` 是 configured/not-running，不能发布 runtime marker。停止清理仍要求整个目标/服务对象 absent。
- [ ] V1 mutation 前再次逐字节比较 live 配置与已分类 snapshot；Task 7.5 再用 LuCI/leaf 共享 gate 封住普通并发写。此 gate 不宣称能阻止 root 管理员绕过锁直接改文件。
- [ ] 同 backend 启动失败若不能恢复旧实例，必须确认 stopped 后清 runtime marker并返回失败；状态 unknown/cleanup 失败写持久 quarantine，绝不能保留“运行成功”证据。持久 activated 只表示 backend 所有权，不表示节点 ready。

---

### Task 7.1: 固化 guardian 合同和 host 测试骨架

**Files:**
- Create: `scripts/test-proxypool-guard.sh`
- Create: `scripts/test-firewall-defaults.sh`
- Modify: `scripts/test-host.sh`
- Modify: `scripts/test-release-contracts.sh`

- [ ] 先写失败测试，固定表名、hook、priority、集合 schema、规则顺序和禁止项。
- [ ] 断言 guardian 使用独立 `inet proxypool_guard` 与 `bridge proxypool_l2_guard`，不得写入 V1 表。
- [ ] 断言最终 input/forward priority 大于 fw4 filter priority 0，且末尾存在 br-lan drop。
- [ ] 断言存在 V1 精确 IP/出口集合与 V2 MAC/IP/出口集合；拒绝仅 `ifname` 或仅 magic mark 的宽泛授权。
- [ ] 断言 input 透明代理同时要求 magic、`ct status dnat` 和动态 listener tuple；禁止固定端口范围作为最终授权。
- [ ] 断言 V1/V2 cleanup 源码不含 guardian table delete、`fw4 flush` 或 `nft flush ruleset`。
- [ ] 若 host 存在 `nft`，运行 `nft -c -f`；Windows 明确 SKIP，Linux CI 必须执行。

### Task 7.2: 安装永久 guardian、早启动服务和 fw4 显式 include

**Files:**
- Create: `proxypool-core/files/proxypool-guard.nft`
- Create: `proxypool-core/files/proxypool-guard.init`
- Create: `proxypool-core/files/proxypool-firewall-defaults`
- Create: `proxypool-core/files/proxypool-fw4-input-gate.nft`
- Create: `proxypool-core/files/proxypool-fw4-forward-gate.nft`
- Create: `proxypool-core/files/guard-resync.sh`
- Create: `proxypool-core/files/proxypool-uci-staged.uc`
- Modify: `proxypool-core/Makefile`
- Modify: `scripts/inspect-ipk.sh`
- Modify: `scripts/test-inspect-ipk.sh`

- [ ] Guardian init 使用 `START=18`，在 firewall `START=19` 前加载空授权规则；普通 stop 只撤权、不删静态表。
- [ ] 显式创建四个命名 UCI include，不依赖 `auto_includes` 或匿名 section 下标：guardian 固定 `ruleset-prepend`，候选 input/forward gate 分别固定 `chain-prepend input`/`chain-prepend forward`，resync 固定 `type script` 且 `fw4_compatible=1`。staged renderer 还必须在输出中找到三份 nft include 的唯一签名，避免 fw4 忽略无效 position 后仍然“检查通过”。
- [ ] Guardian init 与 fw4 include 都可幂等执行同一个 delete/recreate 事务；`guard.nft` 每次 firewall start/reload 原子重建空集合，不因 table already exists 失败。
- [ ] 有界 resync 先验证 guardian schema/version，只能从权限受控、generation 匹配的 ownership manifest 和实际 ready 状态恢复；不得凭 `ppp-*`、PID 存活或 UCI enabled 猜测授权。manifest 缺失/损坏、状态 unknown 或 nft apply 失败都保持空集合。
- [ ] fw4 input gate 只让候选 transparent TCP 到达 guardian；不能直接开放 redsocks 端口。
- [ ] fw4 forward gate 只让带正确 ProxyPool magic 的 IPv4 TCP 候选越过 fw4 的 priority 0 reject；priority +10 guardian 随后仍必须用精确 tuple 最终裁决。联合 ruleset 测试证明合法 V1 L2TP TCP 能到 guardian，而错误 mark、错误 PPP 或未授权 PPP 不能获得最终 accept。
- [ ] live package upgrade 必须显式 enable 新 guardian init；sysupgrade/image build 也保留启动链接。
- [ ] IPK inspector 检查文件存在、权限、精确 include 路径与 init 启动顺序。
- [ ] 明确把管理员主动 `/etc/init.d/firewall stop`、`fw4 flush`、`nft flush ruleset` 列为 root 管理级旁路操作；ProxyPool 自己不得调用。

### Task 7.3: 事务式收敛 fw4、LAN IPv6 和管理白名单

**Files:**
- Modify: `proxypool-core/files/proxypool-firewall-defaults`
- Modify: `files/etc/uci-defaults/99-proxypool-lan`
- Create fixtures under: `scripts/testdata/firewall/`

- [ ] 用 section name/src/dest 查找并删除全部 `lan -> wan` forwarding；zone 任意排序、额外 zone 和重复旧规则都要收敛。
- [ ] `lan` zone 设置 input/forward reject；只添加命名 DHCP、80/443 input rules。旧 DNS allow 必须删除，并由独立 guardian 在 fw4 之后再次保证 TCP/UDP 53 关闭。
- [ ] 不创建动态宽泛 tunnel zone 或 `lan -> wan/tunnel` forwarding；L2TP 候选仅由 `chain-prepend forward` magic gate 送到 guardian，NAT/route 仅由 Task 7.4 的精确 ownership manifest 管理。禁止 `ppp-*`、全局 masquerade 或其他可能接管第三方 PPP 的宽泛 device。
- [ ] 关闭 software/hardware flow offload。
- [ ] 禁用 LAN RA、DHCPv6、NDP proxy 与 IPv6 delegation；guardian 仍保留 IPv6 drop。
- [ ] 所有 UCI 变更先在临时副本生成，并通过显式 staged-check wrapper 验证。OpenWrt 23.05.3 pinned libuci 的 `uci_set_savedir()` 不会移除默认 `/tmp/.uci` delta path，因此禁止把 `uci -c`、`uci -P` 或仅改 `uci.cursor()` 误当成隔离：mutation helper 必须在单一 cursor 中绝对路径 `load($STAGE/package)` 三个包（`has_delta=false`），再用稳定真实 section 名完成修改并只 commit 到 stage；fw4 checker 必须唯一 patch 官方 `this.cursor.load("firewall")` 为绝对 staged firewall load。保存原配置后再原子替换、一次真实 `fw4 check/reload`；替换或 reload 失败必须恢复原配置并保持 guardian 空授权，不能留下半套 firewall。

#### Task 7.3 activation and recovery invariants (review addendum)

- [ ] 在任何 guardian、UCI、staging 或 live-config mutation 前，全程取得独立的 ProxyPool transaction lock；官方 `/var/run/fw4.lock` 从首次 kernel probe 一直持有到 config swap、absolute-config activation 与 ACTION=includes 完成。activator 继承同一个 fd 1000，不能在 swap 与 activation 间 release/reacquire。两个 ProxyPool transaction 或普通 fw4 reload 绝不能在该窗口交错。
- [ ] 第一次 flowtable probe 必须是事务中的首个只读 gate；持有 fw4 lock 后、one-way clamp/guardian reset 前再次 probe；live install 紧前第三次 probe。首次 active/unknown 必须零 guardian/UCI/stage mutation；后续发现 active 时禁止 install，并保持已有 empty guardian。不得热拆未知 flowtable，明确要求 cold reboot。
- [ ] `PROXYPOOL_COLD_BOOT=1` 只有在 `/var/run/fw4.state` 与实际 `inet fw4` table 都不存在时才成立；live mode 必须二者都存在。状态不一致或无法判定一律 fail closed。
- [ ] 在第一次 mutation 前证明 `firewall`、`dhcp`、`network` 没有 pending UCI delta；存在或无法判定均零 mutation 拒绝。最终 live activation 不得调用会重新读取默认 `/tmp/.uci` delta 的普通 `fw4 reload`：必须唯一 patch OpenWrt 23.05.3 pinned `this.cursor.load("firewall")` 为 absolute managed package load，用与 staged checker 同源的 renderer/checker 完成 nft apply 与 ACTION=includes。
- [ ] live `flow_offloading=0` / `flow_offloading_hw=0` 是 one-way safety clamp：在确认无 active flowtable 后，用 absolute load 单次原子 commit，永不回滚为 1。后续所谓“original”专指 clamp 后 byte baseline；失败测试必须验证 dhcp/network 原样且 firewall 仅保留该安全夹紧，不能错误要求恢复真正的 offload=1 原件。
- [ ] 三文件配置事务使用 root-only 持久 journal `/etc/proxypool/firewall-transaction`（可注入 test seam），包含 clamp 后 original、validated staged、严格 manifest/state 与创建 boot ID。文件和 state 原子落盘并 `sync` 后才允许逐文件 replacement；SIGKILL、掉电或 rollback 失败必须保留 journal，绝不由 EXIT cleanup 删除唯一恢复证据。
- [ ] 每次 apply 与 S18 guardian boot 都先处理 journal：跨 boot 的 `prepared`/`validated`/`installing-*`/`installed-all`/`restored-needs-fw4` 必须 byte-restore clamp 后 original；同 boot `awaiting-fw4-start` 保留给 S19。journal symlink、缺文件、宽权限、坏 schema/state/boot ID 一律 quarantine/fail closed，不能猜测或删除。
- [ ] cold install 不调用 reload，写 `awaiting-fw4-start`。只有 S19 已成功执行 nft apply 后的官方 `ACTION=includes` 阶段，`guard-resync.sh` 才能在 empty reset 成功后 finalize journal；include 失败被 fw4 仅 warning，因此 finalize 失败必须保留 journal 与 cold uci-default wrapper，供下次 boot 恢复/重试。
- [ ] S18 失败不会自动阻止独立 S19：guardian `boot()` 必须先在 `/var/run/fw4.state` 放 quarantine sentinel，再 reset empty、recover journal、确认无 pending delta/用户 nft include，并用 absolute checker 验证 live bytes；仅全部成功才删除 sentinel 放行 pinned S19。任一步失败保留 sentinel，使 `fw4 start` 在 nft 前拒绝。sentinel/state path 必须可注入测试。
- [ ] pinned fw4 即使 ACTION=start 的 nft pipeline 失败仍会继续 ACTION=includes，所以 cold finalizer 不能仅凭 `ACTION=includes` 清 WAL：还必须验证实际 `inet fw4` table、两条 candidate gate、无 active flowtable及 live bytes 与 awaiting manifest 一致。否则保留 journal 和 cold wrapper。官方 S19 的 S18→S19 窗口仍列为真机门禁，不宣称抵抗并发 hostile root。
- [ ] 所有非 ProxyPool-owned UCI firewall include（包括 script include）删除或拒绝，`defaults.auto_includes=0`；OpenWrt 23.05.3 ruleset template 还会无条件 include `/etc/nftables.d/*.nft`，因此该目录任何非 owned `*.nft` 都必须在 checker/activation 前拒绝。最终完整 render 显式拒绝 `flowtable` 与 `flow add/offload`。不能保留一个可在最后 probe 后重新创建 ingress fastpath 的第三方 include。
- [ ] 自有 activator 必须显式检查 `ACTION=start utpl | nft` 的 nft 退出状态，失败时绝不执行 ACTION=includes/finalize；不能照搬 pinned `/sbin/fw4` 在 pipeline 后继续 includes、从而以最后一个成功状态掩盖 nft 失败的行为。
- [ ] OpenWrt 23.05.3 `default_postinst` 会在 live opkg 中无条件运行包清单里的 `/etc/uci-defaults/*`、随后全局 `uci commit`，并调用包内 init 的 `start`。因此 core IPK 不直接拥有 uci-default 脚本，只安装 `/usr/lib/proxypool/` 下模板，由 custom postinst 在目标 root 或 live 尝试失败时原子发布排序晚于 `99-proxypool-lan` 的 cold wrapper；guardian 的普通 `start_service` 必须 no-op，真实 S18 行为放在 `boot()`，reload/stop 仍撤权，upgrade custom postinst 显式 enable S18。live transaction 遇 active flowtable 等门禁失败时保留 wrapper；cold wrapper 在 S19 runtime 验证并 finalize journal 前必须返回失败而不能被 S10 删除。
- [ ] live postinst 在第一个只读门禁失败时必须保持零 mutation，因此无法同时停止升级前仍在运行的旧 V1 进程、cron 或旧防火墙。此时属于 **live-deferred 部署边界**：postinst 必须明确提示立即冷重启；冷重启完成前禁止接入客户端且不得视为 fail-closed。若未来要求热升级后即时隔离，必须另立会主动断网的 quarantine 事务及真机测试，不能把当前 cold fallback 宣称成该能力。

#### S18 inhibitor threat boundary (review addendum)

- [ ] A pre-existing real directory (or symlink to a directory) at root-owned `/var/run/fw4.state` cannot be atomically replaced by the pinned BusyBox `mv`. S18 must not move it; it holds `/var/run/fw4.lock`, ignores HUP/INT/TERM, and stops boot progress. A simultaneous SIGKILL/OOM of that holder is an explicit double-fault/root-corruption hardware gate, not a proven software invariant. Do not claim hostile-root or arbitrary-inode-plus-SIGKILL safety until a durable S19 gate/atomic exchange mechanism is implemented and tested on target.

### Task 7.4: 让 V1 数据面只发布精确、可撤回授权

**Files:**
- Modify: `proxypool-core/files/firewall.sh`
- Create: `proxypool-core/files/guard-runtime.sh`
- Create: `proxypool-core/files/route-ownership.sh`
- Create: `scripts/test-v1-guard-publication.sh`

- [ ] 每次 full rebuild 第一步原子清空 V1 guardian 集合；第二阶段失败时集合保持空。
- [ ] 在独立 filter prerouting chain 清理保留 mark mask，再对精确 V1 候选流量写 magic mark；不能依赖 NAT chain 给后续包打标。
- [ ] L2TP 只有 PPP 已有 IPv4、精确策略路由成功后才发布 `(source IPv4, exact PPP ifname)`；最终 guardian 只允许 TCP。
- [ ] SOCKS5/SLP 只有 listener 已确认监听、redirect 已提交后才发布 `(source IPv4, exact listener port)`。
- [ ] remove/down/stop 顺序固定为：撤 guardian 授权和 redirect -> 停 listener/PPP -> 清理 route/table。
- [ ] 删除 V1 的 RFC1918 互访 accept；私网、广播、组播和未知 forward 都由 guardian 最终拒绝。
- [ ] policy route ownership manifest 固定路径、root-only 权限、schema、generation 及每条 rule/route 的完整参数；创建前发现 table/priority 已占用必须失败，不能先覆盖再记账。
- [ ] 重启后只从校验通过且与实际对象逐项一致的 manifest 恢复所有权；manifest 缺失/损坏进入 quarantine。删除必须使用完整参数逐条精确删除，禁止扫描 100–199，也禁止因为“拥有 table”而 flush 整表。
- [ ] 锁超时不得强删其他活跃 owner 的锁；失败应返回 busy/timeout，由上层有界重试。

**Phase 1 review resolution:** 上述完整 V1 ownership 尚未实现，而且旧 leaf 会扫描路由表、停止全局 xl2tpd、模糊杀 pppd 并修改全局 `chap-secrets`。本阶段不再尝试修补该架构：在 Task 7.4/7.5 全部所有权证据完成前，所有 V1 start/stop/reload/delete/probe/watchdog mutation 都由共享 `legacy-gate.sh` 在第一处副作用前返回 `legacy_runtime_quarantined`；LuCI 也必须在任何 UCI/pending 写入前拒绝。guardian 动态集合保持空。该隔离是安全门禁，不是可用 V1 回滚数据面。

### Task 7.5: 收回 xl2tpd、PPP callback 与 legacy leaf 所有权

**Files:**
- Modify: `proxypool-core/files/proxypool.init`
- Modify: `proxypool-core/files/proxypool.sh`
- Modify: `proxypool-core/files/firewall.sh`
- Modify: `proxypool-core/files/l2tp-manager.sh`
- Modify: `proxypool-core/files/socks5-manager.sh`
- Modify: `proxypool-core/files/slp-manager.sh`
- Modify: `proxypool-core/files/dns-manager.sh`
- Modify: `proxypool-core/files/watchdog.sh`
- Modify: `proxypool-core/files/lease.sh`
- Modify: `proxypool-core/files/timeout-check.sh`
- Modify: `proxypool-core/files/timeout-rotate.sh`
- Modify: `proxypool-core/files/status.sh`
- Modify: `proxypool-core/files/ppp-up.sh`
- Modify: `proxypool-core/files/ppp-down.sh`
- Modify: `proxypool-core/Makefile`
- Modify: `luci-app-proxypool/luasrc/controller/proxypool.lua`
- Create: `proxypool-core/files/legacy-gate.sh`
- Create: `scripts/test-legacy-gate.sh`

- [ ] 不再安装或覆盖全局 `/etc/ppp/ip-up`、`/etc/ppp/ip-down` 和全局 `ip-{up,down}.d` hook。
- [ ] V1 生成的 root-only peer options 显式指定 `/usr/lib/proxypool/ppp-{up,down}.sh`，使用私有 password/auth material，并携带 owned `ipparam` session token；start/stop 不得按用户名修改或删除全局 `/etc/ppp/chap-secrets`，相同用户名的两个节点及第三方 PPP 凭据都必须保持不变。
- [ ] init 在停 cron/watchdog 后才进入 teardown，并创建 root-only、动作绑定、时效有界的 transition nonce；每个节点另有随 generation 生命周期有效的 PPP session token，支持 pppd 后续 redial/up。transition nonce 与长期 session token 不得混用。
- [ ] up callback 必须验证稳定 actual=v1，或验证正在进行的严格 startup transition；两条路径都还必须核对接口名、client、generation 和 session token。down callback 只能撤销它所持有 session 的精确授权/route，即使 selector 已改变也不得扩大 mutation。任一不匹配零 mutation。
- [ ] public V1 leaf mutation 仅在 selector/actual 都是 v1 且无 transition/quarantine 时允许；仅严格旧 V1 upgrade 可接受 selector 缺失。内部 startup/teardown 只能使用 init 创建、权限受控的 transition nonce。
- [ ] 删除每节点 `ensure_system_xl2tpd_disabled` 及全局 PID 动作。任何全局 xl2tpd 服务变更只能由 init 的显式 V1 transition 统一执行；v2_shadow 路径绝不停止/disable 系统 xl2tpd，不得 kill `/var/run/xl2tpd.pid`，不得清理其他 PPP。
- [ ] 所有受管进程（xl2tpd、pppd、redsocks、slp-client）的 PID、进程启动标识、可执行文件、配置路径、client、generation/session，以及 PPP 接口都必须与 ownership manifest 全部匹配后才能报告 ready、stop 或 delete；PID 复用、陈旧 PID 文件、同名接口、cmdline 模糊 grep 或任何不能证明所有权的状态一律 quarantine，不报告 ready、不删除、不 kill。
- [ ] unknown/mismatch 状态阻断 watchdog、lease、timeout、status probe、PPP callback、LuCI start/stop/reconnect 和 manager 直接调用；LuCI 必须返回明确 blocked 状态，不能先写 V1 UCI 或谎报 success。
- [ ] 在这些所有权测试全部通过前，跨 backend 自动切换继续返回明确阻断错误。
- [ ] Phase 1 quarantine 测试必须覆盖 init、主 dispatcher、三个 manager、firewall public leaf、watchdog 与 LuCI；每个入口在缺 ownership manifest 时均为 zero mutation，不能因为 action 是 stop/delete 就放宽。

### Task 7.6: DNS 永久 fail closed

**Files:**
- Modify: `proxypool-core/files/dns-manager.sh`
- Modify: `proxypool-core/files/proxypool-firewall-defaults`
- Modify: `proxypool-core/files/status.sh`
- Modify: `luci-app-proxypool/luasrc/controller/proxypool.lua`
- Create: `scripts/test-dns-fail-closed.sh`

- [ ] 持久 `dnsmasq.noresolv=1`；restore/check 无安全 listener 时删除 ProxyPool server 并返回失败，绝不设置 `noresolv=0`。
- [ ] 测试服务 stop、SLP listener 消失、坏 port 文件和 restart 失败都不会读取 WAN resolv.conf。
- [ ] 经过节点的安全 DNS 未实现前，状态/API 明确报告 `dns_path_unavailable`；不能宣称 V1 rollback、网页或微信已经可用，也不能用该状态解除 backend 切换阻断。
- [ ] 后续安全 DNS 数据面完成前，客户端到路由器 TCP/UDP 53 直接关闭，因此表现为 DNS timeout/安全断网，不承诺 SERVFAIL，更不以 ISP DNS 维持可用性。节点 endpoint 自身若使用域名，也必须明确报告当前无法安全解析，不能偷偷走 WAN resolver。
- [ ] `noresolv=1` 且删除 UCI `server` 只证明基础 UCI fallback 被关闭，不证明 `serversfile`、`confdir`、`dnsmasqconf` 或其他片段没有注入上游；Phase 1 的最终安全证据是 guardian 关闭客户端 53。未来开放 53 前必须校验并拥有全部 dnsmasq 配置来源。

### Task 7.7: LAN/Wi-Fi 二层隔离

**Files:**
- Modify: `files/etc/uci-defaults/99-proxypool-lan`
- Modify: `proxypool-core/files/proxypool-firewall-defaults`
- Modify: `proxypool-core/files/proxypool-guard.nft`
- Modify: `proxypool-core/Makefile`
- Modify: `scripts/inspect-ipk.sh`
- Modify: `scripts/test-inspect-ipk.sh`
- Create: `proxypool-core/files/lan-isolation.sh`
- Create: `proxypool-core/files/lan-isolation-worker.sh`
- Create: `proxypool-core/files/proxypool-lan-isolation.hotplug`
- Modify: `proxypool-core/files/proxypool-guard.init`
- Modify: `proxypool-core/files/proxypool-uci-staged.uc`
- Modify: `proxypool-core/files/proxypool.keep`
- Create: `scripts/test-lan-isolation-defaults.sh`
- Create: `openwrt-patches/23.05.3/998-net-bridge-offload-br-isolated.patch`
- Create: `openwrt-patches/23.05.3/999-net-dsa-mt7530-bridge-port-isolation.patch`
- Create: `scripts/test-kernel-isolation-contract.sh`
- Create: `scripts/verify-openwrt-kernel-isolation.sh`
- Modify: `.github/workflows/build.yml`
- Modify: `.github/workflows/build-fast.yml`
- Modify: `config/gl-mt6000.config`

- [ ] staged UCI 必须证明 GL-MT6000 的物理端口集合恰好为 `lan1..lan5`，runtime 再枚举 br-lan 的全部实际成员（包括动态 Wi-Fi 成员）；缺少任一物理口、出现额外物理口或复用到其他 bridge/VLAN/interface 一律失败。
- [ ] 每个有线 bridge port 设置 isolate；每个 LAN AP 设置 `isolate=1` 与 `bridge_isolate=1`。network restart、端口重新加入 bridge 和 Wi-Fi reload 只发布持久 reconciliation 请求，由唯一 procd worker 串行重施、指数退避并周期审计；失败保持 bridge guardian 拒绝。
- [ ] 冷启动无线校验失败不能只调用 `wifi down`。必须先持久发布 root-only `PREPARING`，保存 recovery 原始字节，再同 overlay 原子安装所有 `wifi-device`/`wifi-iface`/`wifi-vlan`/`wifi-station` 均 `disabled=1` 的 fallback；语法无法解析时安装可解析的空 wireless。活动文件复核成功且无 pending delta 后发布 `DISABLED`。首次变更或 `PREPARING` 使 S18 阻塞 S20；真正冷重启后，精确匹配且全禁用的 `DISABLED` 只允许有线管理启动，LAN readiness 仍失败，直到管理员替换配置并通过完整正常拓扑校验。
- [ ] `/etc/proxypool/wireless-quarantine` 及 recovery 必须加入 sysupgrade keep 合同；package payload 不得预创建运行时 quarantine。状态、recovery、disabled reference、临时事务的 owner/mode/link、同文件系统和原子发布均需故障注入测试。不能宣称抵抗 S10 与 S18 在安全 rename 前同时被 SIGKILL/OOM 的双重故障。
- [ ] bridge-family guardian 拒绝 `ibrname br-lan -> obrname br-lan` 的所有 bridge forward 帧。
- [ ] core package 显式依赖 `firewall4`、`kmod-nft-bridge` 与提供 `/usr/sbin/bridge` 的 `ip-bridge`，并把 reconciliation/hotplug 安装到精确路径和 0755 mode；IPK inspector 对缺依赖、缺文件、错路径、错 mode 逐项失败。
- [ ] 回移上游通用先决 `c3976a3f84451ca05ea5be013af6071bf9acab2c` 的 `BR_ISOLATED` switchdev 通知，以及 MT7530/MT7531 `c25c961fc7f36682f0a530150f1b7453ebc344cd` / `3d49ee2127c26fd2c77944fd2e3168c057f99439` 的 port-matrix 隔离；补丁必须适配 OpenWrt 23.05.3 完整补丁栈后的旧 API，并通过 full-source kernel prepare/build。
- [ ] 单靠上述 flag offload 仍有 enslave-to-flag 窗口；GL-MT6000 固件必须默认拒绝 MT7531 bridge join offload、保留 user-port→CPU-only matrix，并通过只读 `proxypool_cpu_only_bridge=Y` 参数提供运行时证明。关闭该参数或缺少证明时不得作为安全固件。
- [ ] SDK 只构建/审计 IPK；fast workflow 不得下载 ImageBuilder、产出 sysupgrade 或发布固件。唯一测试固件来自固定 OpenWrt commit `01170d518da1c8ade9d26e56d0135d12cda8e781` 的 full-source build；packages、LuCI、routing、telephony feeds 也必须固定为官方 23.05.3 `feeds.buildinfo` 对应的完整 commit。kernel prepare 后必须验证最终源码和内建 MT7530 config，再允许编译镜像。
- [ ] 禁止 flow offload；host/CI 只宣称补丁上下文、源码构建和 reconciliation 合同通过。永久硬件 L2 隔离仍必须由两台有线设备跨两个物理 LAN 口进行 ARP/IPv4/IPv6/广播真机门禁后才能宣称。
- [ ] 记录用户已确认的拓扑限制：同一路由器 LAN 口下不接承载多终端的非管理交换机。

### Task 7.8: 清理危险首启副作用、备份和 sysupgrade 合同

**Files:**
- Modify: `luci-app-proxypool/Makefile`
- Remove or neutralize: `luci-app-proxypool/root/etc/uci-defaults/luci-proxypool`
- Modify: `proxypool-core/files/backup.sh`
- Modify: `scripts/inspect-ipk.sh`
- Modify: `scripts/test-inspect-ipk.sh`
- Modify: `.github/workflows/build-fast.yml`
- Create: `scripts/test-backup-contract.sh`

- [ ] LuCI IPK 不再停止 xl2tpd、修改匿名 firewall zone、重写 LAN/Wi-Fi 或打全局 PPP hook；递增 LuCI `PKG_RELEASE`。
- [ ] fresh-image provisioning 只在顶层 overlay，并使用 Task 7.3/7.7 的命名、幂等逻辑。
- [ ] sysupgrade keep list 精确保留 `proxypool_v2`、`proxypool_runtime`、`/etc/proxypool/activated-backend`、`/etc/proxypool/cleanup-required` 与唯一 crash-recovery 证据目录 `/etc/proxypool/firewall-transaction`；journal 必须版本化并在新版本严格验证，坏/旧格式 fail closed。运行时 journal 不作为 conffile、IPK 不预创建；package conffiles 仍只有 V1 `/etc/config/proxypool`，不得用镜像默认值覆盖持久证据。
- [ ] backup bundle 包含 V1、V2、selector、schema 和 manifest；legacy-only 与 full-dual 明确区分，partial-dual 一律拒绝。
- [ ] 本 gate 的 restore 明确返回 unsupported 且零副作用；在完整双配置事务 restore 后续实现前，不得停服务、解 tar 或写文件。路径穿越、symlink 与 partial-dual bundle 都需在任何 mutation 前拒绝。
- [ ] CI 必须实际检查 core 与 LuCI 两个 IPK；两个包都不得含全局 PPP hook，core callback 只能位于 `/usr/lib/proxypool/`；任一包都不得含 xl2tpd disable、匿名 firewall zone 修改或 LAN/Wi-Fi 重写副作用。

### Task 7.9: 安全门禁评审与切换策略

- [ ] 运行全部 host tests、Go tests/vet、shell syntax、`git diff --check`、Linux nft syntax、SDK core/LuCI IPK inspector、固定 OpenWrt 23.05.3 full-source kernel prepare/build 与目标 rootfs `fw4 check/print`。宿主机任意 nft 版本的 `nft -c` 只作补充，不能替代目标版本门禁。
- [ ] 独立安全评审检查所有安全不变量、cleanup 所有权和升级/回滚路径。
- [ ] 即使 host/CI 全绿，本 gate 的真机轮次也只确认 guardian、管理白名单、legacy quarantine、firewall reload 与 MT7531/Wi-Fi 隔离；这些结果作为后续解锁的必要证据，但本身不解除跨 backend 阻断，也不尝试连接旧 V1 节点。
- [ ] 本 safety gate 完成后仍不解除跨 backend 阻断。只有后续完成安全 DNS、正式 V2 tuple publication、持久 L2 验证等依赖，并经真机通过后，才另写计划和单独提交“解除跨 backend 阻断”；不得与 guardian 大改同一提交上线。
- [ ] 本 gate 的 40–60 节点真机范围仅验证预置配置在 guardian 下不泄漏及有界顺序启动；浏览器批量导入/并发连接稳定性属于后续 durable job/scheduler 阶段，不能作为本 gate 已完成功能。

## Deferred runtime defects (next phase)

- 批量导入当前会顺序发出 40–50 个独立 start API，再由后台任务并发进入 V1；下一阶段改成单个持久 job、限并发 scheduler 和可恢复进度。
- SLP 批量导入的前端会发送 token/transport/obfs 字段，但 controller 当前不持久化；批量导出也漏掉 token/transport/remark，往返会静默丢凭据。durable job 阶段必须用同一份严格 schema 做导入、存储、脱敏导出和逐项错误报告。
- V1 L2TP 的 `xl2tpd alive + PPP absent` 会被 start/watchdog 永久误判为已运行；下一阶段用共享 xl2tpd/netifd 状态机、deadline、generation 和 backoff 替代。
- 安全 DNS 数据面、MAC 自动学习和设备绑定在后续数据面阶段实现；在此之前 V2 动态集合保持空，不开放相应设备流量，V1 也不能宣称普通域名访问可用。

## External gates

- OpenWrt 23.05.3 SDK package build and actual IPK inspection（不产固件）。
- 固定 OpenWrt commit 的 full-source kernel/firmware build、目标 rootfs `fw4 check`/`fw4 print`、文件 mode/init symlink 与两份 MT7531 patch 最终源码证明；禁止以 ImageBuilder 替代。
- GL-MT6000 four-round hardware matrix: cold boot/stop/crash/reload leakage、管理白名单、legacy mutation 零副作用、两台有线设备跨物理口及 Wi-Fi 的 ARP/IPv4/IPv6/广播隔离、sysupgrade 保留合同。节点连接、普通域名浏览、40–60 节点批量稳定、成功 restore 与 V2 正式数据面由后续阶段门禁。
- 真机执行与证据回传步骤见 `docs/hardware/phase1-gl-mt6000-four-round-handoff.md`。
