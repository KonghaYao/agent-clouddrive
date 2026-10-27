package core

import (
	"testing"
	"time"
)

func TestMasterKeyStatusCacheTTL(t *testing.T) {
	c := NewMasterKeyStatusCache()
	now := time.Unix(100, 0)
	if _, ok := c.Get("user", now); ok {
		t.Fatal("empty cache returned a value")
	}
	c.Put("user", false, now.Add(time.Minute))
	if enabled, ok := c.Get("user", now.Add(time.Second)); !ok || enabled {
		t.Fatalf("cached disabled state wrong: %v %v", enabled, ok)
	}
	if _, ok := c.Get("user", now.Add(time.Minute)); ok {
		t.Fatal("expired entry returned a value")
	}
	c.Put("user", true, now.Add(time.Minute))
	if enabled, ok := c.Get("user", now.Add(2*time.Minute)); ok || enabled {
		t.Fatal("stale entry must not be returned")
	}
}

func TestMasterKeyStatusCacheIsBounded(t *testing.T) {
	c := NewMasterKeyStatusCache()
	now := time.Now()
	for i := 0; i < maxMasterKeyStatusEntries+10; i++ {
		c.Put(string(rune(i+1))+"-key", true, now.Add(time.Minute))
	}
	c.mu.Lock()
	size := len(c.entries)
	c.mu.Unlock()
	if size > maxMasterKeyStatusEntries {
		t.Fatalf("cache exceeded bound: %d", size)
	}
}
