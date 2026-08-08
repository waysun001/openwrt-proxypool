local rpc = {}

local SOCKET_PATH = "/var/run/proxypoold.sock"
local MAX_FRAME_SIZE = 1024 * 1024
local TIMEOUT_SECONDS = 12

local function failure(code, message, http_status)
    return nil, {
        code = code,
        message = message,
        http_status = http_status
    }
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

local function daemon_error_status(code)
    if code == "invalid_request" then return 400 end
    if code == "revision_conflict" or code == "duplicate" then return 409 end
    if code == "not_found" then return 404 end
    if code == "capacity_exceeded" or code == "invalid_config" or code == "unsupported" then return 422 end
    if code == "service_unavailable" then return 503 end
    return 502
end

local function close_socket(socket)
    if socket then socket:close() end
end

function rpc._call(method, params, dependencies)
    dependencies = dependencies or {}
    local json = dependencies.json or require "luci.jsonc"
    local nixio = dependencies.nixio or require "nixio"
    local make_request_id = dependencies.request_id or request_id
    local id = make_request_id()
    local request_params = params or {}
    local encoded, payload = pcall(json.stringify, {
        version = 1,
        id = id,
        method = method,
        params = request_params
    })
    -- luci.jsonc serializes an empty Lua table as []; the daemon contract requires {}.
    if encoded and type(payload) == "string" and type(request_params) == "table" and next(request_params) == nil then
        payload = payload:gsub('("params"%s*:%s*)%[%]', '%1{}', 1)
    end
    if not encoded or type(payload) ~= "string" or #payload > MAX_FRAME_SIZE then
        return failure("invalid_request", "request encoding failed", 400)
    end

    local socket = nixio.socket("unix", "stream")
    if not socket then
        return failure("service_unavailable", "proxypoold is unavailable", 503)
    end
    if not socket:setopt("socket", "sndtimeo", TIMEOUT_SECONDS, 0) or
        not socket:setopt("socket", "rcvtimeo", TIMEOUT_SECONDS, 0) or
        not socket:connect(SOCKET_PATH) then
        close_socket(socket)
        return failure("service_unavailable", "proxypoold is unavailable", 503)
    end
    if not socket:sendall(payload .. "\n") then
        close_socket(socket)
        return failure("service_unavailable", "control request failed", 503)
    end

    local chunks = {}
    local size = 0
    local complete = false
    while size <= MAX_FRAME_SIZE do
        local chunk = socket:recv(4096)
        if type(chunk) ~= "string" or #chunk == 0 then break end
        local newline = chunk:find("\n", 1, true)
        if newline then
            chunk = chunk:sub(1, newline - 1)
            complete = true
        end
        size = size + #chunk
        chunks[#chunks + 1] = chunk
        if complete then break end
    end
    close_socket(socket)
    if not complete or size > MAX_FRAME_SIZE then
        return failure("bad_gateway", "control response failed", 502)
    end

    local parsed, response = pcall(json.parse, table.concat(chunks))
    if not parsed then
        return failure("bad_gateway", "control response is invalid", 502)
    end
    local has_result = type(response) == "table" and response.result ~= nil
    local has_error = type(response) == "table" and response.error ~= nil
    if type(response) ~= "table" or response.version ~= 1 or response.id ~= id or has_result == has_error then
        return failure("bad_gateway", "control response is invalid", 502)
    end
    if has_error then
        if type(response.error) ~= "table" then
            return failure("bad_gateway", "control response is invalid", 502)
        end
        local code = tostring(response.error.code or "internal")
        return failure(code, tostring(response.error.message or "request failed"), daemon_error_status(code))
    end
    return response.result, nil
end

function rpc.call(method, params)
    return rpc._call(method, params, nil)
end

return rpc
