package backend

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// AgyConversationID recovers only the conversation recorded for this workspace.
// The cache is best-effort: absent/changed metadata means a fresh conversation,
// never a guess at the globally most recent conversation.
func AgyConversationID(workspace string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".gemini", "antigravity-cli", "cache", "last_conversations.json"))
	if err != nil {
		return ""
	}
	var sessions map[string]string
	if json.Unmarshal(data, &sessions) != nil {
		return ""
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return ""
	}
	return sessions[abs]
}
