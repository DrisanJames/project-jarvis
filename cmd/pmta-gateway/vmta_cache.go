package main

import (
	"bufio"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// vmtaCache maintains a set of known VMTA names by periodically parsing
// `pmta show vmtas` output. Used to reject injections targeting
// non-existent VMTAs (which would route to {default}).
type vmtaCache struct {
	mu     sync.RWMutex
	vmtas  map[string]bool
	count  int
	loaded bool
}

func newVMTACache() *vmtaCache {
	return &vmtaCache{
		vmtas: make(map[string]bool),
	}
}

// Has returns true if the VMTA name exists in the cache.
// Before the first successful load, all names are accepted.
func (c *vmtaCache) Has(name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.loaded {
		return true
	}
	return c.vmtas[name]
}

// Count returns the number of known VMTAs.
func (c *vmtaCache) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.count
}

// refresh parses `pmta show vmtas` and updates the cache.
func (c *vmtaCache) refresh() {
	cmd := exec.Command("pmta", "show", "vmtas")
	out, err := cmd.Output()
	if err != nil {
		log.Printf("[vmta-cache] pmta show vmtas failed: %v, trying sudo", err)
		cmd = exec.Command("sudo", "pmta", "show", "vmtas")
		out, err = cmd.Output()
		if err != nil {
			log.Printf("[vmta-cache] sudo pmta show vmtas failed: %v", err)
			return
		}
	}

	names := make(map[string]bool)
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "Name") || strings.HasPrefix(line, "----") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 {
			names[fields[0]] = true
		}
	}

	if len(names) > 0 {
		c.mu.Lock()
		c.vmtas = names
		c.count = len(names)
		c.loaded = true
		c.mu.Unlock()
		log.Printf("[vmta-cache] refreshed: %d VMTAs loaded", len(names))
	}
}

// StartRefreshLoop starts a background goroutine that refreshes the VMTA
// cache at the specified interval. Performs an immediate first refresh.
func (c *vmtaCache) StartRefreshLoop(interval time.Duration) {
	c.refresh()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			c.refresh()
		}
	}()
}
