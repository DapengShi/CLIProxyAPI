package amp

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// AmpRouteType represents the type of routing decision made for an Amp request
type AmpRouteType string

const (
	// RouteTypeLocalProvider indicates the request is handled by a local OAuth provider (free)
	RouteTypeLocalProvider AmpRouteType = "LOCAL_PROVIDER"
	// RouteTypeModelMapping indicates the request was remapped to another available model (free)
	RouteTypeModelMapping AmpRouteType = "MODEL_MAPPING"
	// RouteTypeAmpCredits indicates the request is forwarded to ampcode.com (uses Amp credits)
	RouteTypeAmpCredits AmpRouteType = "AMP_CREDITS"
	// RouteTypeNoProvider indicates no provider or fallback available
	RouteTypeNoProvider AmpRouteType = "NO_PROVIDER"
)

// MappedModelContextKey is the Gin context key for passing mapped model names.
const MappedModelContextKey = "mapped_model"

// modelCandidate represents a candidate model for chain fallback
type modelCandidate struct {
	Model      string   // Full model name (with thinking suffix if applicable)
	Providers  []string // Available providers for this model
	ViaMapping bool     // Whether this candidate came from a mapping
}

// fallbackResponseWriter wraps http.ResponseWriter to capture status and detect quota errors
type fallbackResponseWriter struct {
	gin.ResponseWriter
	statusCode    int
	bodyBuffer    bytes.Buffer
	headerWritten bool
	isQuotaError  bool
}

func newFallbackResponseWriter(w gin.ResponseWriter) *fallbackResponseWriter {
	return &fallbackResponseWriter{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
	}
}

func (w *fallbackResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.headerWritten = true
	// Don't write to underlying writer yet - we may need to retry
}

func (w *fallbackResponseWriter) Write(data []byte) (int, error) {
	// Buffer the data to check for quota errors
	w.bodyBuffer.Write(data)
	return len(data), nil
}

func (w *fallbackResponseWriter) Status() int {
	return w.statusCode
}

func (w *fallbackResponseWriter) Written() bool {
	return w.headerWritten
}

// isRetryableError checks if the response indicates a quota/rate limit error that should trigger fallback
func (w *fallbackResponseWriter) isRetryableError() bool {
	// Check HTTP status codes that indicate quota/rate limiting
	switch w.statusCode {
	case http.StatusTooManyRequests, // 429
		http.StatusForbidden,      // 403 (often used for quota exceeded)
		http.StatusServiceUnavailable, // 503
		http.StatusBadGateway:     // 502
		return true
	}

	// Check response body for quota-related error messages
	body := w.bodyBuffer.String()
	quotaPatterns := []string{
		"quota", "rate_limit", "rate limit", "too many requests",
		"insufficient_quota", "resource_exhausted", "capacity",
		"RESOURCE_EXHAUSTED", "RATE_LIMIT_EXCEEDED",
	}
	lowerBody := strings.ToLower(body)
	for _, pattern := range quotaPatterns {
		if strings.Contains(lowerBody, strings.ToLower(pattern)) {
			return true
		}
	}

	return false
}

// flush writes buffered response to the underlying writer
func (w *fallbackResponseWriter) flush() {
	if w.headerWritten {
		w.ResponseWriter.WriteHeader(w.statusCode)
	}
	if w.bodyBuffer.Len() > 0 {
		w.ResponseWriter.Write(w.bodyBuffer.Bytes())
	}
}

// reset clears the buffer for retry
func (w *fallbackResponseWriter) reset() {
	w.bodyBuffer.Reset()
	w.headerWritten = false
	w.statusCode = http.StatusOK
	w.isQuotaError = false
}

// logAmpRouting logs the routing decision for an Amp request with structured fields
func logAmpRouting(routeType AmpRouteType, requestedModel, resolvedModel, provider, path string) {
	fields := log.Fields{
		"component":       "amp-routing",
		"route_type":      string(routeType),
		"requested_model": requestedModel,
		"path":            path,
		"timestamp":       time.Now().Format(time.RFC3339),
	}

	if resolvedModel != "" && resolvedModel != requestedModel {
		fields["resolved_model"] = resolvedModel
	}
	if provider != "" {
		fields["provider"] = provider
	}

	switch routeType {
	case RouteTypeLocalProvider:
		fields["cost"] = "free"
		fields["source"] = "local_oauth"
		log.WithFields(fields).Debugf("amp using local provider for model: %s", requestedModel)

	case RouteTypeModelMapping:
		fields["cost"] = "free"
		fields["source"] = "local_oauth"
		fields["mapping"] = requestedModel + " -> " + resolvedModel
		// model mapping already logged in mapper; avoid duplicate here

	case RouteTypeAmpCredits:
		fields["cost"] = "amp_credits"
		fields["source"] = "ampcode.com"
		fields["model_id"] = requestedModel // Explicit model_id for easy config reference
		log.WithFields(fields).Warnf("forwarding to ampcode.com (uses amp credits) - model_id: %s | To use local provider, add to config: ampcode.model-mappings: [{from: \"%s\", to: \"<your-local-model>\"}]", requestedModel, requestedModel)

	case RouteTypeNoProvider:
		fields["cost"] = "none"
		fields["source"] = "error"
		fields["model_id"] = requestedModel // Explicit model_id for easy config reference
		log.WithFields(fields).Warnf("no provider available for model_id: %s", requestedModel)
	}
}

// FallbackHandler wraps a standard handler with fallback logic to ampcode.com
// when the model's provider is not available in CLIProxyAPI
type FallbackHandler struct {
	getProxy           func() *httputil.ReverseProxy
	modelMapper        ModelMapper
	forceModelMappings func() bool
}

// NewFallbackHandler creates a new fallback handler wrapper
// The getProxy function allows lazy evaluation of the proxy (useful when proxy is created after routes)
func NewFallbackHandler(getProxy func() *httputil.ReverseProxy) *FallbackHandler {
	return &FallbackHandler{
		getProxy:           getProxy,
		forceModelMappings: func() bool { return false },
	}
}

// NewFallbackHandlerWithMapper creates a new fallback handler with model mapping support
func NewFallbackHandlerWithMapper(getProxy func() *httputil.ReverseProxy, mapper ModelMapper, forceModelMappings func() bool) *FallbackHandler {
	if forceModelMappings == nil {
		forceModelMappings = func() bool { return false }
	}
	return &FallbackHandler{
		getProxy:           getProxy,
		modelMapper:        mapper,
		forceModelMappings: forceModelMappings,
	}
}

// SetModelMapper sets the model mapper for this handler (allows late binding)
func (fh *FallbackHandler) SetModelMapper(mapper ModelMapper) {
	fh.modelMapper = mapper
}

// WrapHandler wraps a gin.HandlerFunc with fallback logic
// If the model's provider is not configured in CLIProxyAPI, it forwards to ampcode.com
// Supports chain fallback: tries multiple candidate models in order until one succeeds
func (fh *FallbackHandler) WrapHandler(handler gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		requestPath := c.Request.URL.Path

		// Read the request body to extract the model name
		bodyBytes, err := io.ReadAll(c.Request.Body)
		if err != nil {
			log.Errorf("amp fallback: failed to read request body: %v", err)
			handler(c)
			return
		}

		// Restore the body for the handler to read
		c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		// Try to extract model from request body or URL path (for Gemini)
		modelName := extractModelFromRequest(bodyBytes, c)
		if modelName == "" {
			// Can't determine model, proceed with normal handler
			handler(c)
			return
		}

		// Normalize model (handles dynamic thinking suffixes)
		normalizedModel, thinkingMetadata := util.NormalizeThinkingModel(modelName)
		thinkingSuffix := ""
		if thinkingMetadata != nil && strings.HasPrefix(modelName, normalizedModel) {
			thinkingSuffix = modelName[len(normalizedModel):]
		}

		// Build candidate list based on force mode
		forceMappings := fh.forceModelMappings != nil && fh.forceModelMappings()
		candidates := fh.buildCandidateList(modelName, normalizedModel, thinkingSuffix, forceMappings)

		// If no candidates available, fallback to ampcode.com
		if len(candidates) == 0 {
			proxy := fh.getProxy()
			if proxy != nil {
				logAmpRouting(RouteTypeAmpCredits, modelName, "", "", requestPath)
				c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				proxy.ServeHTTP(c.Writer, c.Request)
				return
			}
			logAmpRouting(RouteTypeNoProvider, modelName, "", "", requestPath)
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			handler(c)
			return
		}

		// Try each candidate in order (chain fallback)
		originalWriter := c.Writer
		var lastError *fallbackResponseWriter

		for i, cand := range candidates {
			// Prepare request body with candidate model
			candidateBody := bodyBytes
			if cand.ViaMapping {
				candidateBody = rewriteModelInRequest(bodyBytes, cand.Model)
				c.Set(MappedModelContextKey, cand.Model)
			}

			// Create fallback-aware response writer to capture response
			fw := newFallbackResponseWriter(originalWriter)
			c.Writer = fw

			// Filter Anthropic-Beta header for local handling
			filterAntropicBetaHeader(c)
			c.Request.Body = io.NopCloser(bytes.NewReader(candidateBody))

			// Wrap with response rewriter if using mapping
			if cand.ViaMapping {
				rewriter := NewResponseRewriter(fw, modelName)
				c.Writer = rewriter

				// Log routing decision
				providerName := ""
				if len(cand.Providers) > 0 {
					providerName = cand.Providers[0]
				}
				log.Debugf("amp chain fallback: trying candidate %d/%d: %s -> %s", i+1, len(candidates), modelName, cand.Model)
				logAmpRouting(RouteTypeModelMapping, modelName, cand.Model, providerName, requestPath)

				handler(c)
				rewriter.Flush()

				// Check if response indicates quota/rate limit error
				if fw.isRetryableError() && i < len(candidates)-1 {
					log.Warnf("amp chain fallback: candidate %s failed (quota/rate limit), trying next candidate", cand.Model)
					fw.reset()
					continue
				}

				// Success or last candidate - flush response
				fw.flush()
				return
			} else {
				// Direct local provider (no mapping)
				providerName := ""
				if len(cand.Providers) > 0 {
					providerName = cand.Providers[0]
				}
				log.Debugf("amp chain fallback: trying candidate %d/%d: %s (local)", i+1, len(candidates), cand.Model)
				logAmpRouting(RouteTypeLocalProvider, modelName, cand.Model, providerName, requestPath)

				handler(c)

				// Check if response indicates quota/rate limit error
				if fw.isRetryableError() && i < len(candidates)-1 {
					log.Warnf("amp chain fallback: candidate %s failed (quota/rate limit), trying next candidate", cand.Model)
					fw.reset()
					continue
				}

				// Success or last candidate - flush response
				fw.flush()
				return
			}
		}

		// All candidates failed, try ampcode.com as last resort
		if lastError != nil && lastError.isRetryableError() {
			proxy := fh.getProxy()
			if proxy != nil {
				log.Warnf("amp chain fallback: all local candidates exhausted, forwarding to ampcode.com")
				logAmpRouting(RouteTypeAmpCredits, modelName, "", "", requestPath)
				c.Writer = originalWriter
				c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				proxy.ServeHTTP(c.Writer, c.Request)
				return
			}
		}
	}
}

// buildCandidateList builds an ordered list of candidate models to try
func (fh *FallbackHandler) buildCandidateList(modelName, normalizedModel, thinkingSuffix string, forceMappings bool) []modelCandidate {
	var candidates []modelCandidate

	// Helper to add mapped candidates from the mapper
	addMappedCandidates := func() {
		if fh.modelMapper == nil {
			return
		}

		// Try to get candidates for the full model name first, then normalized
		mappedModels := fh.modelMapper.MapModelCandidates(modelName)
		if len(mappedModels) == 0 {
			mappedModels = fh.modelMapper.MapModelCandidates(normalizedModel)
		}

		for _, mapped := range mappedModels {
			mapped = strings.TrimSpace(mapped)
			if mapped == "" {
				continue
			}

			// Preserve dynamic thinking suffix if target doesn't have one
			if thinkingSuffix != "" {
				_, mappedThinkingMetadata := util.NormalizeThinkingModel(mapped)
				if mappedThinkingMetadata == nil {
					mapped += thinkingSuffix
				}
			}

			// Check if this candidate has available providers
			mappedBase, _ := util.NormalizeThinkingModel(mapped)
			providers := util.GetProviderName(mappedBase)
			if len(providers) > 0 {
				candidates = append(candidates, modelCandidate{
					Model:      mapped,
					Providers:  providers,
					ViaMapping: true,
				})
			}
		}
	}

	// Helper to add direct local provider if available
	addLocalCandidate := func() {
		providers := util.GetProviderName(normalizedModel)
		if len(providers) > 0 {
			candidates = append(candidates, modelCandidate{
				Model:      modelName,
				Providers:  providers,
				ViaMapping: false,
			})
		}
	}

	if forceMappings {
		// FORCE MODE: mappings first, then local provider
		addMappedCandidates()
		addLocalCandidate()
	} else {
		// DEFAULT MODE: local provider first, then mappings
		addLocalCandidate()
		addMappedCandidates()
	}

	return candidates
}

// filterAntropicBetaHeader filters Anthropic-Beta header to remove features requiring special subscription
// This is needed when using local providers (bypassing the Amp proxy)
func filterAntropicBetaHeader(c *gin.Context) {
	if betaHeader := c.Request.Header.Get("Anthropic-Beta"); betaHeader != "" {
		if filtered := filterBetaFeatures(betaHeader, "context-1m-2025-08-07"); filtered != "" {
			c.Request.Header.Set("Anthropic-Beta", filtered)
		} else {
			c.Request.Header.Del("Anthropic-Beta")
		}
	}
}

// rewriteModelInRequest replaces the model name in a JSON request body
func rewriteModelInRequest(body []byte, newModel string) []byte {
	if !gjson.GetBytes(body, "model").Exists() {
		return body
	}
	result, err := sjson.SetBytes(body, "model", newModel)
	if err != nil {
		log.Warnf("amp model mapping: failed to rewrite model in request body: %v", err)
		return body
	}
	return result
}

// extractModelFromRequest attempts to extract the model name from various request formats
func extractModelFromRequest(body []byte, c *gin.Context) string {
	// First try to parse from JSON body (OpenAI, Claude, etc.)
	// Check common model field names
	if result := gjson.GetBytes(body, "model"); result.Exists() && result.Type == gjson.String {
		return result.String()
	}

	// For Gemini requests, model is in the URL path
	// Standard format: /models/{model}:generateContent -> :action parameter
	if action := c.Param("action"); action != "" {
		// Split by colon to get model name (e.g., "gemini-pro:generateContent" -> "gemini-pro")
		parts := strings.Split(action, ":")
		if len(parts) > 0 && parts[0] != "" {
			return parts[0]
		}
	}

	// AMP CLI format: /publishers/google/models/{model}:method -> *path parameter
	// Example: /publishers/google/models/gemini-3-pro-preview:streamGenerateContent
	if path := c.Param("path"); path != "" {
		// Look for /models/{model}:method pattern
		if idx := strings.Index(path, "/models/"); idx >= 0 {
			modelPart := path[idx+8:] // Skip "/models/"
			// Split by colon to get model name
			if colonIdx := strings.Index(modelPart, ":"); colonIdx > 0 {
				return modelPart[:colonIdx]
			}
		}
	}

	return ""
}
