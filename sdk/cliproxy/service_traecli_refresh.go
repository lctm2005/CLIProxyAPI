package cliproxy

import (
	"context"
	"reflect"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

type traeCLIModelUpdate struct {
	config      *config.Config
	sequence    uint64
	authID      string
	authVersion uint64
	models      []*ModelInfo
}

func (s *Service) startTraeCLIModelRefresh(ctx context.Context, interval time.Duration) {
	if s == nil || s.coreManager == nil || interval <= 0 {
		return
	}
	s.traeRefreshMu.Lock()
	defer s.traeRefreshMu.Unlock()
	if s.traeRefreshCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.traeRefreshCancel, s.traeRefreshDone = cancel, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		// Cover cache changes during startup before waiting for the first tick.
		s.syncTraeCLIModels(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.syncTraeCLIModels(ctx)
			}
		}
	}()
}

func (s *Service) stopTraeCLIModelRefresh() {
	if s == nil {
		return
	}
	s.traeRefreshMu.Lock()
	defer s.traeRefreshMu.Unlock()
	if s.traeRefreshCancel != nil {
		s.traeRefreshCancel()
		<-s.traeRefreshDone
		s.traeRefreshCancel, s.traeRefreshDone = nil, nil
	}
}

func (s *Service) syncTraeCLIModels(ctx context.Context) int {
	updated := 0
	for _, candidate := range s.collectTraeCLIModelUpdates(ctx) {
		if s.publishTraeCLIModelUpdate(ctx, candidate) {
			updated++
		}
	}
	return updated
}

// collectTraeCLIModelUpdates performs file I/O outside config and auth commit
// locks. Each source is read once and all model construction uses that snapshot.
func (s *Service) collectTraeCLIModelUpdates(ctx context.Context) []traeCLIModelUpdate {
	if s == nil || s.coreManager == nil || ctx.Err() != nil {
		return nil
	}
	s.configUpdateMu.Lock()
	s.cfgMu.RLock()
	cfg, sequence := s.cfg, s.configSequence
	s.cfgMu.RUnlock()
	s.configUpdateMu.Unlock()
	if cfg == nil || cfg.Home.Enabled || !cfg.TraeCLI.Enabled || (len(cfg.TraeCLI.Models) > 0 && !cfg.TraeCLI.IncludeCacheModels) {
		return nil
	}
	// Plugin-owned TRAE accounts retain their own model discovery lifecycle.
	if s.pluginHost != nil {
		if exec, ok := s.coreManager.Executor("traecli"); ok && s.pluginHost.OwnsExecutor(exec) {
			return nil
		}
		if pluginHostHasAuthProvider(s.pluginHost, "traecli") {
			return nil
		}
	}
	snapshots := make(map[string]*traeCLIModelsSnapshot)
	var updates []traeCLIModelUpdate
	for _, listed := range s.coreManager.List() {
		if ctx.Err() != nil {
			break
		}
		if !strings.EqualFold(strings.TrimSpace(listed.Provider), "traecli") {
			continue
		}
		auth, version := s.coreManager.ModelCatalogSnapshot(listed.ID)
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled || !strings.EqualFold(strings.TrimSpace(auth.Provider), "traecli") {
			continue
		}
		path := traeCLIModelsCachePathForConfig(auth, cfg)
		snapshot, read := snapshots[path]
		if !read {
			snapshot = s.traeModelsCache.load(ctx, path)
			snapshots[path] = snapshot
		}
		if snapshot == nil {
			// No valid source yet: retain the cold-start registration fallback.
			continue
		}
		models := s.buildTraeCLIModelsForAuth(auth, cfg, snapshot)
		updates = append(updates, traeCLIModelUpdate{config: cfg, sequence: sequence, authID: auth.ID, authVersion: version, models: models})
	}
	return updates
}

func (s *Service) publishTraeCLIModelUpdate(ctx context.Context, update traeCLIModelUpdate) bool {
	s.configRuntimeMu.Lock()
	defer s.configRuntimeMu.Unlock()
	s.configUpdateMu.Lock()
	defer s.configUpdateMu.Unlock()
	if ctx.Err() != nil || s.configSequence != update.sequence {
		return false
	}
	s.cfgMu.RLock()
	current := s.cfg == update.config
	s.cfgMu.RUnlock()
	if !current {
		return false
	}
	previous := registry.GetGlobalRegistry().GetModelsForClient(update.authID)
	if traeModelCatalogEqual(previous, update.models) {
		return false
	}
	if !s.coreManager.UpdateModelCatalog(coreauth.WithSkipPersist(ctx), update.authID, update.authVersion, update.models) {
		return false
	}
	log.Infof("TRAE model catalog updated for %s: %d models", update.authID, len(update.models))
	return true
}

// Created is generated locally, and ordering is not a catalog change. Preserve
// existing creation times when a real metadata or membership update is needed.
func traeModelCatalogEqual(previous, next []*ModelInfo) bool {
	old := make(map[string]*ModelInfo, len(previous))
	for _, model := range previous {
		old[model.ID] = model
	}
	equal := len(previous) == len(next)
	for _, model := range next {
		prior := old[model.ID]
		if prior == nil {
			equal = false
			continue
		}
		model.Created = prior.Created
		if !reflect.DeepEqual(prior, model) {
			equal = false
		}
	}
	return equal
}
