package reviewer

import (
	"strings"
	"testing"
)

func TestEstimateTokensUsesBPE(t *testing.T) {
	if got := estimateTokens(""); got != 0 {
		t.Fatalf("empty text: got %d tokens, want 0", got)
	}
	if got := estimateTokens("hello world"); got != 2 {
		t.Fatalf("common English phrase: got %d tokens, want 2", got)
	}

	// The old rune-count / 4 estimate gave just 20 tokens for this text.
	chinese := strings.Repeat("你好世界", 20)
	if got := estimateTokens(chinese); got <= 20 {
		t.Fatalf("Chinese text: got %d tokens, expected more than the old rune estimate", got)
	}
}
