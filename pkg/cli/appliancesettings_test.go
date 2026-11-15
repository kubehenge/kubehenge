package cli

import (
	"strings"
	"testing"
)

// field reads a string out of a decoded request body, empty when it is not one.
func field(m map[string]any, name string) string {
	s, ok := m[name].(string)
	if !ok {
		return ""
	}
	return s
}

// savedBody is the settings object of the last save the fake interface got.
func savedBody(f *fakeInterface) map[string]any {
	settings, ok := f.bodies["POST /api/setup"]["settings"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return settings
}

func TestApplianceSettingsShowAndChangeTheSiteDefaults(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	out, err := run(t, f.args("admin@pve", "settings")...)
	if err != nil || out != "Site CA     None\nTime zone   Europe/Zurich\n" {
		t.Errorf("show: %v\n%q", err, out)
	}
	pem := "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----"
	out, err = run(t, f.args("admin@pve", "settings", "set", "--site-ca-file", writeTemp(t, pem), "--passphrase-file", writeTemp(t, "right"))...)
	if err != nil || !strings.Contains(out, "Applying the settings") {
		t.Fatalf("set: %v\n%s", err, out)
	}
	settings := savedBody(f)
	if !strings.HasPrefix(field(settings, "siteCA"), pem) || field(settings, "timeZone") != "Europe/Zurich" || field(settings, "bridge") != "vmbr0" || field(f.bodies["POST /api/setup"], "passphrase") != "right" {
		t.Errorf("set sent %v, want the site CA added to the saved settings", f.bodies["POST /api/setup"])
	}
}

func TestApplianceSettingsRefuseWhatTheInterfaceRefuses(t *testing.T) {
	t.Parallel()
	f := newFakeInterface(t)
	if _, err := run(t, f.args("admin@pve", "settings", "set")...); err == nil || !strings.Contains(err.Error(), "--site-ca-file or --time-zone") {
		t.Errorf("nothing to set: %v", err)
	}
	if _, err := run(t, f.args("admin@pve", "settings", "set", "--time-zone", "Mars/Base")...); err == nil || !strings.Contains(err.Error(), "not an IANA time zone") {
		t.Errorf("a bad time zone: %v", err)
	}
	if _, err := run(t, f.args("admin@pve", "settings", "set", "--time-zone", "")...); err != nil {
		t.Errorf("clearing the time zone: %v", err)
	}
	if got := field(savedBody(f), "timeZone"); got != "" {
		t.Errorf("the cleared time zone was sent as %q", got)
	}
}
