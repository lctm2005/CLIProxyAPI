package cliproxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"

	logs "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

type traeCLIModelsSnapshot struct {
	hash    [sha256.Size]byte
	entries []traeCLIModelCacheEntry
}

// traeCLIModelsCache retains immutable, validated snapshots per source path.
// Reads are serialized so an older read cannot overwrite a newer snapshot.
type traeCLIModelsCache struct {
	mu        sync.Mutex
	snapshots map[string]*traeCLIModelsSnapshot
	errors    map[string]string
}

func (c *traeCLIModelsCache) load(ctx context.Context, path string) *traeCLIModelsSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	previous := c.snapshots[path]
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		c.reportError(ctx, path, errRead)
		return previous
	}
	hash := sha256.Sum256(raw)
	if previous != nil && previous.hash == hash {
		delete(c.errors, path)
		return previous
	}
	var cache struct {
		Models []*traeCLIModelCacheEntry `json:"models"`
	}
	if errParse := json.Unmarshal(raw, &cache); errParse != nil {
		c.reportError(ctx, path, errParse)
		return previous
	}
	if cache.Models == nil {
		c.reportError(ctx, path, errors.New("models must be a JSON array"))
		return previous
	}
	entries := make([]traeCLIModelCacheEntry, 0, len(cache.Models))
	for _, entry := range cache.Models {
		if entry == nil || (entry.SupportedInAPI && strings.TrimSpace(entry.ConfigName) == "" && strings.TrimSpace(entry.Slug) == "") {
			c.reportError(ctx, path, errors.New("model entries must be objects with an API model identifier"))
			return previous
		}
		entries = append(entries, *entry)
	}
	snapshot := &traeCLIModelsSnapshot{hash: hash, entries: entries}
	if c.snapshots == nil {
		c.snapshots = make(map[string]*traeCLIModelsSnapshot)
	}
	c.snapshots[path] = snapshot
	delete(c.errors, path)
	return snapshot
}

// reportError only logs a changed error until the source becomes valid again.
// c.mu must be held.
func (c *traeCLIModelsCache) reportError(ctx context.Context, path string, err error) {
	if c.errors == nil {
		c.errors = make(map[string]string)
	}
	if c.errors[path] == err.Error() {
		return
	}
	c.errors[path] = err.Error()
	logs.CtxError(ctx, "failed to load TRAE model cache %s; retaining the last valid catalog: %v", path, err)
}
