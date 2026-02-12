-- 智联盒子 LuCI Controller
module("luci.controller.proxypool", package.seeall)

function index()
    -- 设置为默认首页
    entry({"admin"}, alias("admin", "services", "proxypool"), _("Administration"), 10).index = true
    
    entry({"admin", "services", "proxypool"}, template("proxypool/main"), _("智联盒子"), 10).dependent = false
    entry({"admin", "services", "proxypool", "api"}, call("api_handler")).leaf = true
end

-- 输入清洗：只允许字母数字下划线（防止命令注入）
local function sanitize_client(raw)
    if not raw then return nil end
    local clean = raw:match("^([%w_]+)$")
    return clean
end

function api_handler()
    local http = require "luci.http"
    local sys = require "luci.sys"
    local uci = require "luci.model.uci".cursor()
    local json = require "luci.jsonc"

    local action = http.formvalue("action") or ""

    if action == "status" then
        local result = sys.exec("/usr/lib/proxypool/status.sh get 2>/dev/null")
        http.prepare_content("application/json")
        http.write(result)

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
            http.prepare_content("application/json")
            http.write(json.stringify(data))
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
                uci:commit("proxypool")
                -- 保存后自动应用：先停止旧的，再根据 enabled 决定是否启动
                -- 注意：分开调用 stop 和 start，模拟手动禁用→启用的流程
                if d.enabled == "1" then
                    sys.exec("/usr/lib/proxypool/proxypool.sh save_restart_client " .. client .. " >/dev/null 2>&1 &")
                else
                    sys.exec("/usr/lib/proxypool/proxypool.sh stop_client " .. client .. " >/dev/null 2>&1 &")
                end
                http.prepare_content("application/json")
                http.write('{"success": true}')
            end
        end

    elseif action == "delete_client" then
        local client = sanitize_client(http.formvalue("client"))
        if client then
            sys.exec("/usr/lib/proxypool/proxypool.sh stop_client " .. client .. " 2>/dev/null")
            uci:delete("proxypool", client)
            uci:commit("proxypool")
            http.prepare_content("application/json")
            http.write('{"success": true}')
        end

    elseif action == "toggle_client" then
        local client = sanitize_client(http.formvalue("client"))
        local enabled = http.formvalue("enabled")
        if client and enabled then
            -- 保存 enabled 状态到 UCI
            uci:set("proxypool", client, "enabled", enabled == "1" and "1" or "0")
            uci:commit("proxypool")
            -- 调用 proxypool.sh toggle_client 执行实际的 stop/start + firewall rebuild
            sys.exec("/usr/lib/proxypool/proxypool.sh toggle_client " .. client .. " 2>/dev/null")
            http.prepare_content("application/json")
            http.write('{"success": true}')
        end

    elseif action == "start_client" then
        local client = sanitize_client(http.formvalue("client"))
        if client then
            sys.exec("/usr/lib/proxypool/proxypool.sh start_client " .. client .. " 2>/dev/null")
            http.prepare_content("application/json")
            http.write('{"success": true}')
        end

    elseif action == "stop_client" then
        local client = sanitize_client(http.formvalue("client"))
        if client then
            sys.exec("/usr/lib/proxypool/proxypool.sh stop_client " .. client .. " 2>/dev/null")
            http.prepare_content("application/json")
            http.write('{"success": true}')
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
        end

    elseif action == "restart_client" then
        local client = sanitize_client(http.formvalue("client"))
        if client then
            sys.exec("/usr/lib/proxypool/proxypool.sh restart_client " .. client .. " 2>/dev/null")
            http.prepare_content("application/json")
            http.write('{"success": true}')
        end

    elseif action == "get_dhcp_lease" then
        local leasetime = uci:get("dhcp", "lan", "leasetime") or "7d"
        http.prepare_content("application/json")
        http.write('{"leasetime": "' .. leasetime .. '"}')

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
        local raw = http.formvalue("data")
        if raw then
            local items = json.parse(raw)
            if items and type(items) == "table" then
                local imported = 0
                for _, d in ipairs(items) do
                    local cid = sanitize_client(d.id)
                    if cid then
                        uci:set("proxypool", cid, "client")
                        uci:set("proxypool", cid, "enabled", d.enabled or "1")
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
                    end
                end
                uci:commit("proxypool")
                http.prepare_content("application/json")
                http.write('{"success": true, "imported": ' .. imported .. '}')
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
        local batch_action = http.formvalue("batch_action") or ""
        local raw_clients = http.formvalue("clients")
        if raw_clients then
            local client_list = json.parse(raw_clients)
            if client_list and type(client_list) == "table" then
                local processed = 0
                if batch_action == "enable" or batch_action == "disable" then
                    local val = (batch_action == "enable") and "1" or "0"
                    for _, cid in ipairs(client_list) do
                        local clean = sanitize_client(cid)
                        if clean then
                            uci:set("proxypool", clean, "enabled", val)
                            processed = processed + 1
                        end
                    end
                    uci:commit("proxypool")
                    for _, cid in ipairs(client_list) do
                        local clean = sanitize_client(cid)
                        if clean then
                            sys.exec("/usr/lib/proxypool/proxypool.sh toggle_client " .. clean .. " 2>/dev/null")
                        end
                    end
                elseif batch_action == "delete" then
                    for _, cid in ipairs(client_list) do
                        local clean = sanitize_client(cid)
                        if clean then
                            sys.exec("/usr/lib/proxypool/proxypool.sh stop_client " .. clean .. " 2>/dev/null")
                        end
                    end
                    for _, cid in ipairs(client_list) do
                        local clean = sanitize_client(cid)
                        if clean then
                            uci:delete("proxypool", clean)
                            processed = processed + 1
                        end
                    end
                    uci:commit("proxypool")
                elseif batch_action == "connect" then
                    for _, cid in ipairs(client_list) do
                        local clean = sanitize_client(cid)
                        if clean then
                            sys.exec("/usr/lib/proxypool/proxypool.sh start_client " .. clean .. " 2>/dev/null")
                            processed = processed + 1
                        end
                    end
                elseif batch_action == "disconnect" then
                    for _, cid in ipairs(client_list) do
                        local clean = sanitize_client(cid)
                        if clean then
                            sys.exec("/usr/lib/proxypool/proxypool.sh stop_client " .. clean .. " 2>/dev/null")
                            processed = processed + 1
                        end
                    end
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
