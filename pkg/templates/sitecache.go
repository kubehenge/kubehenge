package templates

import "sync"

// SiteCache keeps where each template's disks are and what its build recorded, by VMID. A
// template's configuration is read-only, and a VMID that leaves the listing is dropped, so a new VM
// with the same ID is read afresh.
type SiteCache struct {
	mu      sync.Mutex
	entries map[int]cachedSite
}

type cachedSite struct {
	name string
	site Site
	// facts is nil for a template whose build recorded none.
	facts *BuildFacts
}

// Len is the number of templates held.
func (c *SiteCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// get is safe on a nil cache, which holds nothing.
func (c *SiteCache) get(id int) (cachedSite, bool) {
	if c == nil {
		return cachedSite{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[id]
	return e, ok
}

func (c *SiteCache) put(id int, e cachedSite) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[int]cachedSite{}
	}
	c.entries[id] = e
}

// keepOnly drops the entries of VMIDs that are no longer listed.
func (c *SiteCache) keepOnly(listed map[int]bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.entries {
		if !listed[id] {
			delete(c.entries, id)
		}
	}
}
