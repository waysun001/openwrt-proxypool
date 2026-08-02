local nixio = {
    state = {}
}

function nixio.reset(options)
    options = options or {}
    nixio.state = {
        options = options,
        connect_path = nil,
        sent = nil,
        closed = 0,
        setopts = {}
    }
end

function nixio.socket(domain, kind)
    local state = nixio.state
    local options = state.options or {}
    state.domain = domain
    state.kind = kind
    if options.socket_missing then
        return nil
    end

    local socket = {}
    local receive_index = 0

    function socket:setopt(level, name, seconds, microseconds)
        state.setopts[#state.setopts + 1] = {
            level = level,
            name = name,
            seconds = seconds,
            microseconds = microseconds
        }
        return not options.setopt_failure
    end

    function socket:connect(path)
        state.connect_path = path
        return not options.connect_failure
    end

    function socket:sendall(payload)
        state.sent = payload
        return not options.send_failure
    end

    function socket:recv()
        receive_index = receive_index + 1
        return (options.receive or {})[receive_index]
    end

    function socket:close()
        state.closed = state.closed + 1
    end

    return socket
end

nixio.reset()

return nixio
