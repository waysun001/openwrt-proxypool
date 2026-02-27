-- 智联盒子 LuCI Controller
module("luci.controller.proxypool", package.seeall)

function index()
    entry({"admin", "services", "proxypool"}, template("proxypool/main"), _("智联盒子"), 1).dependent = false
    entry({"admin", "services", "proxypool", "api"}, call("api_handler")).leaf = true
    
    -- 设置为默认首页：创建 admin 根路由重定向
    entry({"admin"}, alias("admin", "services", "proxypool"), _("Administration"), 10).index = true
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

-- 生成完整状态 JSON
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
                os.execute("mkdir -p '" .. STATUS_RUN_DIR .. "/location_cache' 2>/dev/null")
                local wf = io.open(cache_file, "w")
                if wf then wf:write(result); wf:close() end
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
                        -- PPP 接口存活且有 IP，读探测缓存判断实际连通性
                        local probe = _read_file(STATUS_RUN_DIR .. "/probe/" .. cid)
                        if probe == "ok" then status = "connected"
                        elseif probe then status = "disconnected"
                        else status = "disconnected" end  -- 无缓存，保守显示 disconnected
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
                os.remove(pending_file)
            elseif pending_op == "stopping" then
                if status == "disconnected" or status == "disabled" then
                    os.remove(pending_file)  -- 操作已完成
                else
                    status = "disconnecting"
                end
            elseif pending_op == "starting" then
                if status == "connected" then
                    os.remove(pending_file)  -- 操作已完成
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
        summary = {
            total = total,
            enabled = enabled_count,
            connected = connected_count,
            disconnected = disconnected
        },
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

    if action == "status" then
        http.prepare_content("application/json")
        local ok, result = pcall(generate_status)
        if ok and result then
            http.write(result)
        else
            -- generate_status 出错：返回兜底 JSON + 记录日志
            local err_msg = not ok and tostring(result) or "generate_status returned nil"
            local log_f = io.open("/var/log/proxypool.log", "a")
            if log_f then
                log_f:write("[" .. os.date("%Y-%m-%d %H:%M:%S") .. "] Lua status error: " .. err_msg .. "\n")
                log_f:close()
            end
            http.write('{"timestamp":0,"datetime":"","global_enabled":1,"summary":{"total":0,"enabled":0,"connected":0,"disconnected":0},"clients":[],"devices":[],"error":"Lua status generation failed"}')
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
                -- 保存后同步应用（所有类型统一走 shell 脚本，单一权威实现）
                if d.enabled == "1" then
                    os.execute("/usr/lib/proxypool/proxypool.sh save_restart_client " .. client .. " >/dev/null 2>&1")
                else
                    os.execute("/usr/lib/proxypool/proxypool.sh stop_client " .. client .. " >/dev/null 2>&1")
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
            uci:set("proxypool", client, "enabled", enabled == "1" and "1" or "0")
            uci:commit("proxypool")
            -- 同步切换（所有类型统一，shell 脚本处理 start/stop + 增量防火墙）
            os.execute("/usr/lib/proxypool/proxypool.sh toggle_client " .. client .. " >/dev/null 2>&1")
            http.prepare_content("application/json")
            http.write('{"success": true}')
        else
            http.prepare_content("application/json")
            http.write('{"error": "Invalid client or enabled parameter"}')
        end

    elseif action == "start_client" then
        local client = sanitize_client(http.formvalue("client"))
        if client then
            -- 同步启动（shell 脚本处理进程启动 + 增量防火墙 add_client）
            os.execute("/usr/lib/proxypool/proxypool.sh start_client " .. client .. " >/dev/null 2>&1")
            http.prepare_content("application/json")
            http.write('{"success": true}')
        else
            http.prepare_content("application/json")
            http.write('{"error": "Invalid client ID"}')
        end

    elseif action == "stop_client" then
        local client = sanitize_client(http.formvalue("client"))
        if client then
            -- 同步停止（shell 脚本处理 mark_stopping + 移除防火墙 + kill 进程）
            os.execute("/usr/lib/proxypool/proxypool.sh stop_client " .. client .. " >/dev/null 2>&1")
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
            -- 同步重启（shell 脚本处理 stop + start + 增量防火墙）
            os.execute("/usr/lib/proxypool/proxypool.sh restart_client " .. client .. " >/dev/null 2>&1")
            http.prepare_content("application/json")
            http.write('{"success": true}')
        else
            http.prepare_content("application/json")
            http.write('{"error": "Invalid client ID"}')
        end

    elseif action == "get_dhcp_lease" then
        local leasetime = uci:get("dhcp", "lan", "leasetime") or "7d"
        http.prepare_content("application/json")
        http.write(json.stringify({leasetime = leasetime}))

    elseif action == "set_dhcp_lease" then
        local leasetime = http.formvalue("leasetime") or "7d"
        -- 验证格式（允许数字+d 或 infinite）
        if leasetime:match("^%d+d$") or leasetime == "infinite" then
            uci:set("dhcp", "lan", "leasetime", leasetime)
            uci:commit("dhcp")
            sys.exec("/etc/init.d/dnsmasq restart >/dev/null 2>&1")
            http.prepare_content("application/json")
            http.write('{"success": true}')
        else
            http.prepare_content("application/json")
            http.write('{"error": "Invalid leasetime format"}')
        end

    elseif action == "reload" then
        sys.exec("/usr/lib/proxypool/proxypool.sh reload 2>/dev/null")
        http.prepare_content("application/json")
        http.write('{"success": true}')

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

    elseif action == "batch_import" then
        -- 批量导入：接收 JSON 数组，逐条写入 UCI，单次 commit
        -- 导入后自动逐个启动已启用的客户端（后台，与手动点"连接"体验一致）
        local raw = http.formvalue("data")
        if raw then
            local items = json.parse(raw)
            if items and type(items) == "table" then
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
        -- 启用/连接：后台逐个启动（和手动点"连接"一致，每个都有同步探测）
        -- 停用/断开/删除：同步执行（停止操作速度快，无需后台）
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

                if batch_action == "enable" then
                    for _, clean in ipairs(clean_ids) do
                        uci:set("proxypool", clean, "enabled", "1")
                    end
                    uci:commit("proxypool")
                    -- UCI 已设置 enabled=1，前端逐个调用 start_client 启动（同步路径）
                elseif batch_action == "disable" then
                    for _, clean in ipairs(clean_ids) do
                        uci:set("proxypool", clean, "enabled", "0")
                    end
                    uci:commit("proxypool")
                    os.execute("/usr/lib/proxypool/proxypool.sh batch_disable " .. id_str .. " >/dev/null 2>&1")
                elseif batch_action == "delete" then
                    for _, clean in ipairs(clean_ids) do
                        uci:delete("proxypool", clean)
                    end
                    uci:commit("proxypool")
                    os.execute("/usr/lib/proxypool/proxypool.sh batch_delete " .. id_str .. " >/dev/null 2>&1")
                elseif batch_action == "connect" then
                    -- 前端逐个调用 start_client 启动（同步路径，此处无需操作）
                elseif batch_action == "disconnect" then
                    os.execute("/usr/lib/proxypool/proxypool.sh batch_disconnect " .. id_str .. " >/dev/null 2>&1")
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
