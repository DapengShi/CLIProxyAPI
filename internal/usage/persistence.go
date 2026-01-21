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
// retentionDays controls how many days of detailed request information to retain.
// When <= 0, defaults to 30 days.
func (s *RequestStatistics) SaveToFile(path string, retentionDays int) error {
	if s == nil || path == "" {
		return nil
	}
	snapshot := s.Snapshot()
	stripRequestDetails(&snapshot, retentionDays)

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
// retentionDays controls how many days of detailed request information to retain.
// Memory cleanup is performed before each save to reduce memory footprint and improve performance.
func (s *RequestStatistics) StartAutoSave(ctx context.Context, path string, interval time.Duration, retentionDays int) {
	if s == nil || path == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		go func() {
			<-ctx.Done()
			s.cleanupAndSave(path, retentionDays)
		}()
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.cleanupAndSave(path, retentionDays)
			case <-ctx.Done():
				s.cleanupAndSave(path, retentionDays)
				return
			}
		}
	}()
}

// cleanupAndSave performs memory cleanup before saving to improve performance.
func (s *RequestStatistics) cleanupAndSave(path string, retentionDays int) {
	// Clean up old details from memory first
	stats := s.CleanupOldDetails(retentionDays)

	// Log cleanup metrics if significant cleanup occurred
	if stats.DetailsRemoved > 0 {
		removalRatio := 0.0
		if stats.TotalDetailsBefore > 0 {
			removalRatio = float64(stats.DetailsRemoved) / float64(stats.TotalDetailsBefore)
		}
		log.WithFields(log.Fields{
			"details_before": stats.TotalDetailsBefore,
			"details_after":  stats.TotalDetailsAfter,
			"details_removed": stats.DetailsRemoved,
			"removal_ratio":  fmt.Sprintf("%.1f%%", removalRatio*100),
		}).Info("usage statistics memory cleanup completed")
	}

	// Now save to file (much faster since old data is already removed)
	if err := s.SaveToFile(path, retentionDays); err != nil {
		log.WithError(err).Warn("failed to save usage statistics")
	}
}

func stripRequestDetails(snapshot *StatisticsSnapshot, retentionDays int) {
	if snapshot == nil || len(snapshot.APIs) == 0 {
		return
	}
	if retentionDays <= 0 {
		retentionDays = 30
	}
	cutoffTime := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)

	for apiKey, apiStats := range snapshot.APIs {
		if len(apiStats.Models) == 0 {
			continue
		}
		models := make(map[string]ModelSnapshot, len(apiStats.Models))
		for modelName, modelStats := range apiStats.Models {
			if len(modelStats.Details) == 0 {
				models[modelName] = modelStats
				continue
			}
			// Filter details to keep only those within retention window
			filteredDetails := make([]RequestDetail, 0, len(modelStats.Details))
			for _, detail := range modelStats.Details {
				if detail.Timestamp.After(cutoffTime) {
					filteredDetails = append(filteredDetails, detail)
				}
			}
			modelStats.Details = filteredDetails
			models[modelName] = modelStats
		}
		apiStats.Models = models
		snapshot.APIs[apiKey] = apiStats
	}
}
