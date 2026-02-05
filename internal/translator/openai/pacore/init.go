package pacore

import (
	. "github.com/router-for-me/CLIProxyAPI/v6/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/openai/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		Claude,
		PaCoRe,
		claude.ConvertClaudeRequestToOpenAI,
		interfaces.TranslateResponse{
			Stream:     PaCoReToClaudeResponse,
			NonStream:  nil,
			TokenCount: claude.ClaudeTokenCount,
		},
	)
}
