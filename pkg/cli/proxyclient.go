package cli

import (
	"net/http"
	"strings"
	"time"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

// savedProxyClient is an HTTP client that goes through the proxy saved in the appliance's settings,
// read again when they change. The appliance's service has no proxy in its environment, so the
// transport's default would ignore the setting.
func savedProxyClient(applianceDir, proxmoxURL string, timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &appliance.SavedTransport{Dir: applianceDir, ProxmoxURL: proxmoxURL}}
}

// savedGuestProxy is the proxy the appliance's template builds give their guests, nil without one.
func savedGuestProxy(a *appliance.Appliance) *templates.GuestProxy {
	saved := a.SavedSettings()
	if saved == nil || !saved.Proxy.Enabled() {
		return nil
	}
	return &templates.GuestProxy{
		URL: saved.Proxy.FullURL(), NoProxy: strings.Join(saved.NoProxyList(a.ProxmoxURL), ","), CABundle: saved.Proxy.CABundle,
	}
}
