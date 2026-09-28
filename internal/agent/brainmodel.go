// brainmodel.go answers one question every CLI asker has to ask before it runs: which model does this ask want?
package agent

import (
	"os"
	"strings"

	"june/internal/config"
)

// pickedModel is the model an ask on the named brain should ask for. Input: the brain id GET /brains publishes ("claude", "antigravity"), the name of that brain's environment override, and the model to use when neither names one. Output: the model name, or "" when there is nothing to name and the caller passed no default, which leaves the CLI on its own default.
// The stored choice comes first because it is the user's own deliberate act: POST /brains writes it when a model is picked in Settings. Until this existed, picking a model changed the row on screen and nothing else — every asker read only its environment variable, so the picker was decoration.
func pickedModel(brainID, envName, fallback string) string {
	if model := strings.TrimSpace(config.LoadConfig().BrainModels[brainID]); model != "" {
		return model
	}
	if model := strings.TrimSpace(os.Getenv(envName)); model != "" {
		return model
	}
	return fallback
}
