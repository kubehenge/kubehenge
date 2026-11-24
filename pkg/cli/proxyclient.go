package cli

import (
	"net/http"
	"strings"
	"time"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

// savedProxyClient is an HTTP client that goes through the proxy saved in the appliance's settings,
// read again when they change. The appliance's service has no proxy in its environment, so the
// transport's default would ignore the setting.
func savedProxyClient(applianceDir, proxmoxURL string, timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &appliance.SavedTransport{Dir: applianceDir, ProxmoxURL: proxmoxURL}}
}

// savedReleaseMirror is the release mirror saved in the appliance's settings, read again for each
// pin, so that the interface reads releases where the nodes download them.
func (o *serveOptions) savedReleaseMirror() provisioner.ReleaseMirror {
	if o.a.Dir == "" {
		return provisioner.ReleaseMirror{}
	}
	saved := o.a.SavedSettings()
	if saved == nil {
		return provisioner.ReleaseMirror{}
	}
	return saved.EffectiveReleaseMirror()
}

// savedGuestProxy is the proxy and the authorities the appliance's template builds give their
// guests, nil with neither.
func savedGuestProxy(a *appliance.Appliance) *templates.GuestProxy {
	saved := a.SavedSettings()
	if saved == nil {
		return nil
	}
	guest := &templates.GuestProxy{CABundle: saved.OutboundCA()}
	if saved.Proxy.Enabled() {
		guest.URL, guest.NoProxy = saved.Proxy.FullURL(), strings.Join(saved.NoProxyList(a.ProxmoxURL), ",")
	}
	if !guest.Configured() {
		return nil
	}
	return guest
}
