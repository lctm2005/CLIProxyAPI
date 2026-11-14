package auth

import (
	"context"
	"time"

	logs "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func (m *Manager) bumpModelCatalogVersionLocked(authID string) {
	if m.modelCatalogVersions == nil {
		m.modelCatalogVersions = make(map[string]uint64)
	}
	m.modelCatalogSequence++
	m.modelCatalogVersions[authID] = m.modelCatalogSequence
}

// ModelCatalogSnapshot captures the auth configuration and its revision for a
// later catalog update. Runtime request results do not invalidate the revision.
func (m *Manager) ModelCatalogSnapshot(authID string) (*Auth, uint64) {
	if m == nil {
		return nil, 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth := m.auths[authID]
	if auth == nil {
		return nil, 0
	}
	return auth.Clone(), m.modelCatalogVersions[authID]
}

// UpdateModelCatalog publishes discovery changes without resetting runtime
// cooldowns. A removed, disabled, or replaced auth rejects stale snapshots.
func (m *Manager) UpdateModelCatalog(ctx context.Context, authID string, expectedVersion uint64, models []*registry.ModelInfo) bool {
	if m == nil || expectedVersion == 0 {
		return false
	}
	m.modelCatalogMu.Lock()
	defer m.modelCatalogMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	auth := m.auths[authID]
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || m.modelCatalogVersions[authID] != expectedVersion {
		return false
	}

	reg := registry.GetGlobalRegistry()
	previous := reg.GetModelsForClient(authID)
	supported := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model != nil && model.ID != "" {
			supported[canonicalModelKey(model.ID)] = struct{}{}
		}
	}
	if m.removedCatalogModels == nil {
		m.removedCatalogModels = make(map[string]map[string]struct{})
	}
	removed := m.removedCatalogModels[authID]
	if removed == nil {
		removed = make(map[string]struct{})
		m.removedCatalogModels[authID] = removed
	}
	for _, model := range previous {
		key := canonicalModelKey(model.ID)
		if _, retained := supported[key]; !retained {
			removed[key] = struct{}{}
			// Older internally tracked requests remain stale even if this model
			// is subsequently added back to the catalog.
			if m.resultGenerationWatermarks == nil {
				m.resultGenerationWatermarks = make(map[resultGenerationKey]uint64)
			}
			m.resultGenerationWatermarks[resultGenerationKey{authID: authID, model: key}] = m.nextResultGeneration()
		}
	}
	for key := range supported {
		delete(removed, key)
	}
	reg.UpdateClientModels(authID, auth.Provider, models)
	auth.Generation++
	changed := false
	for key := range auth.ModelStates {
		if _, retained := supported[canonicalModelKey(key)]; !retained {
			delete(auth.ModelStates, key)
			removed[canonicalModelKey(key)] = struct{}{}
			changed = true
		}
	}
	if changed {
		now := time.Now()
		updateAggregatedAvailability(auth, now)
		if !hasModelError(auth, now) && !(auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now)) {
			auth.LastError = nil
			auth.StatusMessage = ""
			auth.Status = StatusActive
		}
		auth.UpdatedAt = now
		if errPersist := m.persist(ctx, auth); errPersist != nil {
			logs.CtxError(ctx, "failed to persist auth %s after model catalog update: %v", authID, errPersist)
		}
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(auth.Clone())
	}
	m.bumpModelCatalogVersionLocked(authID)
	return true
}
