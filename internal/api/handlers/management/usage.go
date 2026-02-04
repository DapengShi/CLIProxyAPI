package management

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// GetUsageStatistics returns the in-memory request statistics snapshot.
func (h *Handler) GetUsageStatistics(c *gin.Context) {
	var snapshot usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}

	// Transform the internal snapshot to the required external response format
	response := gin.H{
		"total_requests": snapshot.TotalRequests,
		"total_tokens":   snapshot.TotalTokens,
		"success_count":  snapshot.SuccessCount,
		"failure_count":  snapshot.FailureCount,
	}

	apis := make(map[string]interface{})
	for apiName, apiSnap := range snapshot.APIs {
		models := make(map[string]interface{})
		for modelName, modelSnap := range apiSnap.Models {
			details := make([]gin.H, 0, len(modelSnap.Details))
			for _, detail := range modelSnap.Details {
				details = append(details, gin.H{
					"timestamp":  detail.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z"),
					"source":     detail.Source,
					"auth_index": detail.AuthIndex,
					"tokens":     detail.Tokens,
					"failed":     detail.Failed,
				})
			}
			models[modelName] = gin.H{
				"total_requests": modelSnap.TotalRequests,
				"total_tokens":   modelSnap.TotalTokens,
				"details":        details,
			}
		}
		apis[apiName] = gin.H{
			"total_requests": apiSnap.TotalRequests,
			"total_tokens":   apiSnap.TotalTokens,
			"models":         models,
		}
	}
	response["apis"] = apis

	c.JSON(http.StatusOK, response)
}

// ExportUsageStatistics returns a complete usage snapshot for backup/migration.
func (h *Handler) ExportUsageStatistics(c *gin.Context) {
	var snapshot usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}
	c.JSON(http.StatusOK, usage.ExportPayload{
		Version:    1,
		ExportedAt: time.Now().UTC(),
		Usage:      snapshot,
	})
}

// ImportUsageStatistics merges a previously exported usage snapshot into memory.
func (h *Handler) ImportUsageStatistics(c *gin.Context) {
	if h == nil || h.usageStats == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage statistics unavailable"})
		return
	}

	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	var payload usage.ImportPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	if payload.Version != 0 && payload.Version != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported version"})
		return
	}

	result := h.usageStats.MergeSnapshot(payload.Usage)
	snapshot := h.usageStats.Snapshot()
	c.JSON(http.StatusOK, gin.H{
		"added":           result.Added,
		"skipped":         result.Skipped,
		"total_requests":  snapshot.TotalRequests,
		"failed_requests": snapshot.FailureCount,
	})
}
