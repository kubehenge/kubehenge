package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/luthermonson/go-proxmox"
)

// storageResource is a storage on one node as Proxmox VE's resource list shows it.
type storageResource struct {
	Storage string `json:"storage"`
	Node    string `json:"node"`
	Content string `json:"content"`
	Shared  int    `json:"shared"`
	Status  string `json:"status"`
	Disk    int64  `json:"disk"`
	MaxDisk int64  `json:"maxdisk"`
}

func (s storageResource) takes(content string) bool {
	return slices.Contains(strings.Split(s.Content, ","), content)
}

// defaultTemplateStorage is the shared storage with the most room that takes disk images on one of
// the nodes. Without one, the template could be cloned on its own node only, so the build asks for
// a storage to be named.
func defaultTemplateStorage(list []storageResource, nodes []string) (string, error) {
	free := map[string]int64{}
	for _, s := range list {
		if s.Shared == 0 || s.Status != "available" || !s.takes("images") || !slices.Contains(nodes, s.Node) {
			continue
		}
		free[s.Storage] = max(free[s.Storage], s.MaxDisk-s.Disk)
	}
	best, room := "", int64(-1)
	for name, f := range free {
		if f > room || f == room && name < best {
			best, room = name, f
		}
	}
	if best == "" {
		return "", errors.New("no shared storage takes disk images on " + strings.Join(nodes, ", ") +
			". Name the storage with --storage, and keep in mind that a template on node-local storage can be cloned on its own node only")
	}
	return best, nil
}

// sharedNote says, for a storage that is not shared, what that means for the template, or "".
func sharedNote(list []storageResource, storage string, nodes []string) string {
	i := slices.IndexFunc(list, func(s storageResource) bool { return s.Storage == storage })
	if i < 0 || list[i].Shared != 0 {
		return ""
	}
	return fmt.Sprintf("storage %s is not shared, so clones of this template can start only on node %s. "+
		"Use shared storage to let clusters run on every node", storage, strings.Join(nodes, ", "))
}

// resolveStorage fills in the builder's storage when none was given, and returns a note when the
// storage is not shared.
func resolveStorage(ctx context.Context, c *proxmox.Client, storage string, nodes []string) (chosen, note string, err error) {
	var list []storageResource
	if err = c.Get(ctx, "/cluster/resources?type=storage", &list); err != nil {
		if storage != "" {
			return storage, "", nil
		}
		return "", "", fmt.Errorf("listing the storages to choose one for the template: %w. Name the storage with --storage", err)
	}
	if storage == "" {
		if storage, err = defaultTemplateStorage(list, nodes); err != nil {
			return "", "", err
		}
	}
	return storage, sharedNote(list, storage, nodes[:1]), nil
}
