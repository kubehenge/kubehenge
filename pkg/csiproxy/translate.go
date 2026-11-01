package csiproxy

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/url"
	"slices"
	"strings"
)

// names turns the plugin's volume IDs of the copies into their real ones and back.
type names struct {
	// toReal maps each copy's volume ID as the plugin knows it to its real one. toPlugin maps the other
	// way.
	toReal, toPlugin map[string]string
}

func newNames(copies map[string]*Copy) names {
	n := names{toReal: map[string]string{}, toPlugin: map[string]string{}}
	for id, c := range copies {
		n.toReal[id] = c.Real
		n.toPlugin[c.Real] = id
	}
	return n
}

func (n names) empty() bool {
	return len(n.toReal) == 0
}

// volumeIDChar reports whether c may be part of a volume ID, storage:name with name possibly
// below a directory: a match must not have one of these on either side, so that
// local:vm-101-disk-1 is never found inside local:vm-101-disk-10 or other-local:vm-101-disk-1.
func volumeIDChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._-:/", c) >= 0
}

// replace replaces every whole volume ID of m's keys in s by its value.
func replace(s string, m map[string]string) string {
	if len(m) == 0 || !strings.Contains(s, ":") {
		return s
	}
	// Longest first, so that of two IDs where one starts the other, the longer one is taken whole.
	keys := slices.SortedFunc(maps.Keys(m), func(a, b string) int { return len(b) - len(a) })
	var out strings.Builder
	for i := 0; i < len(s); {
		if i > 0 && volumeIDChar(s[i-1]) {
			out.WriteByte(s[i])
			i++
			continue
		}
		replaced := false
		for _, k := range keys {
			end := i + len(k)
			if strings.HasPrefix(s[i:], k) && (end == len(s) || !volumeIDChar(s[end])) {
				out.WriteString(m[k])
				i = end
				replaced = true
				break
			}
		}
		if !replaced {
			out.WriteByte(s[i])
			i++
		}
	}
	return out.String()
}

// values translates every value of a query or form.
func values(v url.Values, m map[string]string) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		for _, s := range vs {
			out[k] = append(out[k], replace(s, m))
		}
	}
	return out
}

// volumePath translates the volume in a path naming one, /nodes/{node}/storage/{storage}/content/
// {volume}, where the plugin gives the volume's bare name, or Proxmox VE's storage:name.
func (n names) volumePath(storage, volume string) string {
	if realID, ok := n.toReal[volume]; ok {
		return realID
	}
	if realID, ok := n.toReal[storage+":"+volume]; ok {
		_, name, _ := splitVolID(realID)
		return name
	}
	return volume
}

// translateJSON translates a JSON answer of Proxmox VE's, every string in it wherever it is. It
// returns the body as it was when it is not JSON.
func translateJSON(body []byte, m map[string]string) []byte {
	if len(m) == 0 {
		return body
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return body
	}
	out, err := json.Marshal(walk(v, m))
	if err != nil {
		return body
	}
	return append(out, '\n')
}

func walk(v any, m map[string]string) any {
	switch t := v.(type) {
	case string:
		return replace(t, m)
	case []any:
		for i := range t {
			t[i] = walk(t[i], m)
		}
	case map[string]any:
		for k, e := range t {
			t[k] = walk(e, m)
		}
	}
	return v
}
