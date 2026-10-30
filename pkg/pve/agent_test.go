package pve

import "testing"

func TestFileBytesUndoesProxmoxVEsEncoding(t *testing.T) {
	t.Parallel()
	// Each byte of the file as one character, as Proxmox VE sends a file the agent read.
	asSent := func(file string) string {
		runes := make([]rune, 0, len(file))
		for _, b := range []byte(file) {
			runes = append(runes, rune(b))
		}
		return string(runes)
	}
	cut := asSent("done ✓")
	for _, tc := range []struct{ name, content, want string }{
		{"ASCII", "k3s[812]: started\n", "k3s[812]: started\n"},
		{"UTF-8", asSent("cloud-init … done ✓\n"), "cloud-init … done ✓\n"},
		{"cut in a character", cut[:len(cut)-2], "done �"},
		{"not sent byte by byte", "Ölfeld ✓", "Ölfeld ✓"},
	} {
		if got := fileBytes(tc.content); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
