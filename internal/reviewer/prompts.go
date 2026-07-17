package reviewer

import (
	"embed"
	"fmt"
	"strings"

	"github.com/koderover/zadig-code-review-agent/internal/protocol"
)

//go:embed prompts/*.md
var promptFiles embed.FS

func loadPrompt(name string, values map[string]string) (string, error) {
	data, err := promptFiles.ReadFile("prompts/" + name + ".md")
	if err != nil {
		return "", fmt.Errorf("load prompt %s: %w", name, err)
	}
	prompt := string(data)
	for key, value := range values {
		prompt = strings.ReplaceAll(prompt, "{{"+key+"}}", value)
	}
	return prompt, nil
}

func loadPromptMessages(name string, values map[string]string) ([]protocol.Message, error) {
	system, err := loadPrompt(name+"_system", values)
	if err != nil {
		return nil, err
	}
	user, err := loadPrompt(name+"_user", values)
	if err != nil {
		return nil, err
	}
	return []protocol.Message{
		{Role: protocol.RoleSystem, Content: system},
		{Role: protocol.RoleUser, Content: user},
	}, nil
}
