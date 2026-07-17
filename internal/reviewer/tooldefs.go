package reviewer

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/koderover/zadig-code-review-agent/internal/protocol"
)

//go:embed tools.json
var toolDefinitionsJSON []byte

func loadToolDefinitions() ([]protocol.ToolDefinition, error) {
	var definitions []protocol.ToolDefinition
	if err := json.Unmarshal(toolDefinitionsJSON, &definitions); err != nil {
		return nil, fmt.Errorf("load tool definitions: %w", err)
	}
	return definitions, nil
}
