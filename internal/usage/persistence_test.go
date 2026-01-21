package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStripRequestDetails_RetentionWindow(t *testing.T) {
	now := time.Now()

	snapshot := StatisticsSnapshot{
		TotalRequests: 5,
		APIs: map[string]APISnapshot{
			"test-api": {
				TotalRequests: 5,
				TotalTokens:   500,
				Models: map[string]ModelSnapshot{
					"test-model": {
						TotalRequests: 5,
						TotalTokens:   500,
						Details: []RequestDetail{
							{Timestamp: now.Add(-40 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}}, // 40 days old - should be removed
							{Timestamp: now.Add(-35 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}}, // 35 days old - should be removed
							{Timestamp: now.Add(-25 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}}, // 25 days old - should be kept
							{Timestamp: now.Add(-10 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}}, // 10 days old - should be kept
							{Timestamp: now.Add(-1 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}},  // 1 day old - should be kept
						},
					},
				},
			},
		},
	}

	// Test with 30 days retention
	stripRequestDetails(&snapshot, 30)

	modelSnapshot := snapshot.APIs["test-api"].Models["test-model"]
	require.Len(t, modelSnapshot.Details, 3, "should keep only details within 30 days")

	// Verify the oldest kept detail is within retention window
	oldestKept := modelSnapshot.Details[0].Timestamp
	assert.True(t, oldestKept.After(now.Add(-30*24*time.Hour)), "oldest kept detail should be within 30 days")
}

func TestStripRequestDetails_DefaultRetention(t *testing.T) {
	now := time.Now()

	snapshot := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"test-api": {
				Models: map[string]ModelSnapshot{
					"test-model": {
						Details: []RequestDetail{
							{Timestamp: now.Add(-31 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}}, // should be removed
							{Timestamp: now.Add(-29 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}}, // should be kept
						},
					},
				},
			},
		},
	}

	// Test with 0 retention (should default to 30)
	stripRequestDetails(&snapshot, 0)

	modelSnapshot := snapshot.APIs["test-api"].Models["test-model"]
	require.Len(t, modelSnapshot.Details, 1, "should default to 30 days retention")
}

func TestSaveToFile_WithRetention(t *testing.T) {
	tmpDir := t.TempDir()
	statsPath := filepath.Join(tmpDir, "usage_stats.json")

	stats := NewRequestStatistics()
	now := time.Now()

	// Add some test data with different timestamps
	stats.mu.Lock()
	stats.apis["test-api"] = &apiStats{
		TotalRequests: 2,
		TotalTokens:   200,
		Models: map[string]*modelStats{
			"test-model": {
				TotalRequests: 2,
				TotalTokens:   200,
				Details: []RequestDetail{
					{Timestamp: now.Add(-40 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}}, // old
					{Timestamp: now.Add(-5 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}},  // recent
				},
			},
		},
	}
	stats.mu.Unlock()

	// Save with 30 days retention
	err := stats.SaveToFile(statsPath, 30)
	require.NoError(t, err, "SaveToFile should succeed")

	// Load and verify
	data, err := os.ReadFile(statsPath)
	require.NoError(t, err)

	var payload ExportPayload
	err = json.Unmarshal(data, &payload)
	require.NoError(t, err)

	modelSnapshot := payload.Usage.APIs["test-api"].Models["test-model"]
	assert.Len(t, modelSnapshot.Details, 1, "should only save recent details")
	assert.True(t, modelSnapshot.Details[0].Timestamp.After(now.Add(-30*24*time.Hour)),
		"saved detail should be within retention window")
}

func TestStripRequestDetails_EmptyDetails(t *testing.T) {
	snapshot := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"test-api": {
				Models: map[string]ModelSnapshot{
					"test-model": {
						TotalRequests: 10,
						TotalTokens:   1000,
						Details:       []RequestDetail{}, // empty details
					},
				},
			},
		},
	}

	stripRequestDetails(&snapshot, 30)

	// Should not panic and should preserve empty slice
	modelSnapshot := snapshot.APIs["test-api"].Models["test-model"]
	assert.Empty(t, modelSnapshot.Details, "empty details should remain empty")
}

func TestCleanupOldDetails(t *testing.T) {
	stats := NewRequestStatistics()
	now := time.Now()

	// Add test data with mixed old and new records
	stats.mu.Lock()
	stats.apis["test-api"] = &apiStats{
		TotalRequests: 5,
		TotalTokens:   500,
		Models: map[string]*modelStats{
			"test-model": {
				TotalRequests: 5,
				TotalTokens:   500,
				Details: []RequestDetail{
					{Timestamp: now.Add(-40 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}},
					{Timestamp: now.Add(-35 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}},
					{Timestamp: now.Add(-25 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}},
					{Timestamp: now.Add(-10 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}},
					{Timestamp: now.Add(-1 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}},
				},
			},
		},
	}
	stats.mu.Unlock()

	// Perform cleanup
	cleanupStats := stats.CleanupOldDetails(30)

	// Verify cleanup statistics
	assert.Equal(t, int64(5), cleanupStats.TotalDetailsBefore, "should count all details before cleanup")
	assert.Equal(t, int64(3), cleanupStats.TotalDetailsAfter, "should keep 3 details within 30 days")
	assert.Equal(t, int64(2), cleanupStats.DetailsRemoved, "should remove 2 old details")

	// Verify memory was actually cleaned
	stats.mu.RLock()
	actualDetails := stats.apis["test-api"].Models["test-model"].Details
	stats.mu.RUnlock()

	assert.Len(t, actualDetails, 3, "memory should only contain recent details")
	for _, detail := range actualDetails {
		assert.True(t, detail.Timestamp.After(now.Add(-30*24*time.Hour)),
			"all remaining details should be within retention window")
	}
}

func TestCleanupOldDetails_MultipleModels(t *testing.T) {
	stats := NewRequestStatistics()
	now := time.Now()

	// Add test data for multiple models
	stats.mu.Lock()
	stats.apis["api1"] = &apiStats{
		Models: map[string]*modelStats{
			"model-a": {
				Details: []RequestDetail{
					{Timestamp: now.Add(-40 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}},
					{Timestamp: now.Add(-10 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}},
				},
			},
			"model-b": {
				Details: []RequestDetail{
					{Timestamp: now.Add(-50 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}},
					{Timestamp: now.Add(-45 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}},
					{Timestamp: now.Add(-5 * 24 * time.Hour), Tokens: TokenStats{TotalTokens: 100}},
				},
			},
		},
	}
	stats.mu.Unlock()

	// Perform cleanup
	cleanupStats := stats.CleanupOldDetails(30)

	// Verify overall statistics
	assert.Equal(t, int64(5), cleanupStats.TotalDetailsBefore)
	assert.Equal(t, int64(2), cleanupStats.TotalDetailsAfter)
	assert.Equal(t, int64(3), cleanupStats.DetailsRemoved)

	// Verify each model was cleaned correctly
	stats.mu.RLock()
	assert.Len(t, stats.apis["api1"].Models["model-a"].Details, 1, "model-a should have 1 detail")
	assert.Len(t, stats.apis["api1"].Models["model-b"].Details, 1, "model-b should have 1 detail")
	stats.mu.RUnlock()
}

func TestCleanupOldDetails_DefaultRetention(t *testing.T) {
	stats := NewRequestStatistics()
	now := time.Now()

	stats.mu.Lock()
	stats.apis["test-api"] = &apiStats{
		Models: map[string]*modelStats{
			"test-model": {
				Details: []RequestDetail{
					{Timestamp: now.Add(-31 * 24 * time.Hour)},
					{Timestamp: now.Add(-29 * 24 * time.Hour)},
				},
			},
		},
	}
	stats.mu.Unlock()

	// Test with 0 retention (should default to 30)
	cleanupStats := stats.CleanupOldDetails(0)

	assert.Equal(t, int64(2), cleanupStats.TotalDetailsBefore)
	assert.Equal(t, int64(1), cleanupStats.TotalDetailsAfter)
	assert.Equal(t, int64(1), cleanupStats.DetailsRemoved)
}

func TestCleanupOldDetails_NoOldData(t *testing.T) {
	stats := NewRequestStatistics()
	now := time.Now()

	// Add only recent data
	stats.mu.Lock()
	stats.apis["test-api"] = &apiStats{
		Models: map[string]*modelStats{
			"test-model": {
				Details: []RequestDetail{
					{Timestamp: now.Add(-5 * 24 * time.Hour)},
					{Timestamp: now.Add(-1 * 24 * time.Hour)},
				},
			},
		},
	}
	stats.mu.Unlock()

	// Cleanup should not remove anything
	cleanupStats := stats.CleanupOldDetails(30)

	assert.Equal(t, int64(2), cleanupStats.TotalDetailsBefore)
	assert.Equal(t, int64(2), cleanupStats.TotalDetailsAfter)
	assert.Equal(t, int64(0), cleanupStats.DetailsRemoved, "no old data should be removed")
}
