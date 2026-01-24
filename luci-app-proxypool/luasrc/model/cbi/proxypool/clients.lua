-- ProxyPool 客户端管理配置界面
local m, s, o

m = Map("proxypool", translate("代理池 - 客户端管理"),
    translate("管理L2TP和SOCKS5代理客户端，最多支持60个客户端"))

-- 全局设置
s = m:section(NamedSection, "global", "global", translate("全局设置"))
s.anonymous = true

o = s:option(Flag, "enabled", translate("启用服务"))
o.rmempty = false

o = s:option(Value, "max_clients", translate("最大客户端数"))
o.datatype = "uinteger"
o.default = "60"

o = s:option(ListValue, "log_level", translate("日志级别"))
o:value("debug", translate("调试"))
o:value("info", translate("信息"))
o:value("warn", translate("警告"))
o:value("error", translate("错误"))
o.default = "info"

o = s:option(Value, "status_interval", translate("状态刷新间隔(秒)"))
o.datatype = "uinteger"
o.default = "30"

-- 客户端列表
s = m:section(TypedSection, "client", translate("客户端列表"))
s.template = "cbi/tblsection"
s.addremove = true
s.anonymous = false
s.sortable = true
s.extedit = luci.dispatcher.build_url("admin", "services", "proxypool", "clients", "%s")

-- 模板用于创建新客户端
function s.create(self, name)
    -- 自动生成客户端ID
    if not name or #name == 0 then
        local num = 1
        while luci.model.uci.cursor():get("proxypool", "client_" .. string.format("%02d", num)) do
            num = num + 1
            if num > 60 then
                return nil
            end
        end
        name = "client_" .. string.format("%02d", num)
    end

    return TypedSection.create(self, name)
end

o = s:option(Flag, "enabled", translate("启用"))
o.width = "5%"
o.rmempty = false

o = s:option(Value, "name", translate("名称"))
o.width = "15%"
o.rmempty = false

o = s:option(ListValue, "type", translate("类型"))
o:value("l2tp", "L2TP")
o:value("socks5", "SOCKS5")
o.width = "10%"
o.rmempty = false

o = s:option(Value, "server", translate("服务器"))
o.width = "20%"
o.datatype = "host"
o.rmempty = false

o = s:option(Value, "port", translate("端口"))
o.width = "10%"
o.datatype = "port"
o.default = "1701"

o = s:option(Value, "username", translate("用户名"))
o.width = "15%"

o = s:option(Value, "password", translate("密码"))
o.width = "15%"
o.password = true

o = s:option(DummyValue, "_status", translate("状态"))
o.width = "10%"
o.rawhtml = true
o.cfgvalue = function(self, section)
    return '<span id="status_' .. section .. '">-</span>'
end

return m
