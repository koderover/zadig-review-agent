package reviewer

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/koderover/zadig-review-agent/internal/protocol"
)

//go:embed tools.json
var toolDefinitionsJSON []byte

func loadToolDefinitions() ([]protocol.ToolDefinition, error) {
	var definitions []protocol.ToolDefinition
	if err := json.Unmarshal(toolDefinitionsJSON, &definitions); err != nil {
		return nil, fmt.Errorf("load tool definitions: %w", err)
	}
	return terminalToolsFirst(definitions), nil
}

func terminalToolsFirst(definitions []protocol.ToolDefinition) []protocol.ToolDefinition {
	ordered := make([]protocol.ToolDefinition, 0, len(definitions))
	for _, name := range []string{"task_done", "code_comment"} {
		for _, definition := range definitions {
			if definition.Name == name {
				ordered = append(ordered, definition)
			}
		}
	}
	for _, definition := range definitions {
		if definition.Name != "task_done" && definition.Name != "code_comment" {
			ordered = append(ordered, definition)
		}
	}
	return ordered
}
