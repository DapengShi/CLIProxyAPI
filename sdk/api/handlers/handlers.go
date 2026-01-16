// Package handlers provides core API handler functionality for the CLI Proxy API server.
// It includes common types, client management, load balancing, and error handling
// shared across all API endpoint handlers (OpenAI, Claude, Gemini).
package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/modelmapping"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/net/context"
)

// ErrorResponse represents a standard error response format for the API.
// It contains a single ErrorDetail field.
type ErrorResponse struct {
	// Error contains detailed information about the error that occurred.
	Error ErrorDetail `json:"error"`
}

// ErrorDetail provides specific information about an error that occurred.
// It includes a human-readable message, an error type, and an optional error code.
type ErrorDetail struct {
	// Message is a human-readable message providing more details about the error.
	Message string `json:"message"`

	// Type is the category of error that occurred (e.g., "invalid_request_error").
	Type string `json:"type"`

	// Code is a short code identifying the error, if applicable.
	Code string `json:"code,omitempty"`
}

const idempotencyKeyMetadataKey = "idempotency_key"

const (
	defaultStreamingKeepAliveSeconds = 0
	defaultStreamingBootstrapRetries = 0
)

// BuildErrorResponseBody builds an OpenAI-compatible JSON error response body.
// If errText is already valid JSON, it is returned as-is to preserve upstream error payloads.
func BuildErrorResponseBody(status int, errText string) []byte {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if strings.TrimSpace(errText) == "" {
		errText = http.StatusText(status)
	}

	trimmed := strings.TrimSpace(errText)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		return []byte(trimmed)
	}

	errType := "invalid_request_error"
	var code string
	switch status {
	case http.StatusUnauthorized:
		errType = "authentication_error"
		code = "invalid_api_key"
	case http.StatusForbidden:
		errType = "permission_error"
		code = "insufficient_quota"
	case http.StatusTooManyRequests:
		errType = "rate_limit_error"
		code = "rate_limit_exceeded"
	case http.StatusNotFound:
		errType = "invalid_request_error"
		code = "model_not_found"
	default:
		if status >= http.StatusInternalServerError {
			errType = "server_error"
			code = "internal_server_error"
		}
	}

	payload, err := json.Marshal(ErrorResponse{
		Error: ErrorDetail{
			Message: errText,
			Type:    errType,
			Code:    code,
		},
	})
	if err != nil {
		return []byte(fmt.Sprintf(`{"error":{"message":%q,"type":"server_error","code":"internal_server_error"}}`, errText))
	}
	return payload
}

// StreamingKeepAliveInterval returns the SSE keep-alive interval for this server.
// Returning 0 disables keep-alives (default when unset).
func StreamingKeepAliveInterval(cfg *config.SDKConfig) time.Duration {
	seconds := defaultStreamingKeepAliveSeconds
	if cfg != nil {
		seconds = cfg.Streaming.KeepAliveSeconds
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// NonStreamingKeepAliveInterval returns the keep-alive interval for non-streaming responses.
// Returning 0 disables keep-alives (default when unset).
func NonStreamingKeepAliveInterval(cfg *config.SDKConfig) time.Duration {
	seconds := 0
	if cfg != nil {
		seconds = cfg.NonStreamKeepAliveInterval
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// StreamingBootstrapRetries returns how many times a streaming request may be retried before any bytes are sent.
func StreamingBootstrapRetries(cfg *config.SDKConfig) int {
	retries := defaultStreamingBootstrapRetries
	if cfg != nil {
		retries = cfg.Streaming.BootstrapRetries
	}
	if retries < 0 {
		retries = 0
	}
	return retries
}

func requestExecutionMetadata(ctx context.Context) map[string]any {
	// Idempotency-Key is an optional client-supplied header used to correlate retries.
	// It is forwarded as execution metadata; when absent we generate a UUID.
	key := ""
	if ctx != nil {
		if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			key = strings.TrimSpace(ginCtx.GetHeader("Idempotency-Key"))
		}
	}
	if key == "" {
		key = uuid.NewString()
	}
	return map[string]any{idempotencyKeyMetadataKey: key}
}

func mergeMetadata(base, overlay map[string]any) map[string]any {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}
	out := make(map[string]any, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}

// BaseAPIHandler contains the handlers for API endpoints.
// It holds a pool of clients to interact with the backend service and manages
// load balancing, client selection, and configuration.
type BaseAPIHandler struct {
	// AuthManager manages auth lifecycle and execution in the new architecture.
	AuthManager *coreauth.Manager

	// Cfg holds the current application configuration.
	Cfg *config.SDKConfig

	modelMapper *modelmapping.DefaultModelMapper
}

// NewBaseAPIHandlers creates a new API handlers instance.
// It takes a slice of clients and configuration as input.
//
// Parameters:
//   - cliClients: A slice of AI service clients
//   - cfg: The application configuration
//
// Returns:
//   - *BaseAPIHandler: A new API handlers instance
func NewBaseAPIHandlers(cfg *config.SDKConfig, authManager *coreauth.Manager) *BaseAPIHandler {
	h := &BaseAPIHandler{
		Cfg:         cfg,
		AuthManager: authManager,
		modelMapper: modelmapping.NewModelMapper(nil),
	}
	return h
}

// UpdateClients updates the handlers' client list and configuration.
// This method is called when the configuration or authentication tokens change.
//
// Parameters:
//   - clients: The new slice of AI service clients
//   - cfg: The new application configuration
func (h *BaseAPIHandler) UpdateClients(cfg *config.SDKConfig) {
	h.Cfg = cfg
}

// UpdateModelMappings refreshes model mapping configuration for global fallback.
func (h *BaseAPIHandler) UpdateModelMappings(mappings []config.AmpModelMapping) {
	if h == nil {
		return
	}
	if h.modelMapper == nil {
		h.modelMapper = modelmapping.NewModelMapper(nil)
	}
	h.modelMapper.UpdateMappings(mappings)
}

// GetAlt extracts the 'alt' parameter from the request query string.
// It checks both 'alt' and '$alt' parameters and returns the appropriate value.
//
// Parameters:
//   - c: The Gin context containing the HTTP request
//
// Returns:
//   - string: The alt parameter value, or empty string if it's "sse"
func (h *BaseAPIHandler) GetAlt(c *gin.Context) string {
	var alt string
	var hasAlt bool
	alt, hasAlt = c.GetQuery("alt")
	if !hasAlt {
		alt, _ = c.GetQuery("$alt")
	}
	if alt == "sse" {
		return ""
	}
	return alt
}

// GetContextWithCancel creates a new context with cancellation capabilities.
// It embeds the Gin context and the API handler into the new context for later use.
// The returned cancel function also handles logging the API response if request logging is enabled.
//
// Parameters:
//   - handler: The API handler associated with the request.
//   - c: The Gin context of the current request.
//   - ctx: The parent context (caller values/deadlines are preserved; request context adds cancellation and request ID).
//
// Returns:
//   - context.Context: The new context with cancellation and embedded values.
//   - APIHandlerCancelFunc: A function to cancel the context and log the response.
func (h *BaseAPIHandler) GetContextWithCancel(handler interfaces.APIHandler, c *gin.Context, ctx context.Context) (context.Context, APIHandlerCancelFunc) {
	parentCtx := ctx
	if parentCtx == nil {
		parentCtx = context.Background()
	}

	var requestCtx context.Context
	if c != nil && c.Request != nil {
		requestCtx = c.Request.Context()
	}

	if requestCtx != nil && logging.GetRequestID(parentCtx) == "" {
		if requestID := logging.GetRequestID(requestCtx); requestID != "" {
			parentCtx = logging.WithRequestID(parentCtx, requestID)
		} else if requestID := logging.GetGinRequestID(c); requestID != "" {
			parentCtx = logging.WithRequestID(parentCtx, requestID)
		}
	}
	newCtx, cancel := context.WithCancel(parentCtx)
	if requestCtx != nil && requestCtx != parentCtx {
		go func() {
			select {
			case <-requestCtx.Done():
				cancel()
			case <-newCtx.Done():
			}
		}()
	}
	newCtx = context.WithValue(newCtx, "gin", c)
	newCtx = context.WithValue(newCtx, "handler", handler)
	return newCtx, func(params ...interface{}) {
		if h.Cfg.RequestLog && len(params) == 1 {
			if existing, exists := c.Get("API_RESPONSE"); exists {
				if existingBytes, ok := existing.([]byte); ok && len(bytes.TrimSpace(existingBytes)) > 0 {
					switch params[0].(type) {
					case error, string:
						cancel()
						return
					}
				}
			}

			var payload []byte
			switch data := params[0].(type) {
			case []byte:
				payload = data
			case error:
				if data != nil {
					payload = []byte(data.Error())
				}
			case string:
				payload = []byte(data)
			}
			if len(payload) > 0 {
				if existing, exists := c.Get("API_RESPONSE"); exists {
					if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
						trimmedPayload := bytes.TrimSpace(payload)
						if len(trimmedPayload) > 0 && bytes.Contains(existingBytes, trimmedPayload) {
							cancel()
							return
						}
					}
				}
				appendAPIResponse(c, payload)
			}
		}

		cancel()
	}
}

// StartNonStreamingKeepAlive emits blank lines every 5 seconds while waiting for a non-streaming response.
// It returns a stop function that must be called before writing the final response.
func (h *BaseAPIHandler) StartNonStreamingKeepAlive(c *gin.Context, ctx context.Context) func() {
	if h == nil || c == nil {
		return func() {}
	}
	interval := NonStreamingKeepAliveInterval(h.Cfg)
	if interval <= 0 {
		return func() {}
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	stopChan := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopChan:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = c.Writer.Write([]byte("\n"))
				flusher.Flush()
			}
		}
	}()

	return func() {
		stopOnce.Do(func() {
			close(stopChan)
		})
		wg.Wait()
	}
}

// appendAPIResponse preserves any previously captured API response and appends new data.
func appendAPIResponse(c *gin.Context, data []byte) {
	if c == nil || len(data) == 0 {
		return
	}

	if existing, exists := c.Get("API_RESPONSE"); exists {
		if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
			combined := make([]byte, 0, len(existingBytes)+len(data)+1)
			combined = append(combined, existingBytes...)
			if existingBytes[len(existingBytes)-1] != '\n' {
				combined = append(combined, '\n')
			}
			combined = append(combined, data...)
			c.Set("API_RESPONSE", combined)
			return
		}
	}

	c.Set("API_RESPONSE", bytes.Clone(data))
}

// ExecuteWithAuthManager executes a non-streaming request via the core auth manager.
// This path is the only supported execution route.
func (h *BaseAPIHandler) ExecuteWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, *interfaces.ErrorMessage) {
	resp, errMsg := h.executeWithGlobalFallback(ctx, handlerType, modelName, rawJSON, alt, func(ctx context.Context, providers []string, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
		return h.AuthManager.Execute(ctx, providers, req, opts)
	})
	if errMsg != nil {
		return nil, errMsg
	}
	return cloneBytes(resp.Payload), nil
}

type fallbackCandidate struct {
	model     string
	baseModel string
	providers []string
	metadata  map[string]any
	payload   []byte
}

func (h *BaseAPIHandler) executeWithGlobalFallback(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string, execute func(ctx context.Context, providers []string, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error)) (coreexecutor.Response, *interfaces.ErrorMessage) {
	providers, normalizedModel, metadata, errMsg := h.getRequestDetails(modelName)
	if errMsg != nil {
		return coreexecutor.Response{}, errMsg
	}
	reqMeta := requestExecutionMetadata(ctx)
	req := coreexecutor.Request{
		Model:   normalizedModel,
		Payload: cloneBytes(rawJSON),
	}
	if cloned := cloneMetadata(metadata); cloned != nil {
		req.Metadata = cloned
	}
	opts := coreexecutor.Options{
		Stream:          false,
		Alt:             alt,
		OriginalRequest: cloneBytes(rawJSON),
		SourceFormat:    sdktranslator.FromString(handlerType),
	}
	opts.Metadata = mergeMetadata(cloneMetadata(metadata), reqMeta)

	resp, err := execute(ctx, providers, req, opts)
	if err == nil {
		return resp, nil
	}
	originalErr := errorMessageFromError(err)
	if originalErr == nil || originalErr.StatusCode != http.StatusTooManyRequests {
		return coreexecutor.Response{}, originalErr
	}

	candidates := h.buildFallbackCandidates(modelName, normalizedModel, rawJSON)
	if len(candidates) == 0 {
		return coreexecutor.Response{}, originalErr
	}

	for _, candidate := range candidates {
		req.Model = candidate.baseModel
		req.Payload = cloneBytes(candidate.payload)
		if cloned := cloneMetadata(candidate.metadata); cloned != nil {
			req.Metadata = cloned
		} else {
			req.Metadata = nil
		}
		opts.Metadata = mergeMetadata(cloneMetadata(candidate.metadata), reqMeta)
		resp, err = execute(ctx, candidate.providers, req, opts)
		if err == nil {
			return resp, nil
		}
		candidateErr := errorMessageFromError(err)
		if candidateErr == nil {
			candidateErr = &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: err}
		}
		if candidateErr.StatusCode != http.StatusTooManyRequests {
			return coreexecutor.Response{}, candidateErr
		}
	}
	return coreexecutor.Response{}, originalErr
}

// ExecuteCountWithAuthManager executes a non-streaming request via the core auth manager.
// This path is the only supported execution route.
func (h *BaseAPIHandler) ExecuteCountWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, *interfaces.ErrorMessage) {
	resp, errMsg := h.executeWithGlobalFallback(ctx, handlerType, modelName, rawJSON, alt, func(ctx context.Context, providers []string, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
		return h.AuthManager.ExecuteCount(ctx, providers, req, opts)
	})
	if errMsg != nil {
		return nil, errMsg
	}
	return cloneBytes(resp.Payload), nil
}

// ExecuteStreamWithAuthManager executes a streaming request via the core auth manager.
// This path is the only supported execution route.
func (h *BaseAPIHandler) ExecuteStreamWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) (<-chan []byte, <-chan *interfaces.ErrorMessage) {
	providers, normalizedModel, metadata, errMsg := h.getRequestDetails(modelName)
	if errMsg != nil {
		errChan := make(chan *interfaces.ErrorMessage, 1)
		errChan <- errMsg
		close(errChan)
		return nil, errChan
	}
	candidates := []fallbackCandidate{
		{
			model:     modelName,
			baseModel: normalizedModel,
			providers: providers,
			metadata:  cloneMetadata(metadata),
			payload:   cloneBytes(rawJSON),
		},
	}
	candidates = append(candidates, h.buildFallbackCandidates(modelName, normalizedModel, rawJSON)...)

	reqMeta := requestExecutionMetadata(ctx)
	baseOpts := coreexecutor.Options{
		Stream:          true,
		Alt:             alt,
		OriginalRequest: cloneBytes(rawJSON),
		SourceFormat:    sdktranslator.FromString(handlerType),
	}

	dataChan := make(chan []byte)
	errChan := make(chan *interfaces.ErrorMessage, 1)
	go func() {
		defer close(dataChan)
		defer close(errChan)
		maxBootstrapRetries := StreamingBootstrapRetries(h.Cfg)
		var originalErr *interfaces.ErrorMessage

		bootstrapEligible := func(err error) bool {
			status := statusFromError(err)
			if status == 0 {
				return true
			}
			switch status {
			case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired,
				http.StatusRequestTimeout, http.StatusTooManyRequests:
				return true
			default:
				return status >= http.StatusInternalServerError
			}
		}

	candidateLoop:
		for i, candidate := range candidates {
			sentPayload := false
			bootstrapRetries := 0
			req := coreexecutor.Request{
				Model:   candidate.baseModel,
				Payload: cloneBytes(candidate.payload),
			}
			if cloned := cloneMetadata(candidate.metadata); cloned != nil {
				req.Metadata = cloned
			}
			opts := baseOpts
			opts.Metadata = mergeMetadata(cloneMetadata(candidate.metadata), reqMeta)

			chunks, err := h.AuthManager.ExecuteStream(ctx, candidate.providers, req, opts)
			if err != nil {
				candidateErr := errorMessageFromError(err)
				if candidateErr != nil && candidateErr.StatusCode == http.StatusTooManyRequests {
					if originalErr == nil {
						originalErr = candidateErr
					}
					if i < len(candidates)-1 {
						continue candidateLoop
					}
					errChan <- originalErr
					return
				}
				if candidateErr == nil {
					candidateErr = &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: err}
				}
				errChan <- candidateErr
				return
			}

			for {
				var chunk coreexecutor.StreamChunk
				var ok bool
				if ctx != nil {
					select {
					case <-ctx.Done():
						return
					case chunk, ok = <-chunks:
					}
				} else {
					chunk, ok = <-chunks
				}
				if !ok {
					return
				}
				if chunk.Err != nil {
					streamErr := chunk.Err
					if !sentPayload {
						if bootstrapRetries < maxBootstrapRetries && bootstrapEligible(streamErr) {
							bootstrapRetries++
							retryChunks, retryErr := h.AuthManager.ExecuteStream(ctx, candidate.providers, req, opts)
							if retryErr == nil {
								chunks = retryChunks
								continue
							}
							streamErr = retryErr
						}
						if statusFromError(streamErr) == http.StatusTooManyRequests {
							if originalErr == nil {
								originalErr = errorMessageFromError(streamErr)
							}
							if i < len(candidates)-1 {
								continue candidateLoop
							}
							if originalErr != nil {
								errChan <- originalErr
								return
							}
						}
					}
					msg := errorMessageFromError(streamErr)
					if msg == nil {
						msg = &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: streamErr}
					}
					errChan <- msg
					return
				}
				if len(chunk.Payload) > 0 {
					sentPayload = true
					dataChan <- cloneBytes(chunk.Payload)
				}
			}
		}
		if originalErr != nil {
			errChan <- originalErr
		}
	}()
	return dataChan, errChan
}

func statusFromError(err error) int {
	if err == nil {
		return 0
	}
	if se, ok := err.(interface{ StatusCode() int }); ok && se != nil {
		if code := se.StatusCode(); code > 0 {
			return code
		}
	}
	return 0
}

func errorMessageFromError(err error) *interfaces.ErrorMessage {
	if err == nil {
		return nil
	}
	status := http.StatusInternalServerError
	if se, ok := err.(interface{ StatusCode() int }); ok && se != nil {
		if code := se.StatusCode(); code > 0 {
			status = code
		}
	}
	var addon http.Header
	if he, ok := err.(interface{ Headers() http.Header }); ok && he != nil {
		if hdr := he.Headers(); hdr != nil {
			addon = hdr.Clone()
		}
	}
	return &interfaces.ErrorMessage{StatusCode: status, Error: err, Addon: addon}
}

func (h *BaseAPIHandler) buildFallbackCandidates(requestedModel, normalizedModel string, rawJSON []byte) []fallbackCandidate {
	if h == nil || h.modelMapper == nil {
		return nil
	}
	requestedModel = strings.TrimSpace(requestedModel)
	normalizedModel = strings.TrimSpace(normalizedModel)
	if requestedModel == "" || normalizedModel == "" {
		return nil
	}

	thinkingSuffix := ""
	if strings.HasPrefix(requestedModel, normalizedModel) {
		thinkingSuffix = requestedModel[len(normalizedModel):]
	}

	mappedModels := h.modelMapper.MapModelCandidates(requestedModel)
	if len(mappedModels) == 0 && !strings.EqualFold(requestedModel, normalizedModel) {
		mappedModels = h.modelMapper.MapModelCandidates(normalizedModel)
	}
	if len(mappedModels) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(mappedModels))
	candidates := make([]fallbackCandidate, 0, len(mappedModels))
	for _, mapped := range mappedModels {
		mapped = strings.TrimSpace(mapped)
		if mapped == "" {
			continue
		}
		if thinkingSuffix != "" {
			_, mappedMetadata := util.NormalizeThinkingModel(mapped)
			if mappedMetadata == nil {
				mapped += thinkingSuffix
			}
		}

		baseModel, mappedMetadata := util.NormalizeThinkingModel(mapped)
		baseModel = strings.TrimSpace(baseModel)
		if baseModel == "" {
			continue
		}
		if strings.EqualFold(baseModel, normalizedModel) {
			continue
		}
		key := strings.ToLower(baseModel)
		if _, exists := seen[key]; exists {
			continue
		}

		providers := util.GetProviderName(baseModel)
		if len(providers) == 0 {
			continue
		}

		metadata := cloneMetadata(mappedMetadata)
		if metadata == nil {
			metadata = make(map[string]any, 1)
		}
		metadata[util.ModelMappingOriginalModelMetadataKey] = requestedModel

		seen[key] = struct{}{}
		candidates = append(candidates, fallbackCandidate{
			model:     mapped,
			baseModel: baseModel,
			providers: providers,
			metadata:  metadata,
			payload:   rewriteModelInRequest(rawJSON, mapped),
		})
	}
	return candidates
}

func rewriteModelInRequest(body []byte, newModel string) []byte {
	if newModel == "" {
		return body
	}
	if !gjson.GetBytes(body, "model").Exists() {
		return body
	}
	updated, err := sjson.SetBytes(body, "model", newModel)
	if err != nil {
		return body
	}
	return updated
}

func (h *BaseAPIHandler) getRequestDetails(modelName string) (providers []string, normalizedModel string, metadata map[string]any, err *interfaces.ErrorMessage) {
	// Resolve "auto" model to an actual available model first
	resolvedModelName := util.ResolveAutoModel(modelName)

	// Normalize the model name to handle dynamic thinking suffixes before determining the provider.
	normalizedModel, metadata = normalizeModelMetadata(resolvedModelName)

	// Use the normalizedModel to get the provider name.
	providers = util.GetProviderName(normalizedModel)
	if len(providers) == 0 && metadata != nil {
		if originalRaw, ok := metadata[util.ThinkingOriginalModelMetadataKey]; ok {
			if originalModel, okStr := originalRaw.(string); okStr {
				originalModel = strings.TrimSpace(originalModel)
				if originalModel != "" && !strings.EqualFold(originalModel, normalizedModel) {
					if altProviders := util.GetProviderName(originalModel); len(altProviders) > 0 {
						providers = altProviders
						normalizedModel = originalModel
					}
				}
			}
		}
	}

	if len(providers) == 0 {
		return nil, "", nil, &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: fmt.Errorf("unknown provider for model %s", modelName)}
	}

	// If it's a dynamic model, the normalizedModel was already set to extractedModelName.
	// If it's a non-dynamic model, normalizedModel was set by normalizeModelMetadata.
	// So, normalizedModel is already correctly set at this point.

	return providers, normalizedModel, metadata, nil
}

func cloneBytes(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}

func normalizeModelMetadata(modelName string) (string, map[string]any) {
	return util.NormalizeThinkingModel(modelName)
}

func cloneMetadata(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// WriteErrorResponse writes an error message to the response writer using the HTTP status embedded in the message.
func (h *BaseAPIHandler) WriteErrorResponse(c *gin.Context, msg *interfaces.ErrorMessage) {
	status := http.StatusInternalServerError
	if msg != nil && msg.StatusCode > 0 {
		status = msg.StatusCode
	}
	if msg != nil && msg.Addon != nil {
		for key, values := range msg.Addon {
			if len(values) == 0 {
				continue
			}
			c.Writer.Header().Del(key)
			for _, value := range values {
				c.Writer.Header().Add(key, value)
			}
		}
	}

	errText := http.StatusText(status)
	if msg != nil && msg.Error != nil {
		if v := strings.TrimSpace(msg.Error.Error()); v != "" {
			errText = v
		}
	}

	body := BuildErrorResponseBody(status, errText)
	// Append first to preserve upstream response logs, then drop duplicate payloads if already recorded.
	var previous []byte
	if existing, exists := c.Get("API_RESPONSE"); exists {
		if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
			previous = bytes.Clone(existingBytes)
		}
	}
	appendAPIResponse(c, body)
	trimmedErrText := strings.TrimSpace(errText)
	trimmedBody := bytes.TrimSpace(body)
	if len(previous) > 0 {
		if (trimmedErrText != "" && bytes.Contains(previous, []byte(trimmedErrText))) ||
			(len(trimmedBody) > 0 && bytes.Contains(previous, trimmedBody)) {
			c.Set("API_RESPONSE", previous)
		}
	}

	if !c.Writer.Written() {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Status(status)
	_, _ = c.Writer.Write(body)
}

func (h *BaseAPIHandler) LoggingAPIResponseError(ctx context.Context, err *interfaces.ErrorMessage) {
	if h.Cfg.RequestLog {
		if ginContext, ok := ctx.Value("gin").(*gin.Context); ok {
			if apiResponseErrors, isExist := ginContext.Get("API_RESPONSE_ERROR"); isExist {
				if slicesAPIResponseError, isOk := apiResponseErrors.([]*interfaces.ErrorMessage); isOk {
					slicesAPIResponseError = append(slicesAPIResponseError, err)
					ginContext.Set("API_RESPONSE_ERROR", slicesAPIResponseError)
				}
			} else {
				// Create new response data entry
				ginContext.Set("API_RESPONSE_ERROR", []*interfaces.ErrorMessage{err})
			}
		}
	}
}

// APIHandlerCancelFunc is a function type for canceling an API handler's context.
// It can optionally accept parameters, which are used for logging the response.
type APIHandlerCancelFunc func(params ...interface{})
