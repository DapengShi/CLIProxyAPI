// Package amp provides model mapping functionality for routing Amp CLI requests
// to alternative models when the requested model is not available locally.
package amp

import (
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/modelmapping"
)

// ModelMapper provides model name mapping/aliasing for Amp CLI requests.
type ModelMapper = modelmapping.ModelMapper

// DefaultModelMapper implements ModelMapper with thread-safe mapping storage.
type DefaultModelMapper = modelmapping.DefaultModelMapper

// NewModelMapper creates a new model mapper with the given initial mappings.
func NewModelMapper(mappings []config.AmpModelMapping) *DefaultModelMapper {
	return modelmapping.NewModelMapper(mappings)
}
