package reviewer

import (
	"embed"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync"

	tiktoken "github.com/pkoukk/tiktoken-go"
)

//go:embed bpe_data/cl100k_base.tiktoken
var bpeData embed.FS

type embeddedBPELoader struct{}

func (embeddedBPELoader) LoadTiktokenBpe(url string) (map[string]int, error) {
	if url != "https://openaipublic.blob.core.windows.net/encodings/cl100k_base.tiktoken" {
		return nil, fmt.Errorf("unsupported tiktoken encoding: %s", url)
	}
	data, err := bpeData.ReadFile("bpe_data/cl100k_base.tiktoken")
	if err != nil {
		return nil, err
	}
	ranks := make(map[string]int)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid BPE data line: %q", line)
		}
		token, err := base64.StdEncoding.DecodeString(parts[0])
		if err != nil {
			return nil, err
		}
		rank, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, err
		}
		ranks[string(token)] = rank
	}
	return ranks, nil
}

var (
	tokenizerOnce sync.Once
	tokenizer     *tiktoken.Tiktoken
)

// estimateTokens matches OCR's default CountTokens: cl100k_base BPE with a
// byte-based fallback. It is still an estimate for models with other tokenizers.
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	tokenizerOnce.Do(func() {
		tiktoken.SetBpeLoader(embeddedBPELoader{})
		tokenizer, _ = tiktoken.GetEncoding("cl100k_base")
	})
	if tokenizer == nil {
		return (len(text) + 3) / 4
	}
	return len(tokenizer.Encode(text, nil, nil))
}
