package agent

const (
	ExitOK         = 0
	ExitBlocked    = 1
	ExitIncomplete = 2
	ExitCanceled   = 130
)

type Finding struct {
	Severity     string  `json:"severity"`
	Category     string  `json:"category"`
	RuleID       string  `json:"rule_id,omitempty"`
	File         string  `json:"file"`
	StartLine    int     `json:"start_line"`
	EndLine      int     `json:"end_line"`
	Title        string  `json:"title"`
	Problem      string  `json:"problem"`
	Evidence     string  `json:"evidence"`
	Suggestion   string  `json:"suggestion"`
	ExistingCode string  `json:"existing_code,omitempty"`
	Confidence   float64 `json:"confidence"`
	Fingerprint  string  `json:"fingerprint"`
}

type Report struct {
	Metadata      Metadata       `json:"metadata"`
	Stats         Stats          `json:"stats"`
	Usage         TokenUsage     `json:"usage"`
	DurationMS    int64          `json:"duration_ms"`
	Process       ReviewProcess  `json:"process"`
	ResolvedRules []ResolvedRule `json:"resolved_rules,omitempty"`
	ExcludedFiles []ExcludedFile `json:"excluded_files,omitempty"`
	Warnings      []string       `json:"warnings,omitempty"`
	Findings      []Finding      `json:"findings"`
	Incomplete    bool           `json:"incomplete"`
	Errors        []string       `json:"errors,omitempty"`
	ExitCode      int            `json:"exit_code"`
}

type ReviewProcess struct {
	ToolCalls      []ToolCall      `json:"tool_calls"`
	Compressions   []Compression   `json:"compressions"`
	ModelResponses []ModelResponse `json:"model_responses"`
}

type ModelResponse struct {
	ID              string     `json:"id"`
	Stage           string     `json:"stage"`
	File            string     `json:"file,omitempty"`
	Attempt         int        `json:"attempt"`
	Status          string     `json:"status"`
	StartedOffsetMS int64      `json:"started_offset_ms"`
	DurationMS      int64      `json:"duration_ms"`
	FinishReason    string     `json:"finish_reason,omitempty"`
	Text            string     `json:"text,omitempty"`
	Usage           TokenUsage `json:"usage"`
	Error           string     `json:"error,omitempty"`
}

type Compression struct {
	ID                 string     `json:"id"`
	File               string     `json:"file"`
	Round              int        `json:"round"`
	Status             string     `json:"status"`
	StartedOffsetMS    int64      `json:"started_offset_ms"`
	DurationMS         int64      `json:"duration_ms"`
	BeforeTokens       int        `json:"before_tokens"`
	AfterTokens        int        `json:"after_tokens"`
	CompressedMessages int        `json:"compressed_messages"`
	PreservedMessages  int        `json:"preserved_messages"`
	Usage              TokenUsage `json:"usage"`
	Error              string     `json:"error,omitempty"`
}

type ToolCall struct {
	ID              string        `json:"id"`
	File            string        `json:"file"`
	Round           int           `json:"round"`
	Tool            string        `json:"tool"`
	Arguments       ToolArguments `json:"arguments"`
	Status          string        `json:"status"`
	Cached          bool          `json:"cached,omitempty"`
	StartedOffsetMS int64         `json:"started_offset_ms"`
	DurationMS      int64         `json:"duration_ms"`
	OutputBytes     int           `json:"output_bytes"`
	OutputTruncated bool          `json:"output_truncated"`
	Summary         string        `json:"summary"`
	Output          string        `json:"output"`
}

type ToolArguments struct {
	FilePath      string   `json:"file_path,omitempty"`
	QueryName     string   `json:"query_name,omitempty"`
	SearchText    string   `json:"search_text,omitempty"`
	FilePatterns  []string `json:"file_patterns,omitempty"`
	CaseSensitive bool     `json:"case_sensitive,omitempty"`
	UsePerlRegexp bool     `json:"use_perl_regexp,omitempty"`
	StartLine     int      `json:"start_line,omitempty"`
	EndLine       int      `json:"end_line,omitempty"`
	Finding       *Finding `json:"finding,omitempty"`
}

type TokenUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	LLMRequests      int64 `json:"llm_requests"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

func (u *TokenUsage) Add(other TokenUsage) {
	u.PromptTokens += other.PromptTokens
	u.CompletionTokens += other.CompletionTokens
	u.TotalTokens += other.TotalTokens
	u.LLMRequests += other.LLMRequests
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheWriteTokens += other.CacheWriteTokens
}

type Metadata struct {
	DiffMode   string `json:"diff_mode"`
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
	Commit     string `json:"commit,omitempty"`
	Head       string `json:"head,omitempty"`
	Protocol   string `json:"protocol"`
	Model      string `json:"model"`
	Zadig      bool   `json:"zadig"`
	Repository string `json:"repository,omitempty"`
	Language   string `json:"language,omitempty"`
	ReportDir  string `json:"report_dir,omitempty"`
	JSONReport string `json:"json_report,omitempty"`
	MDReport   string `json:"markdown_report,omitempty"`
}

type Stats struct {
	ChangedFiles int            `json:"changed_files"`
	Chunks       int            `json:"chunks"`
	BySeverity   map[string]int `json:"by_severity"`
}

type ResolvedRule struct {
	File       string `json:"file"`
	Source     string `json:"source"`
	SourcePath string `json:"source_path,omitempty"`
	Pattern    string `json:"pattern"`
	Digest     string `json:"digest"`
}

type ExcludedFile struct {
	Path           string `json:"path"`
	Reason         string `json:"reason"`
	MatchedPattern string `json:"matched_pattern,omitempty"`
}
