// Package modelmapping provides model mapping functionality for routing requests
// to alternative models when the requested model is unavailable or exhausted.
package modelmapping

import (
	"regexp"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

// ModelMapper provides model name mapping/aliasing for model fallback.
// When a request comes in for a model that isn't available locally,
// this mapper can redirect it to an alternative model that IS available.
type ModelMapper interface {
	// MapModel returns the first target model name if a mapping exists.
	// Returns empty string if no mapping applies.
	// Deprecated: Use MapModelCandidates for chain fallback support.
	MapModel(requestedModel string) string

	// MapModelCandidates returns an ordered list of candidate models for the requested model.
	// The list is ordered by priority (first = highest priority).
	// Returns nil if no mapping applies.
	MapModelCandidates(requestedModel string) []string

	// UpdateMappings refreshes the mapping configuration (for hot-reload).
	UpdateMappings(mappings []config.AmpModelMapping)
}

// DefaultModelMapper implements ModelMapper with thread-safe mapping storage.
type DefaultModelMapper struct {
	mu       sync.RWMutex
	mappings map[string][]string // exact: from -> ordered targets
	regexps  []regexMapping      // regex rules evaluated in order
}

// NewModelMapper creates a new model mapper with the given initial mappings.
func NewModelMapper(mappings []config.AmpModelMapping) *DefaultModelMapper {
	m := &DefaultModelMapper{
		mappings: make(map[string][]string),
		regexps:  nil,
	}
	m.UpdateMappings(mappings)
	return m
}

// MapModel returns the first target model name if a mapping exists.
// This is a convenience method for backwards compatibility.
func (m *DefaultModelMapper) MapModel(requestedModel string) string {
	candidates := m.MapModelCandidates(requestedModel)
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0]
}

// MapModelCandidates returns an ordered list of candidate models for the requested model.
// The list is ordered by priority (first = highest priority).
// Provider availability is NOT checked here - that's done by the caller.
func (m *DefaultModelMapper) MapModelCandidates(requestedModel string) []string {
	if requestedModel == "" {
		return nil
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	// Normalize the requested model for lookup
	normalizedRequest := strings.ToLower(strings.TrimSpace(requestedModel))

	// Check for direct mapping
	targets, exists := m.mappings[normalizedRequest]
	if !exists {
		// Try regex mappings in order
		for _, rm := range m.regexps {
			if rm.re.MatchString(requestedModel) || rm.re.MatchString(normalizedRequest) {
				targets = rm.targets
				exists = true
				break
			}
		}
		if !exists {
			return nil
		}
	}

	// Return a copy to prevent external modification
	result := make([]string, len(targets))
	copy(result, targets)
	return result
}

// UpdateMappings refreshes the mapping configuration from config.
// This is called during initialization and on config hot-reload.
func (m *DefaultModelMapper) UpdateMappings(mappings []config.AmpModelMapping) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Clear and rebuild mappings
	m.mappings = make(map[string][]string, len(mappings))
	m.regexps = make([]regexMapping, 0, len(mappings))

	for _, mapping := range mappings {
		from := strings.TrimSpace(mapping.From)
		to := strings.TrimSpace(mapping.To)

		if from == "" || to == "" {
			log.Warnf("model mapping: skipping invalid mapping (from=%q, to=%q)", from, to)
			continue
		}

		// Build ordered targets list: To + Fallbacks
		targets := []string{to}
		for _, fb := range mapping.Fallbacks {
			fb = strings.TrimSpace(fb)
			if fb != "" {
				targets = append(targets, fb)
			}
		}

		if mapping.Regex {
			// Compile case-insensitive regex; wrap with (?i) to match behavior of exact lookups
			pattern := "(?i)" + from
			re, err := regexp.Compile(pattern)
			if err != nil {
				log.Warnf("model mapping: invalid regex %q: %v", from, err)
				continue
			}
			m.regexps = append(m.regexps, regexMapping{re: re, targets: targets})
			if len(targets) > 1 {
				log.Debugf("model regex mapping registered: /%s/ -> %v (chain)", from, targets)
			} else {
				log.Debugf("model regex mapping registered: /%s/ -> %s", from, to)
			}
		} else {
			// Store with normalized lowercase key for case-insensitive lookup
			normalizedFrom := strings.ToLower(from)
			m.mappings[normalizedFrom] = targets
			if len(targets) > 1 {
				log.Debugf("model mapping registered: %s -> %v (chain)", from, targets)
			} else {
				log.Debugf("model mapping registered: %s -> %s", from, to)
			}
		}
	}

	exactCount := len(m.mappings)
	regexCount := len(m.regexps)
	if exactCount > 0 {
		log.Infof("model mapping: loaded %d exact mapping(s)", exactCount)
	}
	if regexCount > 0 {
		log.Infof("model mapping: loaded %d regex mapping(s)", regexCount)
	}
}

// GetMappings returns a copy of current mappings (for debugging/status).
// Returns only the first target for each mapping for backwards compatibility.
func (m *DefaultModelMapper) GetMappings() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]string, len(m.mappings))
	for k, v := range m.mappings {
		if len(v) > 0 {
			result[k] = v[0]
		}
	}
	return result
}

// GetMappingsWithFallbacks returns a copy of current mappings with full fallback chains.
func (m *DefaultModelMapper) GetMappingsWithFallbacks() map[string][]string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string][]string, len(m.mappings))
	for k, v := range m.mappings {
		targets := make([]string, len(v))
		copy(targets, v)
		result[k] = targets
	}
	return result
}

type regexMapping struct {
	re      *regexp.Regexp
	targets []string
}
