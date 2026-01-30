-- 智联盒子 LuCI Controller
module("luci.controller.proxypool", package.seeall)

function index()
    entry({"admin", "services", "proxypool"}, template("proxypool/main"), _("智联盒子"), 60).dependent = false
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
                if d.bind_ip and #d.bind_ip > 0 then
                    uci:set("proxypool", client, "bind_ip", d.bind_ip)
                else
                    uci:delete("proxypool", client, "bind_ip")
                end
                uci:commit("proxypool")
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

    elseif action == "restart_client" then
        local client = sanitize_client(http.formvalue("client"))
        if client then
            sys.exec("/usr/lib/proxypool/proxypool.sh restart_client " .. client .. " 2>/dev/null")
            http.prepare_content("application/json")
            http.write('{"success": true}')
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
