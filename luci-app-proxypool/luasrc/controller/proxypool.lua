-- ProxyPool LuCI Controller
module("luci.controller.proxypool", package.seeall)

function index()
    -- 主入口
    entry({"admin", "services", "proxypool"}, firstchild(), _("代理池"), 60).dependent = false

    -- 总览页面
    entry({"admin", "services", "proxypool", "overview"}, template("proxypool/overview"), _("总览"), 10)

    -- 客户端管理
    entry({"admin", "services", "proxypool", "clients"}, cbi("proxypool/clients"), _("客户端管理"), 20)

    -- 设备绑定
    entry({"admin", "services", "proxypool", "bindings"}, cbi("proxypool/bindings"), _("设备绑定"), 30)

    -- 状态监控
    entry({"admin", "services", "proxypool", "status"}, template("proxypool/status"), _("状态监控"), 40)

    -- 备份升级
    entry({"admin", "services", "proxypool", "backup"}, template("proxypool/backup"), _("备份升级"), 50)

    -- 日志
    entry({"admin", "services", "proxypool", "log"}, template("proxypool/log"), _("日志"), 60)

    -- API 接口
    entry({"admin", "services", "proxypool", "api"}, call("api_handler")).leaf = true
end

-- API 处理器
function api_handler()
    local http = require "luci.http"
    local sys = require "luci.sys"
    local uci = require "luci.model.uci".cursor()
    local json = require "luci.jsonc"

    local path = http.formvalue("action") or ""

    if path == "status" then
        -- 获取状态
        local result = sys.exec("/usr/lib/proxypool/status.sh get 2>/dev/null")
        http.prepare_content("application/json")
        http.write(result)

    elseif path == "start_client" then
        local client = http.formvalue("client")
        if client then
            sys.exec("/usr/lib/proxypool/proxypool.sh start_client " .. client .. " 2>/dev/null")
            http.prepare_content("application/json")
            http.write('{"success": true}')
        end

    elseif path == "stop_client" then
        local client = http.formvalue("client")
        if client then
            sys.exec("/usr/lib/proxypool/proxypool.sh stop_client " .. client .. " 2>/dev/null")
            http.prepare_content("application/json")
            http.write('{"success": true}')
        end

    elseif path == "restart_client" then
        local client = http.formvalue("client")
        if client then
            sys.exec("/usr/lib/proxypool/proxypool.sh restart_client " .. client .. " 2>/dev/null")
            http.prepare_content("application/json")
            http.write('{"success": true}')
        end

    elseif path == "reload" then
        sys.exec("/usr/lib/proxypool/proxypool.sh reload 2>/dev/null")
        http.prepare_content("application/json")
        http.write('{"success": true}')

    elseif path == "backup_create" then
        local file = "/tmp/proxypool_backup_" .. os.time() .. ".tar.gz"
        sys.exec("/usr/lib/proxypool/backup.sh create " .. file)
        http.prepare_content("application/json")
        http.write('{"success": true, "file": "' .. file .. '"}')

    elseif path == "backup_verify" then
        local file = http.formvalue("file")
        if file then
            local result = sys.exec("/usr/lib/proxypool/backup.sh verify " .. file .. " 2>/dev/null")
            http.prepare_content("application/json")
            http.write(result)
        end

    elseif path == "backup_restore" then
        local file = http.formvalue("file")
        if file then
            sys.exec("/usr/lib/proxypool/backup.sh restore " .. file .. " 2>/dev/null &")
            http.prepare_content("application/json")
            http.write('{"success": true}')
        end

    elseif path == "log" then
        local lines = http.formvalue("lines") or "100"
        local result = sys.exec("tail -n " .. lines .. " /var/log/proxypool.log 2>/dev/null")
        http.prepare_content("text/plain")
        http.write(result)

    else
        http.prepare_content("application/json")
        http.write('{"error": "Unknown action"}')
    end
end
