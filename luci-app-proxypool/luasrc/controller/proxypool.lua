-- 智联盒子 LuCI Controller
module("luci.controller.proxypool", package.seeall)

local LEGACY_GATE = "/usr/lib/proxypool/legacy-gate.sh"
local LEGACY_QUARANTINE_REASON = "legacy_runtime_quarantined"

-- Explicit security boundary: a newly added write action is not admitted by
-- falling through to a permissive default.
local LEGACY_MUTATION_ACTIONS = {
    ["save_client"] = true,
    ["delete_client"] = true,
    ["toggle_client"] = true,
    ["start_client"] = true,
    ["stop_client"] = true,
    ["save_remark"] = true,
    ["restart_client"] = true,
    ["set_dhcp_lease"] = true,
    ["reload"] = true,
    ["probe_all"] = true,
    ["clear_log"] = true,
    ["batch_import"] = true,
    ["batch_action"] = true
}

local function legacy_gate(scope)
    local sys = require "luci.sys"
    local safe_scope = tostring(scope or "unspecified"):gsub("[^%w_%.:%-]", "_")
    -- The controller denies independently of helper status, so missing or
    -- damaged package payload cannot turn quarantine into admission.
    sys.call("/bin/sh " .. LEGACY_GATE .. " mutation " .. safe_scope .. " >/dev/null 2>&1")
    return false, LEGACY_QUARANTINE_REASON
end

local function reject_legacy_api(action)
    local http = require "luci.http"
    legacy_gate("luci:api:" .. tostring(action or "unspecified"))
    http.status(409, "Conflict")
    http.prepare_content("application/json")
    http.write('{"error":"' .. LEGACY_QUARANTINE_REASON .. '"}')
end

local function reject_legacy_page(scope)
    local http = require "luci.http"
    legacy_gate("luci:" .. tostring(scope or "unspecified"))
    http.status(409, "Conflict")
    http.prepare_content("text/plain; charset=utf-8")
    http.write(LEGACY_QUARANTINE_REASON)
end

function index()
    -- 面板入口改为 call 处理：过期时渲染 locked 页面（一直"正在加载中"）
    entry({"admin", "services", "proxypool"}, call("main_page"), _("智联盒子"), 1).dependent = false
    entry({"admin", "services", "proxypool", "api"}, call("api_handler")).leaf = true
    -- 隐藏的使用期限设置页：登录后台后在面板 URL 后追加 /sz 进入
    entry({"admin", "services", "proxypool", "sz"}, call("sz_page")).leaf = true

    -- 设置为默认首页：创建 admin 根路由重定向
    entry({"admin"}, alias("admin", "services", "proxypool"), _("Administration"), 10).index = true
end

-- ============================================================
-- 使用期限（lease）：默认 360 天，到期后面板一直"正在加载中"
-- 计时基于内核单调时钟 /proc/uptime 的累计运行秒数（lease.sh 维护），
-- 完全不依赖墙上时钟，改/回拨系统时间无效。lease_days=0 表示永久解锁。
-- ============================================================

local LEASE_DEFAULT_DAYS = 360
local LEASE_MIN_SANE_TS = 1704067200       -- 2024-01-01：低于此值视为时钟未同步（仅用于显示估算）
local LEASE_USED_FILE = "/var/run/proxypool/lease/used"

-- 读取累计已用秒数：优先实时暂存文件，回退到 UCI 落盘值
local function lease_used_seconds(uci_c)
    local f = io.open(LEASE_USED_FILE, "r")
    if f then
        local c = f:read("*a"); f:close()
        local n = tonumber((c or ""):match("%d+"))
        if n then return n end
    end
    return tonumber(uci_c:get("proxypool", "global", "lease_used") or "") or 0
end

-- 返回 (expired:boolean, info:table)
-- info: { days, used, limit }
local function lease_info()
    local uci_c = require("luci.model.uci").cursor()

    local days = tonumber(uci_c:get("proxypool", "global", "lease_days") or "")
    if days == nil then days = LEASE_DEFAULT_DAYS end

    local used = lease_used_seconds(uci_c)
    local limit = days * 86400
    local expired = (days > 0) and (used >= limit)
    return expired, { days = days, used = used, limit = limit }
end

local function lease_expired()
    local e = lease_info()
    return e
end

-- 面板入口：未过期渲染正常面板，过期渲染锁定页（一直加载中）
function main_page()
    local http = require "luci.http"
    if http.formvalue("save") then
        return reject_legacy_page("main_page:save")
    end

    local tmpl = require "luci.template"
    if lease_expired() then
        tmpl.render("proxypool/locked", {})
    else
        tmpl.render("proxypool/main", {})
    end
end

-- 使用期限设置页（/sz）：显示当前状态，保存时重置起点并续期
function sz_page()
    local http = require "luci.http"
    local tmpl = require "luci.template"

    local msg_type, msg_text = nil, nil
    if http.formvalue("save") then
        return reject_legacy_page("sz_page:save")
    end

    local expired, info = lease_info()
    local used_str, remaining_str, expiry_str

    used_str = string.format("%.1f 天", info.used / 86400)

    if info.days <= 0 then
        remaining_str = "—"
        expiry_str = "永久（不锁定）"
    elseif expired then
        remaining_str = "已过期"
        expiry_str = "已过期"
    else
        local rem = info.limit - info.used
        remaining_str = string.format("%.1f 天（按持续开机估算）", rem / 86400)
        -- 预计到期日期：仅在系统时钟可信时按"从现在持续开机"估算显示
        local now = os.time()
        if now >= LEASE_MIN_SANE_TS then
            expiry_str = os.date("%Y-%m-%d", now + rem) .. "（估算）"
        else
            expiry_str = "—（时钟未同步）"
        end
    end

    tmpl.render("proxypool/lease", {
        msg_type = msg_type,
        msg_text = msg_text,
        days = info.days,
        used_str = used_str,
        remaining_str = remaining_str,
        expiry_str = expiry_str,
        expired = expired
    })
end

-- 输入清洗：只允许字母数字下划线（防止命令注入）
local function sanitize_client(raw)
    if not raw then return nil end
    local clean = raw:match("^([%w_]+)$")
    return clean
end

-- ============================================================
-- 高性能状态生成（Lua 原生实现）
-- 原 status.sh 对 50 个客户端产生 ~3000 次 fork，耗时 ~9 秒
-- Lua 版本仅需 ~3 次 fork（2×nft + 1×ip neigh），耗时 < 0.5 秒
-- ============================================================

local STATUS_RUN_DIR = "/var/run/proxypool"
local ARRAY_MT = { __jsontype = "array" }
local DNS_PATH_UNAVAILABLE = "dns_path_unavailable"

-- 强制 table 序列化为 JSON array（即使为空也输出 [] 而非 {}）
local function as_array(t)
    return setmetatable(t or {}, ARRAY_MT)
end

-- 写入 pending 操作状态文件（"stopping:时间戳"/"starting:时间戳"）
-- 格式 "op:timestamp" 允许无 nixfs 环境也能检测超时
local function set_pending_op(client_id, operation)
    local dir = STATUS_RUN_DIR .. "/pending"
    os.execute("mkdir -p '" .. dir .. "' 2>/dev/null")
    local f = io.open(dir .. "/" .. client_id, "w")
    if f then f:write(operation .. ":" .. os.time()); f:close() end
end

-- 读取文件内容，返回 trim 后的字符串或 nil（零 fork）
local function _read_file(path)
    local f = io.open(path, "r")
    if not f then return nil end
    local content = f:read("*a")
    f:close()
    if not content or content == "" then return nil end
    return content:match("^%s*(.-)%s*$")
end

-- 执行命令并返回输出
local function _read_cmd(cmd)
    local f = io.popen(cmd)
    if not f then return "" end
    local content = f:read("*a")
    f:close()
    return content or ""
end

-- 进程存活检测：通过 /proc/<pid>/status（零 fork）
local function _process_alive(pid_str)
    local p = tonumber(pid_str)
    if not p or p <= 0 then return false end
    local f = io.open("/proc/" .. p .. "/status", "r")
    if f then f:close(); return true end
    return false
end

-- 字节格式化（零 fork，替代 shell 版的 awk 调用）
local function _format_bytes(bytes)
    bytes = tonumber(bytes) or 0
    if bytes >= 1073741824 then return string.format("%.2f GB", bytes / 1073741824)
    elseif bytes >= 1048576 then return string.format("%.2f MB", bytes / 1048576)
    elseif bytes >= 1024 then return string.format("%.2f KB", bytes / 1024)
    else return tostring(bytes) .. " B" end
end

-- 解析 nft 计数器输出为 {ip = bytes} 查找表（单次遍历）
local function _parse_nft(output, prefix)
    local t = {}
    if not output or output == "" then return t end
    for line in output:gmatch("[^\n]+") do
        local ip = line:match('comment "' .. prefix .. '([%d%.]+)"')
        if ip then
            t[ip] = tonumber(line:match("bytes (%d+)")) or 0
        end
    end
    return t
end

-- 解析 ip neigh 输出为 {ip = {mac, state}} 查找表
local function _parse_neigh(output)
    local t = {}
    if not output or output == "" then return t end
    for line in output:gmatch("[^\n]+") do
        local ip = line:match("^(%S+)")
        if ip then
            t[ip] = {
                mac = line:match("lladdr (%S+)") or "",
                state = line:match("(%S+)%s*$") or ""
            }
        end
    end
    return t
end

-- Phase 1 has no safe hostname bootstrap resolver.  Only a canonical IPv4
-- literal avoids DNS; leading-zero and alternate textual forms are rejected
-- so the status API never guesses how a downstream resolver will interpret it.
local function _strict_ipv4_literal(value)
    if type(value) ~= "string" or not value:match("^%d+%.%d+%.%d+%.%d+$") then
        return false
    end

    local count = 0
    for part in value:gmatch("[^.]+") do
        count = count + 1
        if (part ~= "0" and part:match("^0")) or #part > 3 then
            return false
        end
        local octet = tonumber(part)
        if not octet or octet < 0 or octet > 255 then
            return false
        end
    end
    return count == 4
end

local function _endpoint_resolution_status(server)
    if not server or server == "" then
        return "missing_endpoint"
    elseif _strict_ipv4_literal(server) then
        return "literal_ipv4"
    end
    return DNS_PATH_UNAVAILABLE
end

-- Resolve by section type and interface ownership, never by a conventional
-- UCI section name.  Renamed sections are valid; missing or duplicate LAN
-- DHCP sections are ambiguous and must not be guessed.
local function _find_unique_lan_dhcp_section(uci_c)
    local match_count = 0
    local match_name = nil
    uci_c:foreach("dhcp", "dhcp", function(section)
        if section.interface == "lan" then
            match_count = match_count + 1
            match_name = section[".name"]
        end
    end)
    if match_count ~= 1 then return nil end
    if type(match_name) ~= "string" or match_name == "" then return nil end
    return match_name
end

-- 生成完整状态 JSON
-- 检测 WAN 口联网状态（WAN1/WAN2...）
-- 通过 ubus 读取所有网络接口，筛选 IPv4 WAN 口（名字以 wan 开头、排除 wan6），
-- 按接口名排序后依次映射为 WAN1/WAN2…；任一 WAN 已拨号且有 IPv4 地址即视为"有网"。
local function get_wan_status()
    local jsonc = require("luci.jsonc")
    local wans = {}
    local online = false

    local raw = _read_cmd("ubus call network.interface dump 2>/dev/null")
    if raw and raw ~= "" then
        local ok, d = pcall(jsonc.parse, raw)
        if ok and d and d.interface then
            local list = {}
            for _, ifc in ipairs(d.interface) do
                local nm = ifc.interface or ""
                -- 仅 IPv4 WAN 口：名字以 wan 开头且不以 6 结尾（排除 wan6/ipv6）
                if nm:match("^wan") and not nm:match("6$") then
                    local has_ip = ifc["ipv4-address"] and #ifc["ipv4-address"] > 0
                    list[#list + 1] = {
                        iface = nm,
                        up = (ifc.up == true) and has_ip and true or false,
                        ipaddr = (has_ip and ifc["ipv4-address"][1].address) or ""
                    }
                end
            end
            table.sort(list, function(a, b) return a.iface < b.iface end)
            for i, w in ipairs(list) do
                if w.up then online = true end
                wans[#wans + 1] = {
                    name = "WAN" .. i,
                    iface = w.iface,
                    up = w.up,
                    ipaddr = w.ipaddr
                }
            end
        end
    end

    -- 兜底：未能从 ubus 识别 WAN 口时，用默认路由判断是否有网
    if #wans == 0 then
        local route = _read_cmd("ip -4 route show default 2>/dev/null")
        local up = (route and route:match("default")) and true or false
        if up then online = true end
        wans[1] = { name = "WAN", iface = "", up = up, ipaddr = "" }
    end

    return { online = online, wans = wans }
end

local function generate_status()
    local uci_c = require("luci.model.uci").cursor()
    local jsonc = require("luci.jsonc")
    local nixfs = nil
    pcall(function() nixfs = require("nixio.fs") end)

    local global_enabled = tonumber(uci_c:get("proxypool", "global", "enabled") or "1") or 1

    -- 预取 nft 计数器（仅 2 次 fork，缓存为查找表）
    local out_ctr = _parse_nft(_read_cmd("nft list chain inet proxypool count_out 2>/dev/null"), "out_")
    local in_ctr = _parse_nft(_read_cmd("nft list chain inet proxypool count_in 2>/dev/null"), "in_")

    -- 预取 ARP 邻居表（仅 1 次 fork）
    local neigh_table = _parse_neigh(_read_cmd("ip neigh show 2>/dev/null"))

    -- IP 归属地：请求内去重缓存
    local loc_memo = {}
    local function get_location(server)
        if not server or server == "" then return "" end
        if loc_memo[server] ~= nil then return loc_memo[server] end

        local cache_file = STATUS_RUN_DIR .. "/location_cache/" .. server .. ".txt"
        local location = _read_file(cache_file)

        if location then
            local expired = false
            if nixfs then
                local attr = nixfs.stat(cache_file)
                if attr and (os.time() - (attr.mtime or 0)) >= 300 then
                    expired = true
                end
            end
            if not expired then
                loc_memo[server] = location
                return location
            end
        end

        -- 缓存 miss/过期：调用 iplocation.sh
        local safe = server:gsub("[^%w%.%-]", "")
        if safe ~= "" then
            local result = _read_cmd("/usr/lib/proxypool/iplocation.sh " .. safe .. " 2>/dev/null")
            result = result and result:match("^%s*(.-)%s*$") or ""
            if result ~= "" then
                loc_memo[server] = result
                return result
            end
        end

        loc_memo[server] = ""
        return ""
    end

    -- 单次 UCI 遍历（C 绑定零 fork）：构建 clients + devices
    local clients = {}
    local devices_list = {}
    local total, enabled_count, connected_count = 0, 0, 0

    uci_c:foreach("proxypool", "client", function(s)
        local cid = s[".name"]
        local enabled = s.enabled or "0"
        local ctype = s.type or ""
        local server = s.server or ""
        local status = "offline"
        local ip_addr = ""

        if enabled == "1" then
            if ctype == "socks5" then
                -- SOCKS5: PID 文件 + 探测缓存（零 fork）
                local pid = _read_file(STATUS_RUN_DIR .. "/redsocks/" .. cid .. ".pid")
                if pid and _process_alive(pid) then
                    local probe = _read_file(STATUS_RUN_DIR .. "/probe/" .. cid)
                    if probe == "ok" then status = "connected"
                    elseif probe then status = "disconnected"
                    else status = "disconnected" end  -- 无缓存，保守显示 disconnected
                else
                    status = "disconnected"
                end

            elseif ctype == "slp" then
                -- SLP: 双进程检测（slp-client + redsocks，零 fork）
                local slp_pid = _read_file(STATUS_RUN_DIR .. "/slp/" .. cid .. "/slp.pid")
                if slp_pid and _process_alive(slp_pid) then
                    local rs_pid = _read_file(STATUS_RUN_DIR .. "/redsocks/" .. cid .. ".pid")
                    if rs_pid and _process_alive(rs_pid) then
                        local probe = _read_file(STATUS_RUN_DIR .. "/probe/" .. cid)
                        if probe == "ok" then status = "connected"
                        elseif probe then status = "disconnected"
                        else status = "disconnected" end  -- 无缓存，保守显示 disconnected
                    else
                        status = "connecting"
                    end
                else
                    status = "disconnected"
                end

            elseif ctype == "l2tp" then
                -- L2TP: 零 fork 检测 PPP 接口 + 探测缓存（与 SOCKS5/SLP 一致）
                -- 通过 /sys/class/net/ppp-$cid 判断接口是否存在（零 fork）
                local ppp_iface = "ppp-" .. cid
                local iface_exists = false
                local f_iface = io.open("/sys/class/net/" .. ppp_iface .. "/operstate", "r")
                if f_iface then
                    f_iface:close()
                    iface_exists = true
                end
                if iface_exists then
                    -- 读取 PPP 接口 IP（通过 /proc/net/fib_trie 太复杂，保留一次轻量 fork）
                    local ip_out = _read_cmd("ip -4 addr show " .. ppp_iface .. " 2>/dev/null")
                    ip_addr = ip_out and ip_out:match("inet (%d+%.%d+%.%d+%.%d+)") or ""
                    if ip_addr ~= "" then
                        -- PPP 接口拿到 IP 即视为已连接（对齐 netifd ifstatus .up）。
                        -- 不再要求即时 curl 探测 ok：L2TP 拨通需数秒，连接瞬间探测必失败
                        -- 会把缓存写成 fail 导致误判"未连接"。隧道连通性由后台 watchdog
                        -- 定期 ping 网关检测并自动重连。
                        status = "connected"
                    else
                        status = "connecting"  -- 接口存在但无 IP（PPP 协商中）
                    end
                else
                    -- PPP 接口不存在，检查 xl2tpd 是否在运行
                    local xl2tpd_pid = _read_file("/var/run/proxypool/l2tp/" .. cid .. "/xl2tpd.pid")
                    if xl2tpd_pid and _process_alive(xl2tpd_pid) then
                        status = "connecting"
                    else
                        status = "disconnected"
                    end
                end
            else
                status = "disconnected"
            end
        else
            status = "disabled"
        end

        -- 检查 pending 操作状态文件：控制器在触发异步操作时写入，
        -- 让 status API 在操作完成前返回过渡状态（disconnecting/connecting）
        local pending_file = STATUS_RUN_DIR .. "/pending/" .. cid
        local pending_op = _read_file(pending_file)
        if pending_op then
            -- 超时检测：30 秒未完成视为过期（防止残留文件永久卡住状态）
            local pending_expired = false
            if nixfs then
                local attr = nixfs.stat(pending_file)
                if not attr or (os.time() - (attr.mtime or 0)) >= 30 then
                    pending_expired = true
                end
            else
                -- nixfs 不可用回退：读文件内容中的时间戳，或直接用 30s 安全窗口
                -- pending 文件格式扩展为 "op:timestamp"，兼容旧格式（纯 op 字符串）
                local parts = pending_op:match(":(%d+)$")
                if parts then
                    local ts = tonumber(parts)
                    if ts and (os.time() - ts) >= 30 then
                        pending_expired = true
                    end
                    pending_op = pending_op:match("^(.+):%d+$") or pending_op
                end
            end
            if pending_expired then
                pending_op = nil
            elseif pending_op == "stopping" then
                if status == "disconnected" or status == "disabled" then
                    pending_op = nil
                else
                    status = "disconnecting"
                end
            elseif pending_op == "starting" then
                if status == "connected" then
                    pending_op = nil
                else
                    status = "connecting"
                end
            end
        end

        -- bind_ips 规范化
        local bind_ip = s.bind_ip or {}
        if type(bind_ip) == "string" then bind_ip = { bind_ip } end

        -- 流量统计：持久化累加值 + nft 当前值
        local rx, tx = 0, 0
        for _, bip in ipairs(bind_ip) do
            local saved_out = tonumber(_read_file(STATUS_RUN_DIR .. "/counters/" .. bip .. ".out")) or 0
            local saved_in = tonumber(_read_file(STATUS_RUN_DIR .. "/counters/" .. bip .. ".in")) or 0
            tx = tx + saved_out + (out_ctr[bip] or 0)
            rx = rx + saved_in + (in_ctr[bip] or 0)
        end

        -- 超时计数
        local timeout_today = tonumber(_read_file(STATUS_RUN_DIR .. "/timeout/" .. cid .. ".today")) or 0
        local timeout_yesterday = tonumber(_read_file(STATUS_RUN_DIR .. "/timeout/" .. cid .. ".yesterday")) or 0

        total = total + 1
        if enabled == "1" then enabled_count = enabled_count + 1 end
        if status == "connected" then connected_count = connected_count + 1 end

        clients[#clients + 1] = {
            id = cid,
            name = s.name or cid,
            type = ctype,
            server = server,
            endpoint_resolution = _endpoint_resolution_status(server),
            port = s.port or "",
            username = s.username or "",
            password = s.password or "",
            expiry = s.expiry or "",
            remark = s.remark or "",
            location = get_location(server),
            enabled = tonumber(enabled) or 0,
            status = status,
            ip_addr = ip_addr,
            bind_ips = as_array(bind_ip),
            rx_bytes = rx,
            tx_bytes = tx,
            rx_human = _format_bytes(rx),
            tx_human = _format_bytes(tx),
            timeout_today = timeout_today,
            timeout_yesterday = timeout_yesterday
        }

        -- 同时构建 devices 列表（避免二次 UCI 遍历）
        local client_name = s.name or cid
        for _, ip in ipairs(bind_ip) do
            local neigh = neigh_table[ip]
            local mac = ""
            local online = false
            if neigh then
                mac = neigh.mac
                if mac ~= "" and mac ~= "FAILED" then
                    local st = neigh.state
                    if st == "REACHABLE" or st == "DELAY" or st == "PROBE" then
                        online = true
                    end
                end
            end
            devices_list[#devices_list + 1] = {
                ip = ip,
                mac = mac,
                online = online,
                client = cid,
                client_name = client_name
            }
        end
    end)

    local disconnected = enabled_count - connected_count
    if disconnected < 0 then disconnected = 0 end

    return jsonc.stringify({
        timestamp = os.time(),
        datetime = os.date("%Y-%m-%d %H:%M:%S"),
        global_enabled = global_enabled,
        dns_path_status = DNS_PATH_UNAVAILABLE,
        internet_ready = false,
        summary = {
            total = total,
            enabled = enabled_count,
            connected = connected_count,
            disconnected = disconnected
        },
        network = get_wan_status(),
        clients = as_array(clients),
        devices = as_array(devices_list)
    })
end

function api_handler()
    local http = require "luci.http"
    local sys = require "luci.sys"
    local uci = require "luci.model.uci".cursor()
    local json = require "luci.jsonc"

    local action = http.formvalue("action") or ""

    if LEGACY_MUTATION_ACTIONS[action] then
        return reject_legacy_api(action)
    end

    local function reject_dns_unavailable()
        http.prepare_content("application/json")
        http.write('{"success":false,"dns_path_status":"dns_path_unavailable","internet_ready":false,"error":"dns_path_unavailable"}')
    end

    if action == "status" then
        -- 使用期限到期：不返回任何客户端数据，前端保持"正在加载中"
        if lease_expired() then
            http.prepare_content("application/json")
            http.write('{"locked":true,"dns_path_status":"dns_path_unavailable","internet_ready":false}')
            return
        end
        http.prepare_content("application/json")
        local ok, result = pcall(generate_status)
        if ok and result then
            http.write(result)
        else
            -- generate_status 出错：返回兜底 JSON + 记录日志
            http.write('{"timestamp":0,"datetime":"","global_enabled":1,"dns_path_status":"dns_path_unavailable","internet_ready":false,"summary":{"total":0,"enabled":0,"connected":0,"disconnected":0},"clients":[],"devices":[],"error":"Lua status generation failed"}')
        end
        -- probe_all 已从 status 端点移除：每次轮询都 fork 50+ 探测进程会导致 CGI worker 饱和
        -- 探测改为独立触发（见 probe_all action），前端按需调度

    elseif action == "get_client" then
        local client = sanitize_client(http.formvalue("client"))
        if client then
            local data = {}
            data.id = client
            data.enabled = uci:get("proxypool", client, "enabled") or "0"
            data.name = uci:get("proxypool", client, "name") or ""
            data.type = uci:get("proxypool", client, "type") or "socks5"
            data.server = uci:get("proxypool", client, "server") or ""
            data.port = uci:get("proxypool", client, "port") or ""
            data.username = uci:get("proxypool", client, "username") or ""
            data.password = uci:get("proxypool", client, "password") or ""
            data.expiry = uci:get("proxypool", client, "expiry") or ""
            data.bind_ip = uci:get("proxypool", client, "bind_ip") or {}
            if type(data.bind_ip) == "string" then
                data.bind_ip = {data.bind_ip}
            end
            -- SLP 专用字段
            data.slp_token = uci:get("proxypool", client, "slp_token") or ""
            data.slp_transport = uci:get("proxypool", client, "slp_transport") or "quic"
            data.slp_obfs = uci:get("proxypool", client, "slp_obfs") or "0"
            data.slp_obfs_key = uci:get("proxypool", client, "slp_obfs_key") or ""
            data.slp_insecure = uci:get("proxypool", client, "slp_insecure") or "1"
            http.prepare_content("application/json")
            http.write(json.stringify(data))
        else
            http.prepare_content("application/json")
            http.write('{"error": "Invalid client ID"}')
        end

    elseif action == "save_client" then
        local client = sanitize_client(http.formvalue("client"))
        local data = http.formvalue("data")
        if client and data then
            local d = json.parse(data)
            if d then
                if tostring(d.enabled or "0") == "1" then
                    reject_dns_unavailable(); return
                end
                -- 去除连接字段首尾空格（防止复制粘贴带入空格导致连接失败）
                local function trim(s)
                    if type(s) ~= "string" then return s end
                    return (s:gsub("^%s+", ""):gsub("%s+$", ""))
                end
                d.server = trim(d.server)
                d.port = trim(d.port)
                d.username = trim(d.username)
                d.password = trim(d.password)
                uci:set("proxypool", client, "client")
                uci:set("proxypool", client, "enabled", d.enabled or "0")
                uci:set("proxypool", client, "name", d.name or "")
                uci:set("proxypool", client, "type", d.type or "socks5")
                uci:set("proxypool", client, "server", d.server or "")
                uci:set("proxypool", client, "port", d.port or "")
                uci:set("proxypool", client, "username", d.username or "")
                uci:set("proxypool", client, "password", d.password or "")
                if d.expiry and d.expiry ~= "" then
                    uci:set("proxypool", client, "expiry", d.expiry)
                else
                    uci:delete("proxypool", client, "expiry")
                end
                if d.bind_ip and #d.bind_ip > 0 then
                    uci:set("proxypool", client, "bind_ip", d.bind_ip)
                else
                    uci:delete("proxypool", client, "bind_ip")
                end
                -- SLP 专用字段
                if d.type == "slp" then
                    uci:set("proxypool", client, "slp_token", d.slp_token or "")
                    uci:set("proxypool", client, "slp_transport", d.slp_transport or "quic")
                    uci:set("proxypool", client, "slp_obfs", d.slp_obfs or "0")
                    uci:set("proxypool", client, "slp_obfs_key", d.slp_obfs_key or "")
                    uci:set("proxypool", client, "slp_insecure", d.slp_insecure or "1")
                else
                    -- 非 SLP 类型，清理 SLP 字段
                    uci:delete("proxypool", client, "slp_token")
                    uci:delete("proxypool", client, "slp_transport")
                    uci:delete("proxypool", client, "slp_obfs")
                    uci:delete("proxypool", client, "slp_obfs_key")
                    uci:delete("proxypool", client, "slp_insecure")
                end
                uci:commit("proxypool")
                -- 保存后异步应用：立即返回，后台重启/停止（L2TP 拨号不阻塞请求）。
                if d.enabled == "1" then
                    set_pending_op(client, "starting")
                    os.execute("setsid /usr/lib/proxypool/proxypool.sh save_restart_client " .. client .. " >/dev/null 2>&1 &")
                else
                    set_pending_op(client, "stopping")
                    os.execute("setsid /usr/lib/proxypool/proxypool.sh stop_client " .. client .. " >/dev/null 2>&1 &")
                end
                http.prepare_content("application/json")
                http.write('{"success": true}')
            else
                http.prepare_content("application/json")
                http.write('{"error": "Invalid JSON data"}')
            end
        else
            http.prepare_content("application/json")
            http.write('{"error": "Missing client or data"}')
        end

    elseif action == "delete_client" then
        local client = sanitize_client(http.formvalue("client"))
        if client then
            -- 同步停止客户端进程 + 移除防火墙规则，再删 UCI
            os.execute("/usr/lib/proxypool/proxypool.sh stop_client " .. client .. " >/dev/null 2>&1")
            uci:delete("proxypool", client)
            uci:commit("proxypool")
            http.prepare_content("application/json")
            http.write('{"success": true}')
        else
            http.prepare_content("application/json")
            http.write('{"error": "Invalid client ID"}')
        end

    elseif action == "toggle_client" then
        local client = sanitize_client(http.formvalue("client"))
        local enabled = http.formvalue("enabled")
        if client and enabled then
            if enabled == "1" then
                reject_dns_unavailable(); return
            end
            uci:set("proxypool", client, "enabled", enabled == "1" and "1" or "0")
            uci:commit("proxypool")
            -- 异步切换：立即返回，后台 start/stop（不阻塞请求）。
            set_pending_op(client, enabled == "1" and "starting" or "stopping")
            os.execute("setsid /usr/lib/proxypool/proxypool.sh toggle_client " .. client .. " >/dev/null 2>&1 &")
            http.prepare_content("application/json")
            http.write('{"success": true}')
        else
            http.prepare_content("application/json")
            http.write('{"error": "Invalid client or enabled parameter"}')
        end

    elseif action == "start_client" then
        local client = sanitize_client(http.formvalue("client"))
        if client then
            reject_dns_unavailable(); return
        else
            http.prepare_content("application/json")
            http.write('{"error": "Invalid client ID"}')
        end

    elseif action == "stop_client" then
        local client = sanitize_client(http.formvalue("client"))
        if client then
            -- 异步停止：立即返回，后台断开（不再阻塞请求）。
            -- 写 stopping 标记，status API 在断开完成前返回 disconnecting 过渡态。
            set_pending_op(client, "stopping")
            os.execute("setsid /usr/lib/proxypool/proxypool.sh stop_client " .. client .. " >/dev/null 2>&1 &")
            http.prepare_content("application/json")
            http.write('{"success": true}')
        else
            http.prepare_content("application/json")
            http.write('{"error": "Invalid client ID"}')
        end

    elseif action == "save_remark" then
        local client = sanitize_client(http.formvalue("client"))
        local remark = http.formvalue("remark") or ""
        if client then
            if remark ~= "" then
                uci:set("proxypool", client, "remark", remark)
            else
                uci:delete("proxypool", client, "remark")
            end
            uci:commit("proxypool")
            http.prepare_content("application/json")
            http.write('{"success": true}')
        else
            http.prepare_content("application/json")
            http.write('{"error": "Invalid client ID"}')
        end

    elseif action == "restart_client" then
        local client = sanitize_client(http.formvalue("client"))
        if client then
            reject_dns_unavailable(); return
        else
            http.prepare_content("application/json")
            http.write('{"error": "Invalid client ID"}')
        end

    elseif action == "get_dhcp_lease" then
        http.prepare_content("application/json")
        local dhcp_section = _find_unique_lan_dhcp_section(uci)
        if not dhcp_section then
            http.write('{"success":false,"dns_path_status":"dns_path_unavailable","internet_ready":false,"error":"DHCP LAN section missing or ambiguous"}')
        else
            local leasetime = uci:get("dhcp", dhcp_section, "leasetime") or "7d"
            http.write(json.stringify({success = true, leasetime = leasetime}))
        end

    elseif action == "set_dhcp_lease" then
        local leasetime = http.formvalue("leasetime") or "7d"
        -- 验证格式（允许数字+d 或 infinite）
        http.prepare_content("application/json")
        if leasetime:match("^%d+d$") or leasetime == "infinite" then
            local dhcp_section = _find_unique_lan_dhcp_section(uci)
            if not dhcp_section then
                http.write('{"success":false,"dns_path_status":"dns_path_unavailable","internet_ready":false,"error":"DHCP LAN section missing or ambiguous"}')
            elseif not uci:set("dhcp", dhcp_section, "leasetime", leasetime) then
                http.write('{"success":false,"dns_path_status":"dns_path_unavailable","internet_ready":false,"error":"DHCP lease update failed"}')
            elseif not uci:commit("dhcp") then
                uci:revert("dhcp")
                http.write('{"success":false,"dns_path_status":"dns_path_unavailable","internet_ready":false,"error":"DHCP commit failed"}')
            elseif sys.call("/usr/lib/proxypool/dns-manager.sh enforce-unavailable >/dev/null 2>&1") ~= 0 then
                http.write('{"success":false,"dns_path_status":"dns_path_unavailable","internet_ready":false,"error":"DNS fail-closed verification failed"}')
            else
                -- success means the lease setting was applied and dnsmasq was
                -- safely converged; Phase 1 Internet DNS remains unavailable.
                http.write('{"success":true,"dns_path_status":"dns_path_unavailable","internet_ready":false}')
            end
        else
            http.write('{"error": "Invalid leasetime format"}')
        end

    elseif action == "reload" then
        reject_dns_unavailable(); return

    elseif action == "probe_all" then
        -- 后台探测：setsid 创建新会话，脱离 uhttpd CGI 进程组（防止被杀）
        -- nohup 在 busybox 上不可靠，setsid 是 POSIX 标准且 busybox 自带
        os.execute("setsid /usr/lib/proxypool/proxypool.sh probe_all >/dev/null 2>&1 &")
        http.prepare_content("application/json")
        http.write('{"success": true}')

    elseif action == "log" then
        local lines = tonumber(http.formvalue("lines")) or 100
        -- 限制最大行数防止滥用
        if lines > 1000 then lines = 1000 end
        local result = sys.exec("tail -n " .. lines .. " /var/log/proxypool.log 2>/dev/null || echo '暂无日志'")
        http.prepare_content("text/plain; charset=utf-8")
        http.write(result)

    elseif action == "clear_log" then
        sys.exec("> /var/log/proxypool.log")
        http.prepare_content("application/json")
        http.write('{"success": true}')

    elseif action == "syslog" then
        -- 系统日志（logread）：含 xl2tpd/pppd 拨号细节，用于排查 L2TP 连不上。
        -- filter 用固定白名单匹配（不接受用户输入拼接命令，避免注入）。
        local lines = tonumber(http.formvalue("lines")) or 200
        if lines > 1000 then lines = 1000 end
        local filter = http.formvalue("filter") or "all"
        local cmd
        if filter == "l2tp" then
            cmd = "logread 2>/dev/null | grep -iE 'l2tp|pppd|ppp[0-9]|chap| pap|lcp|ipcp|xl2tpd|peer' | tail -n " .. lines
        else
            cmd = "logread 2>/dev/null | tail -n " .. lines
        end
        local result = sys.exec(cmd)
        http.prepare_content("text/plain; charset=utf-8")
        http.write((result and result ~= "") and result or "暂无系统日志")

    elseif action == "batch_import" then
        -- 批量导入：接收 JSON 数组，逐条写入 UCI，单次 commit
        -- 导入后自动逐个启动已启用的客户端（后台，与手动点"连接"体验一致）
        local raw = http.formvalue("data")
        if raw then
            local items = json.parse(raw)
            if items and type(items) == "table" then
                for _, d in ipairs(items) do
                    if sanitize_client(d.id) and tostring(d.enabled or "1") == "1" then
                        reject_dns_unavailable(); return
                    end
                end
                local imported = 0
                local enabled_ids = {}
                for _, d in ipairs(items) do
                    local cid = sanitize_client(d.id)
                    if cid then
                        local en = d.enabled or "1"
                        uci:set("proxypool", cid, "client")
                        uci:set("proxypool", cid, "enabled", en)
                        uci:set("proxypool", cid, "name", d.name or "")
                        uci:set("proxypool", cid, "type", d.type or "socks5")
                        uci:set("proxypool", cid, "server", d.server or "")
                        uci:set("proxypool", cid, "port", d.port or "")
                        uci:set("proxypool", cid, "username", d.username or "")
                        uci:set("proxypool", cid, "password", d.password or "")
                        if d.expiry and d.expiry ~= "" then
                            uci:set("proxypool", cid, "expiry", d.expiry)
                        end
                        if d.bind_ip and type(d.bind_ip) == "table" and #d.bind_ip > 0 then
                            uci:set("proxypool", cid, "bind_ip", d.bind_ip)
                        end
                        imported = imported + 1
                        if en == "1" then
                            enabled_ids[#enabled_ids + 1] = cid
                        end
                    end
                end
                uci:commit("proxypool")
                -- 不在后端启动：返回 enabled_ids 给前端，前端逐个调用 start_client（同步路径）
                http.prepare_content("application/json")
                local ids_json = "[]"
                if #enabled_ids > 0 then
                    ids_json = '["' .. table.concat(enabled_ids, '","') .. '"]'
                end
                http.write('{"success": true, "imported": ' .. imported .. ', "auto_start": ' .. #enabled_ids .. ', "auto_start_ids": ' .. ids_json .. '}')
            else
                http.prepare_content("application/json")
                http.write('{"error": "Invalid JSON data"}')
            end
        else
            http.prepare_content("application/json")
            http.write('{"error": "No data provided"}')
        end

    elseif action == "batch_action" then
        -- 批量操作：enable/disable/delete/connect/disconnect
        -- 启用/连接：前端逐个调用 start_client（已改异步，秒回）
        -- 停用/断开：异步执行 + 写 stopping 标记（避免串行 stop 阻塞 10+ 秒）
        -- 删除：同步执行（需停完再删 UCI，避免竞态）
        local batch_action = http.formvalue("batch_action") or ""
        local raw_clients = http.formvalue("clients")
        if raw_clients then
            local client_list = json.parse(raw_clients)
            if client_list and type(client_list) == "table" then
                -- 清洗 ID 列表
                local clean_ids = {}
                for _, cid in ipairs(client_list) do
                    local clean = sanitize_client(cid)
                    if clean then
                        clean_ids[#clean_ids + 1] = clean
                    end
                end
                local id_str = table.concat(clean_ids, " ")
                local processed = #clean_ids

                if processed > 0 and (batch_action == "enable" or batch_action == "connect") then
                    reject_dns_unavailable(); return
                end

                if batch_action == "enable" then
                    for _, clean in ipairs(clean_ids) do
                        uci:set("proxypool", clean, "enabled", "1")
                    end
                    uci:commit("proxypool")
                    -- UCI 已设置 enabled=1，前端逐个调用 start_client 启动（同步路径）
                elseif batch_action == "disable" then
                    for _, clean in ipairs(clean_ids) do
                        uci:set("proxypool", clean, "enabled", "0")
                        set_pending_op(clean, "stopping")
                    end
                    uci:commit("proxypool")
                    os.execute("setsid /usr/lib/proxypool/proxypool.sh batch_disable " .. id_str .. " >/dev/null 2>&1 &")
                elseif batch_action == "delete" then
                    for _, clean in ipairs(clean_ids) do
                        uci:delete("proxypool", clean)
                    end
                    uci:commit("proxypool")
                    os.execute("/usr/lib/proxypool/proxypool.sh batch_delete " .. id_str .. " >/dev/null 2>&1")
                elseif batch_action == "connect" then
                    -- 前端逐个调用 start_client 启动（同步路径，此处无需操作）
                elseif batch_action == "disconnect" then
                    for _, clean in ipairs(clean_ids) do
                        set_pending_op(clean, "stopping")
                    end
                    os.execute("setsid /usr/lib/proxypool/proxypool.sh batch_disconnect " .. id_str .. " >/dev/null 2>&1 &")
                end
                http.prepare_content("application/json")
                http.write('{"success": true, "processed": ' .. processed .. '}')
            else
                http.prepare_content("application/json")
                http.write('{"error": "Invalid clients data"}')
            end
        else
            http.prepare_content("application/json")
            http.write('{"error": "No clients provided"}')
        end

    elseif action == "export_all" then
        -- 导出所有客户端（含密码）
        local clients = {}
        uci:foreach("proxypool", "client", function(s)
            local item = {
                id = s[".name"] or "",
                name = s.name or "",
                type = s.type or "",
                server = s.server or "",
                port = s.port or "",
                username = s.username or "",
                password = s.password or "",
                expiry = s.expiry or "",
                enabled = s.enabled or "0"
            }
            if s.bind_ip then
                if type(s.bind_ip) == "table" then
                    item.bind_ip = s.bind_ip
                else
                    item.bind_ip = {s.bind_ip}
                end
            else
                item.bind_ip = {}
            end
            clients[#clients + 1] = item
        end)
        http.prepare_content("application/json")
        http.write(json.stringify(clients))

    elseif action == "backup_create" then
        local file = "/tmp/proxypool_backup_" .. os.time() .. ".tar.gz"
        sys.exec("/usr/lib/proxypool/backup.sh create " .. file .. " 2>/dev/null")
        http.prepare_content("application/json")
        http.write('{"success": true, "file": "' .. file .. '"}')

    else
        http.prepare_content("application/json")
        http.write('{"error": "Unknown action"}')
    end
end
