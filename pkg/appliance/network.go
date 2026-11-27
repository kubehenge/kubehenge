package appliance

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// NetworkFile is the file in the appliance's directory that holds its container's network cards as
// Proxmox VE configures them, one "netN: <value>" line each. The installer writes it on every
// install and upgrade, and the interface whenever it reads the container's configuration, so it
// follows a card that was added or moved. Upgrades and their rollbacks leave it alone.
const NetworkFile = "network"

var cardKey = regexp.MustCompile(`^net(\d+)$`)

// NetworkCards are the network cards among a container's configuration, given by its keys and
// values, as the lines of NetworkFile in the order of their number. Nil where it has none.
func NetworkCards(config map[string]string) []byte {
	type card struct {
		n    int
		line string
	}
	var cards []card
	for key, value := range config {
		m := cardKey.FindStringSubmatch(key)
		value = strings.TrimSpace(value)
		if m == nil || value == "" || strings.ContainsAny(value, "\r\n") {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		cards = append(cards, card{n, key + ": " + value + "\n"})
	}
	slices.SortFunc(cards, func(a, b card) int { return cmp.Compare(a.n, b.n) })
	var out []byte
	for _, c := range cards {
		out = append(out, c.line...)
	}
	return out
}

// RecordNetwork writes the network cards of the container's configuration into NetworkFile where
// they differ from what it holds. A configuration without cards leaves the file as it is.
func (a *Appliance) RecordNetwork(config map[string]string) error {
	cards := NetworkCards(config)
	if len(cards) == 0 {
		return nil
	}
	path := filepath.Join(a.Dir, NetworkFile)
	if held, err := os.ReadFile(path); err == nil && bytes.Equal(held, cards) { //nolint:gosec // the appliance's own file
		return nil
	}
	return writeFileAtomic(path, cards)
}

// segments are the bridge, or VNet by its name, and the VLAN tag of each card NetworkFile holds,
// as "<bridge>/<vlan>" by the name of its interface in the container. None while the file is
// missing.
func (a *Appliance) segments() (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(a.Dir, NetworkFile))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the appliance's network: %w", err)
	}
	segments := map[string]string{}
	for line := range strings.Lines(string(data)) {
		if name, segment := cardSegment(line); name != "" {
			segments[name] = segment
		}
	}
	return segments, nil
}

// cardSegment is the interface name and the "<bridge>/<vlan>" of a line of NetworkFile, such as
// "net1: name=eth1,bridge=vmbr1,hwaddr=BC:24:11:2E:5D:6A,ip=dhcp,tag=30,type=veth". The name is
// empty where the line names no interface or bridge, or a tag that is no number.
func cardSegment(line string) (string, string) {
	_, value, ok := strings.Cut(strings.TrimSpace(line), ": ")
	if !ok {
		return "", ""
	}
	var name, bridge string
	vlan := 0
	var err error
	for part := range strings.SplitSeq(value, ",") {
		if v, ok := strings.CutPrefix(part, "name="); ok {
			name = v
		} else if v, ok := strings.CutPrefix(part, "bridge="); ok {
			bridge = v
		} else if v, ok := strings.CutPrefix(part, "tag="); ok {
			if vlan, err = strconv.Atoi(v); err != nil {
				return "", ""
			}
		}
	}
	if name == "" || bridge == "" {
		return "", ""
	}
	return name, fmt.Sprintf("%s/%d", bridge, vlan)
}
