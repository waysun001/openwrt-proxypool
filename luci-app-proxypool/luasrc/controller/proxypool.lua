module("luci.controller.proxypool", package.seeall)

local SOCKET = "/var/run/proxypoold.sock"
local MAX_RESPONSE = 1024 * 1024

local ACTIONS = {
    status = { method = "status.get", mutation = false },
    devices = { method = "device.list", mutation = false },
    jobs = { method = "job.list", mutation = false },
    job = { method = "job.get", mutation = false },
    events = { method = "system.events", mutation = false },
    bind = { method = "device.bind", mutation = true },
    unbind = { method = "device.unbind", mutation = true },
    node_action = { method = "node.action", mutation = true },
    import_preview = { method = "import.preview", mutation = true },
    import_commit = { method = "import.commit", mutation = true }
}

function index()
    entry({"admin", "services", "proxypool"}, call("main_page"), _("ProxyPool"), 1).dependent = false
    entry({"admin", "services", "proxypool", "api"}, call("api_handler")).leaf = true
    entry({"admin"}, alias("admin", "services", "proxypool"), _("Administration"), 10).index = true
end

function main_page()
    require("luci.template").render("proxypool/main", {})
end

local function request_id()
    local file = io.open("/proc/sys/kernel/random/uuid", "r")
    local value
    if file then
        value = file:read("*l")
        file:close()
    end
    value = tostring(value or (tostring(os.time()) .. "-" .. tostring(math.random(100000, 999999))))
    value = value:gsub("[^A-Za-z0-9_-]", "")
    return "luci-" .. value:sub(1, 64)
end

local function exact_id(value)
    value = tostring(value or "")
    if #value < 1 or #value > 64 or not value:match("^[A-Za-z0-9_-]+$") then return nil end
    return value
end

local function exact_revision(value)
    local number = tonumber(value)
    if not number or number < 1 or number ~= math.floor(number) then return nil end
    return number
end

local function exact_hash(value)
    value = tostring(value or "")
    if #value ~= 64 or not value:match("^[a-f0-9]+$") then return nil end
    return value
end

local function params_for(action, http)
    if action == "status" or action == "devices" or action == "jobs" then return {} end
    if action == "job" then
        local job = exact_id(http.formvalue("job_id"))
        if not job then return nil end
        return { job_id = job }
    end
    if action == "events" then
        local after = tonumber(http.formvalue("after_sequence") or "0")
        local limit = tonumber(http.formvalue("limit") or "100")
        if not after or after < 0 or after ~= math.floor(after) or not limit or limit < 1 or limit > 200 or limit ~= math.floor(limit) then return nil end
        return { after_sequence = after, limit = limit }
    end
    if action == "import_preview" then
        local protocol = tostring(http.formvalue("protocol") or "")
        local raw = tostring(http.formvalue("raw") or "")
        local revision = exact_revision(http.formvalue("expected_revision"))
        if (protocol ~= "l2tp" and protocol ~= "socks5" and protocol ~= "slp") or
            #raw < 1 or #raw > MAX_RESPONSE or not revision then return nil end
        return { protocol = protocol, raw = raw, expected_revision = revision }
    end
    if action == "import_commit" then
        local preview = exact_id(http.formvalue("preview_id"))
        local hash = exact_hash(http.formvalue("preview_hash"))
        local revision = exact_revision(http.formvalue("expected_revision"))
        if not preview or not hash or not revision then return nil end
        return { preview_id = preview, preview_hash = hash, expected_revision = revision }
    end
    local revision = exact_revision(http.formvalue("expected_revision"))
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
    if action == "node_action" then
        local node = exact_id(http.formvalue("node_id"))
        local operation = tostring(http.formvalue("operation") or "")
        if not revision or not node or (operation ~= "connect" and operation ~= "reconnect" and operation ~= "stop") then return nil end
        return { node_id = node, action = operation, expected_revision = revision }
    end
    return nil
end

local function daemon_call(method, params)
    local json = require "luci.jsonc"
    local nixio = require "nixio"
    local id = request_id()
    local payload = json.stringify({ version = 1, id = id, method = method, params = params })
    if type(payload) ~= "string" or #payload > MAX_RESPONSE then
        return nil, { code = "invalid_request", message = "request encoding failed" }
    end
    local socket = nixio.socket("unix", "stream")
    if not socket or not socket:setopt("socket", "sndtimeo", 12, 0) or
        not socket:setopt("socket", "rcvtimeo", 12, 0) or not socket:connect(SOCKET) then
        if socket then socket:close() end
        return nil, { code = "service_unavailable", message = "proxypoold is unavailable" }
    end
    if not socket:sendall(payload .. "\n") then
        socket:close()
        return nil, { code = "service_unavailable", message = "control request failed" }
    end
    local chunks, size, complete = {}, 0, false
    while size <= MAX_RESPONSE do
        local chunk = socket:recv(4096)
        if not chunk or #chunk == 0 then break end
        local newline = chunk:find("\n", 1, true)
        if newline then
            chunk = chunk:sub(1, newline - 1)
            complete = true
        end
        size = size + #chunk
        chunks[#chunks + 1] = chunk
        if complete then break end
    end
    socket:close()
    if not complete or size > MAX_RESPONSE then
        return nil, { code = "service_unavailable", message = "control response failed" }
    end
    local response = json.parse(table.concat(chunks))
    if type(response) ~= "table" or response.version ~= 1 or response.id ~= id or (response.result == nil) == (response.error == nil) then
        return nil, { code = "service_unavailable", message = "control response is invalid" }
    end
    if response.error then return nil, response.error end
    return response.result, nil
end

local function write_error(http, json, err)
    local code = tostring((err or {}).code or "service_unavailable")
    local status = 503
    if code == "revision_conflict" or code == "duplicate" then status = 409
    elseif code == "invalid_request" or code == "invalid_config" or code == "unsupported" then status = 422
    elseif code == "not_found" then status = 404 end
    http.status(status, "ProxyPool API Error")
    http.prepare_content("application/json")
    http.write(json.stringify({ success = false, error = { code = code, message = tostring((err or {}).message or "request failed") } }))
end

function api_handler()
    local http = require "luci.http"
    local json = require "luci.jsonc"
    local dispatcher = require "luci.dispatcher"
    local action = tostring(http.formvalue("action") or "")
    local definition = ACTIONS[action]
    if not definition then
        return write_error(http, json, { code = "not_found", message = "unknown action" })
    end
    if definition.mutation then
        if http.getenv("REQUEST_METHOD") ~= "POST" then
            http.status(405, "Method Not Allowed")
            http.prepare_content("application/json")
            http.write(json.stringify({ success = false, error = { code = "invalid_request", message = "POST is required" } }))
            return
        end
        if not dispatcher.test_post_security() then
            http.status(403, "Forbidden")
            http.prepare_content("application/json")
            http.write(json.stringify({ success = false, error = { code = "invalid_request", message = "security token failed" } }))
            return
        end
    end
    local params = params_for(action, http)
    if not params then
        return write_error(http, json, { code = "invalid_request", message = "request parameters are invalid" })
    end
    local result, err = daemon_call(definition.method, params)
    if err then return write_error(http, json, err) end
    http.prepare_content("application/json")
    http.write(json.stringify({ success = true, result = result }))
end
