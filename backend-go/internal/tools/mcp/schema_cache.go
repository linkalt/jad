package mcp

import (
	"container/list"
	"crypto/sha256"
	"sync"

	"jad/backend-go/internal/tools"
)

// defaultSchemaCacheEntries bounds how many compiled input schemas one Manager keeps.
const defaultSchemaCacheEntries = 512

// schemaCache keeps compiled input schemas keyed by the SHA-256 of the schema bytes, which are
// the only input of tools.CompileSchema. A compiled validator is read-only, so it is shared by
// every caller. Failures are not cached. The least recently used entry is evicted at the bound.
type schemaCache struct {
	mu      sync.Mutex
	max     int
	compile func([]byte) (*tools.SchemaValidator, error)
	order   *list.List
	entries map[[sha256.Size]byte]*list.Element
}

type schemaCacheEntry struct {
	key       [sha256.Size]byte
	validator *tools.SchemaValidator
}

func newSchemaCache(max int) *schemaCache {
	return &schemaCache{
		max:     max,
		compile: tools.CompileSchema,
		order:   list.New(),
		entries: make(map[[sha256.Size]byte]*list.Element),
	}
}

func (c *schemaCache) get(schema []byte) (*tools.SchemaValidator, error) {
	key := sha256.Sum256(schema)
	c.mu.Lock()
	if element, ok := c.entries[key]; ok {
		c.order.MoveToFront(element)
		validator := element.Value.(*schemaCacheEntry).validator
		c.mu.Unlock()
		return validator, nil
	}
	c.mu.Unlock()

	validator, err := c.compile(schema)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[key]; ok {
		c.order.MoveToFront(element)
		return element.Value.(*schemaCacheEntry).validator, nil
	}
	c.entries[key] = c.order.PushFront(&schemaCacheEntry{key: key, validator: validator})
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*schemaCacheEntry).key)
	}
	return validator, nil
}

func (c *schemaCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
