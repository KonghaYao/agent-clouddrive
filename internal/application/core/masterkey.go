package core

import (
	"sync"
	"time"
)

// MasterKeyStatusCache memoizes an external subject's authoritative enabled
// state for a bounded TTL. It is process-local and never stores credentials.
type MasterKeyStatusCache struct {
	mu      sync.Mutex
	entries map[string]masterKeyStatus
}

type masterKeyStatus struct {
	enabled bool
	until   time.Time
}

const maxMasterKeyStatusEntries = 4096

func NewMasterKeyStatusCache() *MasterKeyStatusCache {
	return &MasterKeyStatusCache{entries: map[string]masterKeyStatus{}}
}

func (c *MasterKeyStatusCache) Get(key string, now time.Time) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.entries[key]
	if !ok {
		return false, false
	}
	if !now.Before(v.until) {
		delete(c.entries, key)
		return false, false
	}
	return v.enabled, true
}

func (c *MasterKeyStatusCache) Put(key string, enabled bool, until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]masterKeyStatus{}
	}
	if len(c.entries) >= maxMasterKeyStatusEntries {
		now := time.Now()
		for k, v := range c.entries {
			if !now.Before(v.until) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= maxMasterKeyStatusEntries {
			c.entries = map[string]masterKeyStatus{}
		}
	}
	c.entries[key] = masterKeyStatus{enabled: enabled, until: until}
}
