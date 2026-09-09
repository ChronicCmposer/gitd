// Package objectstore defines the consumer-side storage seam for bundle
// mirrors (R6-Q7): Put/Get/List/Delete. The interface lives here, at the
// consumer side, so the core never depends on a concrete backend; the AWS
// implementation lives in objectstore/s3, and tests use the in-memory fake in
// this package.
package objectstore

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// Store is the storage seam for git bundle mirrors (R6-Q7, R8-Q2). Keys are
// flat strings (for s3: "repos/<repo>/<timestamp>.bundle"); implementations
// may interpret a prefix.
type Store interface {
	// Put stores data under key, overwriting any existing object.
	Put(ctx context.Context, key string, data []byte) error
	// Get returns the object stored under key.
	Get(ctx context.Context, key string) ([]byte, error)
	// List returns all keys with the given prefix, sorted ascending.
	List(ctx context.Context, prefix string) ([]string, error)
	// Delete removes the object under key. Deleting a missing key is not an
	// error.
	Delete(ctx context.Context, key string) error
}

// MemoryStore is an in-memory Store for tests and local development.
// It is safe for concurrent use.
type MemoryStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{objects: make(map[string][]byte)}
}

// Put implements Store.
func (m *MemoryStore) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = append([]byte(nil), data...)
	return nil
}

// Get implements Store.
func (m *MemoryStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[key]
	if !ok {
		return nil, &NotFoundError{Key: key}
	}
	return append([]byte(nil), data...), nil
}

// List implements Store.
func (m *MemoryStore) List(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// Delete implements Store.
func (m *MemoryStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// NotFoundError reports a missing object; callers can errors.As it to
// distinguish "no bundle yet" from backend failures.
type NotFoundError struct {
	Key string
}

// Error implements error.
func (e *NotFoundError) Error() string { return "object not found: " + e.Key }
