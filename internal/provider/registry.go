package provider

import (
	"fmt"

	"coder/internal/provider/anthropic"
	"coder/internal/provider/copilot"
	"coder/internal/provider/openaicompat"
	"coder/internal/types"
)

// New instantiates the model backend specified by profile.Type:
//   - types.ProviderTypeCopilotSDK   -> *copilot.Provider (implements types.ManagedSessionProvider)
//   - types.ProviderTypeOpenAICompat -> *openaicompat.Provider (implements types.ChatProvider)
//   - types.ProviderTypeAnthropic    -> *anthropic.Provider (implements types.ChatProvider)
func New(profile types.ProviderProfile) (types.Provider, error) {
	switch profile.Type {
	case types.ProviderTypeCopilotSDK:
		return copilot.New(profile)
	case types.ProviderTypeOpenAICompat:
		return openaicompat.New(profile)
	case types.ProviderTypeAnthropic:
		return anthropic.New(profile)
	default:
		return nil, fmt.Errorf("unsupported provider type %q", profile.Type)
	}
}
