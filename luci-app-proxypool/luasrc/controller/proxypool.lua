module("luci.controller.proxypool", package.seeall)

local MAX_IMPORT_SIZE = 1024 * 1024

local READ_ACTIONS = {
    status = "status.get",
    devices = "device.list",
    jobs = "job.list",
    job = "job.get",
    events = "system.events",
    diagnostics = "diagnostics.get"
}

local WRITE_ACTIONS = {
    bind = "device.bind",
    unbind = "device.unbind",
    node_save = "node.save",
    node_delete = "node.delete",
    node_action = "node.action",
    import_preview = "import.preview",
    import_commit = "import.commit",
    diagnostics_create = "diagnostics.create"
}

function index()
    entry({"admin", "services", "proxypool"}, call("main_page"), _("ProxyPool"), 1).dependent = false
    entry({"admin", "services", "proxypool", "locked"}, call("locked_page")).leaf = true
    entry({"admin", "services", "proxypool", "lease"}, call("lease_page")).leaf = true
    entry({"admin", "services", "proxypool", "api", "read"}, call("api_read")).leaf = true
    entry({"admin", "services", "proxypool", "api", "write"}, post("api_write")).leaf = true
    entry({"admin", "services", "proxypool", "download"}, post("diagnostics_download")).leaf = true
    entry({"admin"}, alias("admin", "services", "proxypool"), _("Administration"), 10).index = true
end

function main_page()
    require("luci.template").render("proxypool/main", {})
end

function locked_page()
    require("luci.template").render("proxypool/locked", {})
end

function lease_page()
    require("luci.template").render("proxypool/lease", {})
end

local function exact_id(value, optional)
    value = tostring(value or "")
    if optional and value == "" then return "" end
    if #value < 1 or #value > 64 or not value:match("^[A-Za-z0-9_-]+$") then return nil end
    return value
end

local function exact_revision(value)
    local number = tonumber(value)
    if not number or number < 1 or number ~= math.floor(number) then return nil end
    return number
end

local function exact_integer(value, minimum, maximum)
    local number = tonumber(value)
    if not number or number < minimum or number > maximum or number ~= math.floor(number) then return nil end
    return number
end

local function exact_boolean(value)
    value = tostring(value or "")
    if value == "1" or value == "true" then return true end
    if value == "0" or value == "false" then return false end
    return nil
end

local function exact_hash(value)
    value = tostring(value or "")
    if #value ~= 64 or not value:match("^[a-f0-9]+$") then return nil end
    return value
end

local function bounded(value, maximum, required)
    value = tostring(value or "")
    if (required and #value == 0) or #value > maximum then return nil end
    return value
end

local function read_params(action, http)
    if action == "status" or action == "devices" or action == "jobs" then return {} end
    if action == "job" then
        local job = exact_id(http.formvalue("job_id"))
        if not job then return nil end
        return { job_id = job }
    end
    if action == "diagnostics" then
        local job = exact_id(http.formvalue("job_id"))
        if not job then return nil end
        return { job_id = job }
    end
    if action == "events" then
        local after = exact_integer(http.formvalue("after_sequence") or "0", 0, 9007199254740991)
        local limit = exact_integer(http.formvalue("limit") or "100", 1, 200)
        if not after or not limit then return nil end
        return { after_sequence = after, limit = limit }
    end
    return nil
end

local function node_save_params(http)
    local node = exact_id(http.formvalue("node_id"), true)
    local name = bounded(http.formvalue("name"), 128, true)
    local protocol = tostring(http.formvalue("protocol") or "")
    local enabled = exact_boolean(http.formvalue("enabled"))
    local server = bounded(http.formvalue("server"), 253, true)
    local port = exact_integer(http.formvalue("port"), 1, 65535)
    local username = bounded(http.formvalue("username"), 1024, false)
    local password = bounded(http.formvalue("password"), 4096, false)
    local expires = bounded(http.formvalue("expires_at"), 32, false)
    local revision = exact_revision(http.formvalue("expected_revision"))
    if node == nil or not name or (protocol ~= "l2tp" and protocol ~= "socks5" and protocol ~= "slp") or
        enabled == nil or not server or not port or username == nil or password == nil or expires == nil or not revision then
        return nil
    end
    return {
        node_id = node,
        name = name,
        protocol = protocol,
        enabled = enabled,
        server = server,
        port = port,
        username = username,
        password = password,
        expires_at = expires,
        expected_revision = revision
    }
end

local function write_params(action, http)
    if action == "node_save" then return node_save_params(http) end
    if action == "diagnostics_create" then return {} end

    local revision = exact_revision(http.formvalue("expected_revision"))
    if action == "import_preview" then
        local protocol = tostring(http.formvalue("protocol") or "")
        local raw = tostring(http.formvalue("raw") or "")
        if (protocol ~= "l2tp" and protocol ~= "socks5" and protocol ~= "slp") or
            #raw < 1 or #raw > MAX_IMPORT_SIZE or not revision then return nil end
        return { protocol = protocol, raw = raw, expected_revision = revision }
    end
    if action == "import_commit" then
        local preview = exact_id(http.formvalue("preview_id"))
        local hash = exact_hash(http.formvalue("preview_hash"))
        if not preview or not hash or not revision then return nil end
        return { preview_id = preview, preview_hash = hash, expected_revision = revision }
    end

    local device = exact_id(http.formvalue("device_id"))
    if action == "bind" then
        local node = exact_id(http.formvalue("node_id"))
        if not revision or not device or not node then return nil end
        return { device_id = device, node_id = node, expected_revision = revision }
    end
    if action == "unbind" then
        if not revision or not device then return nil end
        return { device_id = device, expected_revision = revision }
    end
    if action == "node_delete" then
        local node = exact_id(http.formvalue("node_id"))
        if not revision or not node then return nil end
        return { node_id = node, expected_revision = revision }
    end
    if action == "node_action" then
        local node = exact_id(http.formvalue("node_id"))
        local operation = tostring(http.formvalue("operation") or "")
        if not revision or not node or
            (operation ~= "connect" and operation ~= "reconnect" and operation ~= "stop") then return nil end
        return { node_id = node, action = operation, expected_revision = revision }
    end
    return nil
end

local function write_json(http, json, value)
    http.prepare_content("application/json")
    http.write(json.stringify(value))
end

local function write_error(http, json, err)
    err = err or {}
    local code = tostring(err.code or "bad_gateway")
    local status = tonumber(err.http_status) or 502
    http.status(status, "ProxyPool API Error")
    write_json(http, json, {
        success = false,
        error = {
            code = code,
            message = tostring(err.message or "request failed")
        }
    })
end

local function dispatch_action(actions, parameter_builder)
    local http = require "luci.http"
    local json = require "luci.jsonc"
    local rpc = require "luci.model.proxypool_rpc"
    local action = tostring(http.formvalue("action") or "")
    local method = actions[action]
    if not method then
        return write_error(http, json, { code = "not_found", message = "unknown action", http_status = 404 })
    end
    local params = parameter_builder(action, http)
    if not params then
        return write_error(http, json, { code = "invalid_request", message = "request parameters are invalid", http_status = 400 })
    end
    local result, err = rpc.call(method, params)
    if err then return write_error(http, json, err) end
    write_json(http, json, { success = true, result = result })
end

function api_read()
    local http = require "luci.http"
    if http.getenv("REQUEST_METHOD") ~= "GET" then
        http.header("Allow", "GET")
        return write_error(http, require("luci.jsonc"), {
            code = "invalid_request",
            message = "GET is required",
            http_status = 405
        })
    end
    return dispatch_action(READ_ACTIONS, read_params)
end

function api_write()
    return dispatch_action(WRITE_ACTIONS, write_params)
end

function diagnostics_download()
    local http = require "luci.http"
    local json = require "luci.jsonc"
    local fs = require "nixio.fs"
    local rpc = require "luci.model.proxypool_rpc"
    local artifact = tostring(http.formvalue("artifact_id") or "")
    if not artifact:match("^diag%-[a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9][a-f0-9]*$") or #artifact > 69 then
        return write_error(http, json, { code = "invalid_request", message = "artifact id is invalid", http_status = 400 })
    end
    local claim, claim_error = rpc.call("diagnostics.claim", { artifact_id = artifact })
    if claim_error then return write_error(http, json, claim_error) end
    local expected_path = "/tmp/proxypool/diagnostics/" .. artifact .. ".tar.gz"
    local expected_name = "proxypool-diagnostics-" .. artifact .. ".tar.gz"
    local claim_path = claim and tostring(claim.path or "") or ""
    local stat = claim_path == expected_path and fs.lstat(claim_path) or nil
    local safe = claim and claim.artifact_id == artifact and claim_path == expected_path and claim.filename == expected_name and
        tonumber(claim.size) and tonumber(claim.size) > 0 and tonumber(claim.size) <= 20 * 1024 * 1024 and
        stat and stat.type == "reg" and tonumber(stat.size) == tonumber(claim.size)
    if not safe then
        rpc.call("diagnostics.release", { artifact_id = artifact })
        return write_error(http, json, { code = "not_found", message = "diagnostic artifact is unavailable", http_status = 404 })
    end
    local file = io.open(expected_path, "rb")
    if not file then
        rpc.call("diagnostics.release", { artifact_id = artifact })
        return write_error(http, json, { code = "not_found", message = "diagnostic artifact is unavailable", http_status = 404 })
    end
    http.header("Cache-Control", "no-store")
    http.header("Content-Disposition", 'attachment; filename="' .. expected_name .. '"')
    http.header("Content-Length", tostring(claim.size))
    http.prepare_content("application/gzip")
    pcall(function()
        while true do
            local chunk = file:read(65536)
            if not chunk or #chunk == 0 then break end
            http.write(chunk)
        end
    end)
    file:close()
    rpc.call("diagnostics.release", { artifact_id = artifact })
end
