-- ProxyPool 设备绑定配置界面
local m, s, o
local uci = luci.model.uci.cursor()

m = Map("proxypool", translate("代理池 - 设备绑定"),
    translate("将内网设备IP绑定到指定的代理客户端。未绑定的设备将无法访问外网。"))

-- 获取所有客户端列表
local clients = {}
uci:foreach("proxypool", "client", function(section)
    local name = section.name or section[".name"]
    local enabled = section.enabled == "1"
    local status = enabled and "启用" or "禁用"
    clients[section[".name"]] = name .. " (" .. status .. ")"
end)

-- 设备绑定表格
s = m:section(TypedSection, "client", translate("设备绑定"))
s.template = "cbi/tblsection"
s.addremove = false
s.anonymous = false

function s.filter(self, section)
    return true
end

-- 客户端名称（只读）
o = s:option(DummyValue, "name", translate("客户端"))
o.width = "20%"

-- 类型（只读）
o = s:option(DummyValue, "type", translate("类型"))
o.width = "10%"

-- 状态
o = s:option(DummyValue, "_status", translate("状态"))
o.width = "10%"
o.rawhtml = true
o.cfgvalue = function(self, section)
    local enabled = uci:get("proxypool", section, "enabled")
    if enabled == "1" then
        return '<span style="color:green">●</span> 启用'
    else
        return '<span style="color:gray">○</span> 禁用'
    end
end

-- 绑定IP列表
o = s:option(DynamicList, "bind_ip", translate("绑定IP"))
o.width = "50%"
o.datatype = "ip4addr"
o.placeholder = "192.168.1.100"

-- 帮助信息
m:section(SimpleSection).template = "proxypool/bindings_help"

return m
