package rules

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

//go:embed system_rules.json rule_docs/*
var embeddedSystemRules embed.FS

const (
	SourceCustom  = "Custom (--rule)"
	SourceProject = "Project config"
	SourceGlobal  = "Global config"
	SourceSystem  = "System built-in"
)

type RuleFile struct {
	Include []string    `json:"include,omitempty"`
	Exclude []string    `json:"exclude,omitempty"`
	Rules   []RuleEntry `json:"rules,omitempty"`
}

type RuleEntry struct {
	Path            string `json:"path"`
	Rule            string `json:"rule"`
	MergeSystemRule bool   `json:"merge_system_rule,omitempty"`
}

type ResolvedRule struct {
	Source     string
	SourcePath string
	Pattern    string
	Rule       string
	Digest     string
}

type Layer struct {
	Source string
	Path   string
	File   RuleFile
}

type Resolver struct {
	Layers     []Layer
	FilterFile RuleFile
	Warnings   []string
}

func NewResolver(repoRoot, customPath string) (Resolver, error) {
	var layers []Layer
	var filterFile RuleFile
	var warnings []string
	filterResolved := false
	appendLayer := func(layer Layer) {
		layers = append(layers, layer)
		if !filterResolved && (len(layer.File.Include) > 0 || len(layer.File.Exclude) > 0) {
			filterFile = layer.File
			filterResolved = true
		}
	}
	if customPath != "" {
		layer, layerWarnings, err := loadLayer(SourceCustom, customPath, filepath.Dir(customPath))
		if err != nil {
			return Resolver{}, err
		}
		warnings = append(warnings, layerWarnings...)
		appendLayer(layer)
	}
	projectPath := filepath.Join(repoRoot, ".zadig-review", "rules.json")
	project, found, layerWarnings, err := loadOptionalLayer(SourceProject, projectPath, repoRoot)
	if err != nil {
		return Resolver{}, err
	}
	warnings = append(warnings, layerWarnings...)
	if found {
		appendLayer(project)
	}
	globalPath := filepath.Join(userHomeDir(), ".zadig-review", "rules.json")
	global, found, layerWarnings, err := loadOptionalLayer(SourceGlobal, globalPath, filepath.Dir(globalPath))
	if err != nil {
		return Resolver{}, err
	}
	warnings = append(warnings, layerWarnings...)
	if found {
		appendLayer(global)
	}
	systemFile, err := SystemRuleFile()
	if err != nil {
		return Resolver{}, err
	}
	appendLayer(Layer{Source: SourceSystem, Path: "embedded:internal/rules/system_rules.json", File: systemFile})
	return Resolver{Layers: layers, FilterFile: filterFile, Warnings: warnings}, nil
}

func (r Resolver) Resolve(path string) ResolvedRule {
	for _, layer := range r.Layers {
		for _, entry := range layer.File.Rules {
			if strings.TrimSpace(entry.Rule) == "" && !entry.MergeSystemRule {
				continue
			}
			if Match(path, entry.Path) {
				rule := entry.Rule
				if entry.MergeSystemRule && layer.Source != SourceSystem {
					rule = mergeRules(r.resolveSystem(path).Rule, rule)
				}
				return resolvedText(layer, entry.Path, rule)
			}
		}
	}
	return ResolvedRule{Source: SourceSystem}
}

func (r Resolver) resolveSystem(path string) ResolvedRule {
	for _, layer := range r.Layers {
		if layer.Source != SourceSystem {
			continue
		}
		for _, entry := range layer.File.Rules {
			if strings.TrimSpace(entry.Rule) != "" && Match(path, entry.Path) {
				return resolved(layer, entry)
			}
		}
	}
	return ResolvedRule{}
}

func mergeRules(systemRule, userRule string) string {
	systemRule = strings.TrimSpace(systemRule)
	userRule = strings.TrimSpace(userRule)
	if systemRule == "" {
		return userRule
	}
	if userRule == "" {
		return systemRule
	}
	return "## System-Specific Rules (Mandatory)\n\n" + systemRule +
		"\n\n---\n\n## User-Specific Rules (Mandatory)\n\n" + userRule
}

func loadOptionalLayer(source, path, ruleBase string) (Layer, bool, []string, error) {
	layer, warnings, err := loadLayer(source, path, ruleBase)
	if errors.Is(err, os.ErrNotExist) {
		return Layer{}, false, nil, nil
	}
	if err != nil {
		return Layer{}, false, nil, err
	}
	return layer, true, warnings, nil
}

func loadLayer(source, path, ruleBase string) (Layer, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Layer{}, nil, err
	}
	file, err := parseRuleFile(path, data)
	if err != nil {
		return Layer{}, nil, err
	}
	warnings := resolveRuleEntries(file.Rules, ruleBase, path)
	return Layer{Source: source, Path: path, File: file}, warnings, nil
}

func parseRuleFile(path string, data []byte) (RuleFile, error) {
	var file RuleFile
	if err := json.Unmarshal(data, &file); err != nil {
		return RuleFile{}, fmt.Errorf("load rule file %s: %w", path, err)
	}
	for i, entry := range file.Rules {
		if strings.TrimSpace(entry.Path) == "" {
			return RuleFile{}, fmt.Errorf("load rule file %s: rules[%d] requires path", path, i)
		}
	}
	return file, nil
}

func resolved(layer Layer, entry RuleEntry) ResolvedRule {
	return resolvedText(layer, entry.Path, entry.Rule)
}

func resolvedText(layer Layer, pattern, rule string) ResolvedRule {
	sum := sha256.Sum256([]byte(rule))
	return ResolvedRule{
		Source:     layer.Source,
		SourcePath: layer.Path,
		Pattern:    pattern,
		Rule:       rule,
		Digest:     hex.EncodeToString(sum[:8]),
	}
}

func SystemRuleFile() (RuleFile, error) {
	data, err := readSystemRules("system_rules.json")
	if err != nil {
		return RuleFile{}, err
	}
	file, err := parseRuleFile("embedded:internal/rules/system_rules.json", data)
	if err != nil {
		return RuleFile{}, err
	}
	if len(file.Rules) == 0 {
		return RuleFile{}, fmt.Errorf("system rules must contain at least one rule")
	}
	hasDefault := false
	for index := range file.Rules {
		entry := &file.Rules[index]
		if entry.Path == "**" {
			hasDefault = true
		}
		if !looksLikeRuleFile(entry.Rule) {
			continue
		}
		content, err := readSystemRules(filepath.ToSlash(entry.Rule))
		if err != nil {
			return RuleFile{}, fmt.Errorf("read system rule %q for pattern %q: %w", entry.Rule, entry.Path, err)
		}
		entry.Rule = strings.TrimRight(string(content), "\n")
	}
	if !hasDefault {
		return RuleFile{}, fmt.Errorf("system rules must contain a ** default rule")
	}
	return file, nil
}

var readSystemRules = embeddedSystemRules.ReadFile

func Match(path, pattern string) bool {
	path = normalize(path)
	for _, expanded := range expandBraces(normalize(pattern)) {
		if matched, err := doublestar.Match(expanded, path); err == nil && matched {
			return true
		}
	}
	return false
}

func normalize(s string) string {
	return strings.ToLower(filepath.ToSlash(strings.TrimSpace(s)))
}

func expandBraces(pattern string) []string {
	start := strings.IndexByte(pattern, '{')
	if start < 0 {
		return []string{pattern}
	}
	end := strings.IndexByte(pattern[start:], '}')
	if end < 0 {
		return []string{pattern}
	}
	end += start
	prefix, suffix := pattern[:start], pattern[end+1:]
	var out []string
	for _, item := range strings.Split(pattern[start+1:end], ",") {
		for _, tail := range expandBraces(suffix) {
			out = append(out, prefix+item+tail)
		}
	}
	return out
}

var userHomeDir = func() string {
	home, _ := os.UserHomeDir()
	return home
}

var allowedRuleExtensions = map[string]bool{".md": true, ".txt": true, ".markdown": true}

func looksLikeRuleFile(value string) bool {
	return !strings.Contains(value, "\n") && !strings.Contains(value, " ") && allowedRuleExtensions[strings.ToLower(filepath.Ext(value))]
}

func resolveRuleEntries(entries []RuleEntry, baseDir, sourcePath string) []string {
	var warnings []string
	for index := range entries {
		entry := &entries[index]
		if strings.TrimSpace(entry.Rule) == "" || !looksLikeRuleFile(entry.Rule) {
			continue
		}
		content, err := readRuleReference(entry.Rule, baseDir)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("rule_reference_failed: %s rules[%d] %q: %v", sourcePath, index, entry.Rule, err))
			entry.Rule = ""
			continue
		}
		entry.Rule = content
	}
	return warnings
}

func readRuleReference(reference, baseDir string) (string, error) {
	path := reference
	if !filepath.IsAbs(path) {
		if strings.TrimSpace(baseDir) == "" {
			return "", fmt.Errorf("relative rule reference has no base directory")
		}
		base, err := filepath.Abs(baseDir)
		if err != nil {
			return "", err
		}
		path = filepath.Clean(filepath.Join(base, filepath.FromSlash(reference)))
		relative, err := filepath.Rel(base, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("relative path escapes rule base")
		}
	}
	return readRuleFileSafe(path)
}

func readRuleFileSafe(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if !allowedRuleExtensions[strings.ToLower(filepath.Ext(resolved))] {
		return "", fmt.Errorf("unsupported resolved extension %q", filepath.Ext(resolved))
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	const maxRuleFileSize = 512 * 1024
	if info.Size() > maxRuleFileSize {
		return "", fmt.Errorf("rule file is too large: %d bytes (max %d)", info.Size(), maxRuleFileSize)
	}
	content, err := os.ReadFile(resolved)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(content), "\n"), nil
}
