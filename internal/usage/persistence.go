package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const usageStatsFileName = "usage_stats.json"

var persistenceMu sync.Mutex

// StatsFilePath builds the default usage stats persistence path under auth dir.
func StatsFilePath(authDir string) string {
	if authDir == "" {
		return ""
	}
	return filepath.Join(authDir, usageStatsFileName)
}

// LoadFromFile replaces the in-memory statistics with snapshot loaded from disk.
func (s *RequestStatistics) LoadFromFile(path string) error {
	if s == nil || path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read usage stats: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	var payload ImportPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("parse usage stats: %w", err)
	}
	if payload.Version != 0 && payload.Version != 1 {
		return fmt.Errorf("unsupported usage stats version: %d", payload.Version)
	}
	s.Replace(payload.Usage)
	return nil
}

// SaveToFile persists the current statistics snapshot to disk.
func (s *RequestStatistics) SaveToFile(path string) error {
	if s == nil || path == "" {
		return nil
	}
	snapshot := s.Snapshot()
	stripRequestDetails(&snapshot)

	payload := ExportPayload{
		Version:    1,
		ExportedAt: time.Now().UTC(),
		Usage:      snapshot,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("encode usage stats: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("prepare usage stats dir: %w", err)
	}

	persistenceMu.Lock()
	defer persistenceMu.Unlock()

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write usage stats: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("finalize usage stats: %w", err)
	}
	return nil
}

// StartAutoSave periodically persists usage statistics until context is canceled.
func (s *RequestStatistics) StartAutoSave(ctx context.Context, path string, interval time.Duration) {
	if s == nil || path == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		go func() {
			<-ctx.Done()
			_ = s.SaveToFile(path)
		}()
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := s.SaveToFile(path); err != nil {
					log.WithError(err).Warn("failed to auto-save usage statistics")
				}
			case <-ctx.Done():
				if err := s.SaveToFile(path); err != nil {
					log.WithError(err).Warn("failed to save usage statistics on shutdown")
				}
				return
			}
		}
	}()
}

func stripRequestDetails(snapshot *StatisticsSnapshot) {
	if snapshot == nil || len(snapshot.APIs) == 0 {
		return
	}
	for apiKey, apiStats := range snapshot.APIs {
		if len(apiStats.Models) == 0 {
			continue
		}
		models := make(map[string]ModelSnapshot, len(apiStats.Models))
		for modelName, modelStats := range apiStats.Models {
			modelStats.Details = nil
			models[modelName] = modelStats
		}
		apiStats.Models = models
		snapshot.APIs[apiKey] = apiStats
	}
}
