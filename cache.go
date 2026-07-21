package main

import (
	"container/list"
	"sync"

	"golang.org/x/sync/singleflight"
)

// resultCache memoizes engine responses for deterministic models (config
// "cache": true — full-strength analysis only; sampled engines like maia
// must never opt in). Concurrent identical requests coalesce into one
// upstream call via singleflight; settled results live in a size-capped LRU.
// Positions never go stale, so there is no TTL — a restart clears it.
type resultCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List // front = most recently used
	max     int
	group   singleflight.Group
}

type cacheEntry struct {
	key   string
	value engineResponse
}

func newResultCache(max int) *resultCache {
	return &resultCache{
		entries: map[string]*list.Element{},
		order:   list.New(),
		max:     max,
	}
}

// get returns the cached response for key and refreshes its recency.
func (c *resultCache) get(key string) (engineResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.entries[key]
	if !ok {
		return engineResponse{}, false
	}
	c.order.MoveToFront(elem)
	return elem.Value.(*cacheEntry).value, true
}

func (c *resultCache) put(key string, value engineResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.entries[key]; ok {
		c.order.MoveToFront(elem)
		elem.Value.(*cacheEntry).value = value
		return
	}
	c.entries[key] = c.order.PushFront(&cacheEntry{key: key, value: value})
	if c.order.Len() > c.max {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*cacheEntry).key)
	}
}

// do returns the cached response for key, or runs fetch — once, no matter
// how many identical requests arrive concurrently — and caches its result.
// hit reports whether the response came from the cache; coalesced waiters
// report a miss even though they cost no engine call. Errors are returned
// to every waiter and never cached.
func (c *resultCache) do(key string, fetch func() (engineResponse, error)) (resp engineResponse, hit bool, err error) {
	if resp, ok := c.get(key); ok {
		return resp, true, nil
	}
	v, err, _ := c.group.Do(key, func() (any, error) {
		resp, err := fetch()
		if err != nil {
			return nil, err
		}
		c.put(key, resp)
		return resp, nil
	})
	if err != nil {
		return engineResponse{}, false, err
	}
	return v.(engineResponse), false, nil
}
