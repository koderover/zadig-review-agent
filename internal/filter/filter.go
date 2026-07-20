package filter

import (
	"path"
	"path/filepath"
	"strings"

	"github.com/koderover/zadig-review-agent/internal/agent"
	"github.com/koderover/zadig-review-agent/internal/gitdiff"
	"github.com/koderover/zadig-review-agent/internal/rules"
)

const (
	ReasonBinary         = "binary"
	ReasonDeleted        = "deleted"
	ReasonUserExclude    = "user_exclude"
	ReasonUnsupportedExt = "unsupported_ext"
	ReasonDefaultPath    = "default_path"
	ReasonInvalidPath    = "invalid_path"
)

type Result struct {
	Kept     []gitdiff.FileDiff
	Excluded []agent.ExcludedFile
}

type Options struct {
	RuleFile rules.RuleFile
}

func Apply(files []gitdiff.FileDiff, opts Options) Result {
	var result Result
	for _, f := range files {
		clean, ok := CleanRelative(f.Path)
		if !ok {
			result.Excluded = append(result.Excluded, agent.ExcludedFile{Path: f.Path, Reason: ReasonInvalidPath})
			continue
		}
		f.Path = clean
		if f.IsBinary {
			result.Excluded = append(result.Excluded, agent.ExcludedFile{Path: clean, Reason: ReasonBinary})
			continue
		}
		if f.IsDeleted {
			result.Excluded = append(result.Excluded, agent.ExcludedFile{Path: clean, Reason: ReasonDeleted})
			continue
		}
		if pattern, ok := matchAny(clean, opts.RuleFile.Exclude); ok {
			result.Excluded = append(result.Excluded, agent.ExcludedFile{Path: clean, Reason: ReasonUserExclude, MatchedPattern: pattern})
			continue
		}
		if _, ok := matchAny(clean, opts.RuleFile.Include); ok {
			result.Kept = append(result.Kept, f)
			continue
		}
		if !supported(clean) {
			result.Excluded = append(result.Excluded, agent.ExcludedFile{Path: clean, Reason: ReasonUnsupportedExt})
			continue
		}
		if pattern, ok := matchAny(clean, DefaultExcludePatterns); ok {
			result.Excluded = append(result.Excluded, agent.ExcludedFile{Path: clean, Reason: ReasonDefaultPath, MatchedPattern: pattern})
			continue
		}
		result.Kept = append(result.Kept, f)
	}
	return result
}

func CleanRelative(p string) (string, bool) {
	p = filepath.ToSlash(strings.TrimSpace(p))
	if p == "" || path.IsAbs(p) {
		return "", false
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

func matchAny(p string, patterns []string) (string, bool) {
	for _, pattern := range patterns {
		if rules.Match(p, filepath.ToSlash(pattern)) {
			return pattern, true
		}
	}
	return "", false
}

func supported(p string) bool {
	base := strings.ToLower(path.Base(p))
	if SupportedBasenames[base] {
		return true
	}
	ext := strings.ToLower(path.Ext(p))
	return SupportedExtensions[ext]
}

var SupportedBasenames = map[string]bool{
	"dockerfile":   true,
	"makefile":     true,
	"pom.xml":      true,
	"build.gradle": true,
	"package.json": true,
	"cargo.toml":   true,
}

var SupportedExtensions = map[string]bool{
	".c":          true,
	".cc":         true,
	".cpp":        true,
	".h":          true,
	".hpp":        true,
	".go":         true,
	".java":       true,
	".kt":         true,
	".kts":        true,
	".scala":      true,
	".groovy":     true,
	".rs":         true,
	".py":         true,
	".pyi":        true,
	".rb":         true,
	".rake":       true,
	".gemspec":    true,
	".php":        true,
	".ts":         true,
	".tsx":        true,
	".js":         true,
	".jsx":        true,
	".mjs":        true,
	".cjs":        true,
	".ets":        true,
	".cxx":        true,
	".hxx":        true,
	".cs":         true,
	".vb":         true,
	".fs":         true,
	".swift":      true,
	".m":          true,
	".mm":         true,
	".xml":        true,
	".json":       true,
	".json5":      true,
	".yaml":       true,
	".yml":        true,
	".properties": true,
	".md":         true,
	".sh":         true,
	".bash":       true,
	".zsh":        true,
	".fish":       true,
	".ps1":        true,
	".sql":        true,
	".css":        true,
	".scss":       true,
	".sass":       true,
	".less":       true,
	".html":       true,
	".htm":        true,
	".vue":        true,
	".svelte":     true,
	".toml":       true,
	".ini":        true,
	".env":        true,
	".gradle":     true,
	".cmake":      true,
	".r":          true,
	".lua":        true,
	".pl":         true,
	".pm":         true,
	".ex":         true,
	".exs":        true,
	".erl":        true,
	".hrl":        true,
	".dart":       true,
	".tf":         true,
}

var DefaultExcludePatterns = []string{
	"**/*_test.go",
	"**/src/test/java/**/*.java",
	"**/src/test/**/*.kt",
	"**/*.test.{js,jsx,ts,tsx}",
	"**/*.spec.{js,jsx,ts,tsx}",
	"**/__tests__/**",
	"**/test/**/*_test.py",
	"**/tests/**/*_test.py",
	"**/*_test.py",
	"**/*_spec.rb",
	"**/spec/**/*_spec.rb",
	"**/*Test.java",
	"**/*Tests.java",
	"**/*_test.rs",
	"**/oh_modules/**",
	"**/*.test.ets",
}
