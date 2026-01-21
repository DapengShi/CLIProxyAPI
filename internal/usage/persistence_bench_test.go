package usage

import (
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkSaveToFile_WithoutCleanup benchmarks save performance when memory contains old data
func BenchmarkSaveToFile_WithoutCleanup(b *testing.B) {
	tmpDir := b.TempDir()
	statsPath := filepath.Join(tmpDir, "usage_stats.json")

	stats := NewRequestStatistics()
	now := time.Now()

	// Simulate 100k requests over 90 days (most are old data)
	stats.mu.Lock()
	stats.apis["test-api"] = &apiStats{
		Models: map[string]*modelStats{
			"test-model": {
				Details: make([]RequestDetail, 100000),
			},
		},
	}
	model := stats.apis["test-api"].Models["test-model"]
	for i := 0; i < 100000; i++ {
		// 70% old data (31-90 days), 30% recent (0-30 days)
		daysOld := 31 + (i % 60)
		if i%10 < 3 {
			daysOld = i % 30
		}
		model.Details[i] = RequestDetail{
			Timestamp: now.Add(-time.Duration(daysOld) * 24 * time.Hour),
			Tokens:    TokenStats{TotalTokens: 100},
		}
	}
	stats.mu.Unlock()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = stats.SaveToFile(statsPath, 30)
	}
}

// BenchmarkSaveToFile_WithCleanup benchmarks save performance after memory cleanup
func BenchmarkSaveToFile_WithCleanup(b *testing.B) {
	tmpDir := b.TempDir()
	statsPath := filepath.Join(tmpDir, "usage_stats.json")

	stats := NewRequestStatistics()
	now := time.Now()

	// Same setup: 100k requests over 90 days
	stats.mu.Lock()
	stats.apis["test-api"] = &apiStats{
		Models: map[string]*modelStats{
			"test-model": {
				Details: make([]RequestDetail, 100000),
			},
		},
	}
	model := stats.apis["test-api"].Models["test-model"]
	for i := 0; i < 100000; i++ {
		daysOld := 31 + (i % 60)
		if i%10 < 3 {
			daysOld = i % 30
		}
		model.Details[i] = RequestDetail{
			Timestamp: now.Add(-time.Duration(daysOld) * 24 * time.Hour),
			Tokens:    TokenStats{TotalTokens: 100},
		}
	}
	stats.mu.Unlock()

	// Clean up old data first (this is what the optimized flow does)
	stats.CleanupOldDetails(30)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = stats.SaveToFile(statsPath, 30)
	}
}

// BenchmarkCleanupOldDetails benchmarks the cleanup operation itself
func BenchmarkCleanupOldDetails(b *testing.B) {
	now := time.Now()

	// Create test data once
	testData := make([]RequestDetail, 100000)
	for i := 0; i < 100000; i++ {
		daysOld := 31 + (i % 60)
		if i%10 < 3 {
			daysOld = i % 30
		}
		testData[i] = RequestDetail{
			Timestamp: now.Add(-time.Duration(daysOld) * 24 * time.Hour),
			Tokens:    TokenStats{TotalTokens: 100},
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		stats := NewRequestStatistics()
		stats.mu.Lock()
		stats.apis["test-api"] = &apiStats{
			Models: map[string]*modelStats{
				"test-model": {
					Details: append([]RequestDetail{}, testData...),
				},
			},
		}
		stats.mu.Unlock()
		b.StartTimer()

		stats.CleanupOldDetails(30)
	}
}

// BenchmarkSnapshot_WithOldData benchmarks snapshot creation with old data
func BenchmarkSnapshot_WithOldData(b *testing.B) {
	stats := NewRequestStatistics()
	now := time.Now()

	stats.mu.Lock()
	stats.apis["test-api"] = &apiStats{
		Models: map[string]*modelStats{
			"test-model": {
				Details: make([]RequestDetail, 100000),
			},
		},
	}
	for i := 0; i < 100000; i++ {
		stats.apis["test-api"].Models["test-model"].Details[i] = RequestDetail{
			Timestamp: now.Add(-time.Duration(i%90) * 24 * time.Hour),
			Tokens:    TokenStats{TotalTokens: 100},
		}
	}
	stats.mu.Unlock()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = stats.Snapshot()
	}
}

// BenchmarkSnapshot_AfterCleanup benchmarks snapshot creation after cleanup
func BenchmarkSnapshot_AfterCleanup(b *testing.B) {
	stats := NewRequestStatistics()
	now := time.Now()

	stats.mu.Lock()
	stats.apis["test-api"] = &apiStats{
		Models: map[string]*modelStats{
			"test-model": {
				Details: make([]RequestDetail, 100000),
			},
		},
	}
	for i := 0; i < 100000; i++ {
		stats.apis["test-api"].Models["test-model"].Details[i] = RequestDetail{
			Timestamp: now.Add(-time.Duration(i%90) * 24 * time.Hour),
			Tokens:    TokenStats{TotalTokens: 100},
		}
	}
	stats.mu.Unlock()

	// Clean up first
	stats.CleanupOldDetails(30)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = stats.Snapshot()
	}
}

// BenchmarkEndToEnd_AutoSave simulates the complete auto-save cycle
func BenchmarkEndToEnd_AutoSave(b *testing.B) {
	tmpDir := b.TempDir()
	statsPath := filepath.Join(tmpDir, "usage_stats.json")

	stats := NewRequestStatistics()
	now := time.Now()

	// Simulate realistic data: 10k requests per day for 90 days = 900k total
	stats.mu.Lock()
	stats.apis["test-api"] = &apiStats{
		Models: map[string]*modelStats{
			"test-model": {
				Details: make([]RequestDetail, 300000), // Use 300k for faster benchmark
			},
		},
	}
	for i := 0; i < 300000; i++ {
		daysOld := i / 3333 // Distribute evenly over 90 days
		stats.apis["test-api"].Models["test-model"].Details[i] = RequestDetail{
			Timestamp: now.Add(-time.Duration(daysOld) * 24 * time.Hour),
			Tokens:    TokenStats{TotalTokens: 100},
		}
	}
	stats.mu.Unlock()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// This simulates what happens in StartAutoSave
		stats.cleanupAndSave(statsPath, 30)
	}
}

// BenchmarkMemoryFootprint measures memory allocation
func BenchmarkMemoryFootprint(b *testing.B) {
	now := time.Now()

	b.Run("Before_Cleanup", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			stats := NewRequestStatistics()
			stats.mu.Lock()
			stats.apis["test-api"] = &apiStats{
				Models: map[string]*modelStats{
					"test-model": {
						Details: make([]RequestDetail, 100000),
					},
				},
			}
			for j := 0; j < 100000; j++ {
				stats.apis["test-api"].Models["test-model"].Details[j] = RequestDetail{
					Timestamp: now.Add(-time.Duration(j%90) * 24 * time.Hour),
					Tokens:    TokenStats{TotalTokens: 100},
				}
			}
			stats.mu.Unlock()
			_ = stats.Snapshot()
		}
	})

	b.Run("After_Cleanup", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			stats := NewRequestStatistics()
			stats.mu.Lock()
			stats.apis["test-api"] = &apiStats{
				Models: map[string]*modelStats{
					"test-model": {
						Details: make([]RequestDetail, 100000),
					},
				},
			}
			for j := 0; j < 100000; j++ {
				stats.apis["test-api"].Models["test-model"].Details[j] = RequestDetail{
					Timestamp: now.Add(-time.Duration(j%90) * 24 * time.Hour),
					Tokens:    TokenStats{TotalTokens: 100},
				}
			}
			stats.mu.Unlock()
			stats.CleanupOldDetails(30)
			_ = stats.Snapshot()
		}
	})
}
