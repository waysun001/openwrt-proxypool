package.path = "luci-app-proxypool/tests/stubs/?.lua;" .. package.path

local failures = 0

local function fail(message)
    error(message, 2)
end

local function equal(actual, expected, message)
    if actual ~= expected then
        fail((message or "values differ") .. ": got " .. tostring(actual) .. ", want " .. tostring(expected))
    end
end

local function truthy(value, message)
    if not value then fail(message or "expected truthy value") end
end

local function test(name, body)
    local ok, err = pcall(body)
    if ok then
        io.write("PASS: " .. name .. "\n")
    else
        failures = failures + 1
        io.stderr:write("FAIL: " .. name .. ": " .. tostring(err) .. "\n")
    end
end

local nixio = require "nixio"
local rpc = dofile("luci-app-proxypool/luasrc/model/proxypool_rpc.lua")

local function json_stub(encoded, decoded, parse_failure, encode_failure)
    return {
        stringify = function()
            if encode_failure then error("encoding failed") end
            return encoded
        end,
        parse = function()
            if parse_failure then error("malformed JSON") end
            return decoded
        end
    }
end

local function call_with(response, options)
    options = options or {}
    nixio.reset({
        receive = response,
        socket_missing = options.socket_missing,
        connect_failure = options.connect_failure,
        send_failure = options.send_failure,
        setopt_failure = options.setopt_failure
    })
    local decoded = options.decoded or { version = 1, id = "test-id", result = { ok = true } }
    local json = json_stub(options.encoded or "request", decoded, options.parse_failure, options.encode_failure)
    return rpc._call("status.get", {}, {
        nixio = nixio,
        json = json,
        request_id = function() return "test-id" end
    })
end

test("frames one request and validates one response", function()
    local result, err = call_with({ "response\n" })
    truthy(result and result.ok, "missing daemon result")
    equal(err, nil, "unexpected error")
    equal(nixio.state.domain, "unix", "socket domain")
    equal(nixio.state.kind, "stream", "socket kind")
    equal(nixio.state.connect_path, "/var/run/proxypoold.sock", "socket path")
    equal(nixio.state.sent, "request\n", "newline request framing")
    equal(nixio.state.closed, 1, "socket close count")
    equal(#nixio.state.setopts, 2, "timeout option count")
end)

test("encodes empty parameters as an object for the strict daemon contract", function()
    local serialized = '{"version":1,"id":"test-id","method":"status.get","params":[]}'
    local result, err = call_with({ "response\n" }, { encoded = serialized })
    truthy(result and result.ok, "missing daemon result")
    equal(err, nil, "unexpected error")
    equal(
        nixio.state.sent,
        '{"version":1,"id":"test-id","method":"status.get","params":{}}\n',
        "empty parameters JSON shape"
    )
end)

test("rejects an oversized request before opening a socket", function()
    local result, err = call_with({}, { encoded = string.rep("x", 1024 * 1024 + 1) })
    equal(result, nil, "oversized result")
    equal(err.code, "invalid_request", "oversized code")
    equal(err.http_status, 400, "oversized status")
    equal(nixio.state.connect_path, nil, "oversized request opened a socket")
end)

test("maps request encoding failure to 400", function()
    local result, err = call_with({}, { encode_failure = true })
    equal(result, nil, "encoding failure result")
    equal(err.code, "invalid_request", "encoding failure code")
    equal(err.http_status, 400, "encoding failure status")
end)

test("maps daemon connection failure to 503", function()
    local result, err = call_with({}, { connect_failure = true })
    equal(result, nil, "connection failure result")
    equal(err.code, "service_unavailable", "connection failure code")
    equal(err.http_status, 503, "connection failure status")
    equal(nixio.state.closed, 1, "failed connection socket close")
end)

test("maps incomplete or timed out response to 502", function()
    local result, err = call_with({ "partial" })
    equal(result, nil, "timeout result")
    equal(err.code, "bad_gateway", "timeout code")
    equal(err.http_status, 502, "timeout status")
end)

test("rejects mismatched response ids", function()
    local result, err = call_with({ "response\n" }, {
        decoded = { version = 1, id = "other-id", result = {} }
    })
    equal(result, nil, "mismatched id result")
    equal(err.code, "bad_gateway", "mismatched id code")
    equal(err.http_status, 502, "mismatched id status")
end)

test("maps a malformed daemon JSON response to 502", function()
    local result, err = call_with({ "not-json\n" }, { parse_failure = true })
    equal(result, nil, "malformed response result")
    equal(err.code, "bad_gateway", "malformed response code")
    equal(err.http_status, 502, "malformed response status")
end)

test("rejects a daemon error with the wrong shape", function()
    local result, err = call_with({ "response\n" }, {
        decoded = { version = 1, id = "test-id", error = "not-an-error-object" }
    })
    equal(result, nil, "invalid error result")
    equal(err.code, "bad_gateway", "invalid error code")
    equal(err.http_status, 502, "invalid error status")
end)

test("maps daemon capacity exhaustion to 422", function()
    local result, err = call_with({ "response\n" }, {
        decoded = {
            version = 1,
            id = "test-id",
            error = { code = "capacity_exceeded", message = "node capacity is exhausted" }
        }
    })
    equal(result, nil, "capacity result")
    equal(err.code, "capacity_exceeded", "capacity code")
    equal(err.http_status, 422, "capacity status")
end)

test("never includes request payload or credentials in transport errors", function()
    local secret = "credential-that-must-not-leak"
    local result, err = call_with({}, {
        encoded = "encoded-" .. secret,
        send_failure = true
    })
    equal(result, nil, "send failure result")
    local visible = tostring(err.code) .. " " .. tostring(err.message)
    truthy(not visible:find(secret, 1, true), "transport error leaked credentials")
end)

if failures > 0 then
    os.exit(1)
end
