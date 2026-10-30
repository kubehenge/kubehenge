// Package redact takes secrets out of what Kubehenge hands out for someone to read: log lines, a
// node's boot log, and the objects of a support bundle.
package redact

import (
	"regexp"
	"strings"
)

// Mark stands where a secret was.
const Mark = "[REDACTED]"

var (
	// A key that names a secret, followed by its value: in JSON, YAML, a flag or key=value.
	keyed = regexp.MustCompile(`(?i)("?[a-z0-9_.-]*(?:password|passwd|secret|token|apikey|api_key|privatekey|private_key|certificatekey|certificate_key|certificate-key)[a-z0-9_.-]*"?\s*[:=]\s*)("(?:[^"\\]|\\.)*"|[^\s,;}&"]+)`)
	// A flag that takes a secret as its next argument, as kubeadm join and k3s print theirs.
	flagged = regexp.MustCompile(`(?i)(--(?:token|password|certificate-key|agent-token)\s+)(\S+)`)
	bearer  = regexp.MustCompile(`(?i)(bearer\s+)[a-z0-9._~+/=-]+`)
	// A kubeadm bootstrap token, wherever it stands.
	bootstrapToken = regexp.MustCompile(`\b[a-z0-9]{6}\.[a-z0-9]{16}\b`)
	privateKey     = regexp.MustCompile(`-{5}BEGIN [A-Z ]*PRIVATE KEY-{5}`)
)

// Line takes the secrets out of one line of a log.
func Line(s string) string {
	if privateKey.MatchString(s) {
		return Mark
	}
	s = keyed.ReplaceAllStringFunc(s, func(m string) string {
		parts := keyed.FindStringSubmatch(m)
		if strings.HasPrefix(parts[2], `"`) {
			return parts[1] + `"` + Mark + `"`
		}
		return parts[1] + Mark
	})
	s = flagged.ReplaceAllString(s, "${1}"+Mark)
	s = bearer.ReplaceAllString(s, "${1}"+Mark)
	return bootstrapToken.ReplaceAllString(s, Mark)
}

// Text takes the secrets out of every line of a text.
func Text(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = Line(l)
	}
	return strings.Join(lines, "\n")
}

// What names a secret in an object's field: such a key's text is taken out whole.
var secretKeys = []string{
	"password", "passwd", "secret", "token", "privatekey", "apikey", "credential", "talosconfig",
	"kubeconfig", "certificatekey", "encryptionkey",
}

// What a key that names a secret ends in when its value only refers to one, such as secretName.
var referenceSuffixes = []string{"name", "names", "ref", "refs", "namespace", "kind", "path", "file"}

// secretKey tells whether an object's field holds a secret, rather than naming or referring to one.
func secretKey(key string) bool {
	k := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
	for _, suffix := range referenceSuffixes {
		if strings.HasSuffix(k, suffix) {
			return false
		}
	}
	for _, s := range secretKeys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// Object takes the secrets out of an object decoded from JSON, in place: the text of every field
// whose key names a secret, and secrets within any other text, such as a Helm chart's values.
func Object(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if s, ok := val.(string); ok && secretKey(k) && s != "" {
				t[k] = Mark
				continue
			}
			t[k] = Object(val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = Object(t[i])
		}
		return t
	case string:
		return Text(t)
	default:
		return v
	}
}
