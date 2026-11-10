-- Test fixture only. The actual CrowdSec client is installed by its package.
local module = {}
function module.init() return true end
function module.get_mode() return "stream" end
function module.SetupStream() end
function module.SetupMetrics() end
function module.Allow(ip)
    if ip == "192.0.2.66" then return ngx.exit(ngx.HTTP_FORBIDDEN) end
end
return module
