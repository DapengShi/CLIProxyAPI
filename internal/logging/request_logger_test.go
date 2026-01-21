package logging

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCleanupRequestLogs_TimeBasedCleanup tests time-based cleanup functionality
func TestCleanupRequestLogs_TimeBasedCleanup(t *testing.T) {
	// Create temporary directory for test logs
	tmpDir := t.TempDir()

	logger := &FileRequestLogger{
		enabled:        true,
		logsDir:        tmpDir,
		retentionDays:  7,
		maxTotalSizeMB: 0, // Disable size-based cleanup
	}

	now := time.Now()

	// Create test log files with different ages
	testFiles := []struct {
		name      string
		age       time.Duration
		shouldDel bool
	}{
		{"v1-request-2days-old.log", 2 * 24 * time.Hour, false},
		{"v1-request-5days-old.log", 5 * 24 * time.Hour, false},
		{"v1-request-8days-old.log", 8 * 24 * time.Hour, true},
		{"v1-request-10days-old.log", 10 * 24 * time.Hour, true},
		{"v1-request-15days-old.log", 15 * 24 * time.Hour, true},
	}

	for _, tf := range testFiles {
		path := filepath.Join(tmpDir, tf.name)
		if err := os.WriteFile(path, []byte("test log content"), 0644); err != nil {
			t.Fatalf("failed to create test file %s: %v", tf.name, err)
		}

		// Set modification time to simulate age
		modTime := now.Add(-tf.age)
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("failed to set time for %s: %v", tf.name, err)
		}
	}

	// Also create files that should NOT be deleted (different types)
	protectedFiles := []string{
		"error-test.log",
		"main.log",
		"main-2024-01-01.log",
		"request-body-123.tmp",
	}

	for _, name := range protectedFiles {
		path := filepath.Join(tmpDir, name)
		if err := os.WriteFile(path, []byte("protected"), 0644); err != nil {
			t.Fatalf("failed to create protected file %s: %v", name, err)
		}
		// Make them very old
		oldTime := now.Add(-30 * 24 * time.Hour)
		if err := os.Chtimes(path, oldTime, oldTime); err != nil {
			t.Fatalf("failed to set time for %s: %v", name, err)
		}
	}

	// Execute cleanup
	deleted, err := logger.CleanupRequestLogs(logger.retentionDays, logger.maxTotalSizeMB)
	if err != nil {
		t.Fatalf("CleanupRequestLogs failed: %v", err)
	}

	// Verify correct number of files deleted
	expectedDeleted := 0
	for _, tf := range testFiles {
		if tf.shouldDel {
			expectedDeleted++
		}
	}

	if deleted != expectedDeleted {
		t.Errorf("expected %d files deleted, got %d", expectedDeleted, deleted)
	}

	// Verify files exist/don't exist as expected
	for _, tf := range testFiles {
		path := filepath.Join(tmpDir, tf.name)
		_, err := os.Stat(path)
		exists := err == nil

		if tf.shouldDel && exists {
			t.Errorf("file %s should have been deleted but still exists", tf.name)
		}
		if !tf.shouldDel && !exists {
			t.Errorf("file %s should still exist but was deleted", tf.name)
		}
	}

	// Verify protected files still exist
	for _, name := range protectedFiles {
		path := filepath.Join(tmpDir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("protected file %s was deleted", name)
		}
	}
}

// TestCleanupRequestLogs_SizeBasedCleanup tests size-based cleanup functionality
func TestCleanupRequestLogs_SizeBasedCleanup(t *testing.T) {
	tmpDir := t.TempDir()

	logger := &FileRequestLogger{
		enabled:        true,
		logsDir:        tmpDir,
		retentionDays:  0,  // Disable time-based cleanup
		maxTotalSizeMB: 1,  // 1 MB limit
	}

	now := time.Now()

	// Create test log files with different sizes
	// Each file is ~0.4MB, so total will be ~1.6MB (exceeding 1MB limit)
	// After deleting oldest, we have ~1.2MB (still over), so need to delete 2nd oldest too
	testFiles := []struct {
		name      string
		size      int
		age       time.Duration
		shouldDel bool // Oldest files should be deleted
	}{
		{"v1-request-oldest.log", 400 * 1024, 5 * time.Hour, true},
		{"v1-request-old.log", 400 * 1024, 4 * time.Hour, true},
		{"v1-request-newer.log", 400 * 1024, 3 * time.Hour, false},
		{"v1-request-newest.log", 400 * 1024, 2 * time.Hour, false},
	}

	for _, tf := range testFiles {
		path := filepath.Join(tmpDir, tf.name)
		// Create file with specified size
		data := make([]byte, tf.size)
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatalf("failed to create test file %s: %v", tf.name, err)
		}

		// Set modification time to establish order
		modTime := now.Add(-tf.age)
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("failed to set time for %s: %v", tf.name, err)
		}
	}

	// Execute cleanup
	deleted, err := logger.CleanupRequestLogs(logger.retentionDays, logger.maxTotalSizeMB)
	if err != nil {
		t.Fatalf("CleanupRequestLogs failed: %v", err)
	}

	// Should have deleted at least the oldest files to get under limit
	if deleted < 2 {
		t.Errorf("expected at least 2 files deleted, got %d", deleted)
	}

	// Verify newest files still exist
	for _, tf := range testFiles {
		path := filepath.Join(tmpDir, tf.name)
		_, err := os.Stat(path)
		exists := err == nil

		if tf.shouldDel && exists {
			t.Errorf("file %s should have been deleted but still exists", tf.name)
		}
		if !tf.shouldDel && !exists {
			t.Errorf("file %s should still exist but was deleted", tf.name)
		}
	}

	// Verify total size is now under limit
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("failed to read directory: %v", err)
	}

	var totalSize int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		totalSize += info.Size()
	}

	maxBytes := int64(logger.maxTotalSizeMB) * 1024 * 1024
	if totalSize > maxBytes {
		t.Errorf("total size %d bytes still exceeds limit %d bytes after cleanup", totalSize, maxBytes)
	}
}

// TestCleanupRequestLogs_CombinedCleanup tests both time and size-based cleanup
func TestCleanupRequestLogs_CombinedCleanup(t *testing.T) {
	tmpDir := t.TempDir()

	logger := &FileRequestLogger{
		enabled:        true,
		logsDir:        tmpDir,
		retentionDays:  7,
		maxTotalSizeMB: 1,
	}

	now := time.Now()

	// Create files that trigger both conditions
	testFiles := []struct {
		name         string
		size         int
		age          time.Duration
		shouldDelAge bool
		shouldDelSize bool
	}{
		{"v1-request-old-large.log", 300 * 1024, 10 * 24 * time.Hour, true, true},
		{"v1-request-old-small.log", 100 * 1024, 8 * 24 * time.Hour, true, false},
		{"v1-request-new-large.log", 300 * 1024, 2 * 24 * time.Hour, false, false},
		{"v1-request-new-small.log", 100 * 1024, 1 * 24 * time.Hour, false, false},
	}

	for _, tf := range testFiles {
		path := filepath.Join(tmpDir, tf.name)
		data := make([]byte, tf.size)
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatalf("failed to create test file %s: %v", tf.name, err)
		}

		modTime := now.Add(-tf.age)
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("failed to set time for %s: %v", tf.name, err)
		}
	}

	// Execute cleanup
	deleted, err := logger.CleanupRequestLogs(logger.retentionDays, logger.maxTotalSizeMB)
	if err != nil {
		t.Fatalf("CleanupRequestLogs failed: %v", err)
	}

	// Should delete at least the old files
	if deleted < 2 {
		t.Errorf("expected at least 2 files deleted, got %d", deleted)
	}

	// Old files should be deleted
	oldFiles := []string{"v1-request-old-large.log", "v1-request-old-small.log"}
	for _, name := range oldFiles {
		path := filepath.Join(tmpDir, name)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("old file %s should have been deleted", name)
		}
	}
}

// TestCleanupRequestLogs_EmptyDirectory tests cleanup on empty directory
func TestCleanupRequestLogs_EmptyDirectory(t *testing.T) {
	tmpDir := t.TempDir()

	logger := &FileRequestLogger{
		enabled:        true,
		logsDir:        tmpDir,
		retentionDays:  7,
		maxTotalSizeMB: 100,
	}

	deleted, err := logger.CleanupRequestLogs(logger.retentionDays, logger.maxTotalSizeMB)
	if err != nil {
		t.Fatalf("CleanupRequestLogs failed: %v", err)
	}

	if deleted != 0 {
		t.Errorf("expected 0 files deleted, got %d", deleted)
	}
}

// TestCleanupRequestLogs_NonexistentDirectory tests cleanup on non-existent directory
func TestCleanupRequestLogs_NonexistentDirectory(t *testing.T) {
	tmpDir := filepath.Join(t.TempDir(), "nonexistent")

	logger := &FileRequestLogger{
		enabled:        true,
		logsDir:        tmpDir,
		retentionDays:  7,
		maxTotalSizeMB: 100,
	}

	deleted, err := logger.CleanupRequestLogs(logger.retentionDays, logger.maxTotalSizeMB)
	if err != nil {
		t.Fatalf("CleanupRequestLogs should not error on non-existent dir: %v", err)
	}

	if deleted != 0 {
		t.Errorf("expected 0 files deleted, got %d", deleted)
	}
}

// TestCleanupRequestLogs_DisabledCleanup tests when both cleanup options are disabled
func TestCleanupRequestLogs_DisabledCleanup(t *testing.T) {
	tmpDir := t.TempDir()

	logger := &FileRequestLogger{
		enabled:        true,
		logsDir:        tmpDir,
		retentionDays:  0, // Disabled
		maxTotalSizeMB: 0, // Disabled
	}

	now := time.Now()

	// Create old and large files
	testFiles := []string{
		"v1-request-old.log",
		"v1-request-large.log",
	}

	for _, name := range testFiles {
		path := filepath.Join(tmpDir, name)
		data := make([]byte, 1024*1024) // 1MB
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatalf("failed to create test file %s: %v", name, err)
		}

		// Make them very old
		oldTime := now.Add(-30 * 24 * time.Hour)
		if err := os.Chtimes(path, oldTime, oldTime); err != nil {
			t.Fatalf("failed to set time for %s: %v", name, err)
		}
	}

	deleted, err := logger.CleanupRequestLogs(logger.retentionDays, logger.maxTotalSizeMB)
	if err != nil {
		t.Fatalf("CleanupRequestLogs failed: %v", err)
	}

	// Should delete nothing when both are disabled
	if deleted != 0 {
		t.Errorf("expected 0 files deleted when cleanup is disabled, got %d", deleted)
	}

	// Verify all files still exist
	for _, name := range testFiles {
		path := filepath.Join(tmpDir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("file %s should not have been deleted", name)
		}
	}
}

// TestNewFileRequestLogger_DefaultValues tests default values are set correctly
func TestNewFileRequestLogger_DefaultValues(t *testing.T) {
	// Test with zero values (should use defaults)
	logger := NewFileRequestLogger(true, "logs", "", 0, 0)

	if logger.retentionDays != 7 {
		t.Errorf("expected default retentionDays=7, got %d", logger.retentionDays)
	}

	if logger.maxTotalSizeMB != 100 {
		t.Errorf("expected default maxTotalSizeMB=100, got %d", logger.maxTotalSizeMB)
	}

	// Test with custom values
	logger2 := NewFileRequestLogger(true, "logs", "", 14, 200)

	if logger2.retentionDays != 14 {
		t.Errorf("expected retentionDays=14, got %d", logger2.retentionDays)
	}

	if logger2.maxTotalSizeMB != 200 {
		t.Errorf("expected maxTotalSizeMB=200, got %d", logger2.maxTotalSizeMB)
	}
}
