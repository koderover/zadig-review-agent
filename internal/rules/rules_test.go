package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolverPriorityAndFirstMatch(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, ".zadig-review", "rules.json")
	if err := os.MkdirAll(filepath.Dir(project), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte(`{"rules":[{"path":"**/*.go","rule":"first"},{"path":"**/*.go","rule":"second"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	got := resolver.Resolve("SRC/MAIN.GO")
	if got.Source != SourceProject || got.Rule != "first" {
		t.Fatalf("unexpected rule: %+v", got)
	}
}

func TestResolveAllNamedRulesKeepsLayerPriority(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	oldHome := userHomeDir
	userHomeDir = func() string { return home }
	defer func() { userHomeDir = oldHome }()
	project := filepath.Join(dir, ".zadig-review", "rules.json")
	if err := os.MkdirAll(filepath.Dir(project), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte(`{"rules":[{"name":"Go security","path":"**/*.go","rule":"security checks"},{"name":"Go style","path":"**/*.go","rule":"style checks"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(home, ".zadig-review", "rules.json")
	if err := os.MkdirAll(filepath.Dir(global), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte(`{"rules":[{"name":"Global Go","path":"**/*.go","rule":"global checks"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	got := resolver.ResolveAll("src/main.go")
	if len(got) != 2 || got[0].Name != "Go security" || got[0].ID != "project:1" || got[1].Name != "Go style" || got[1].ID != "project:2" {
		t.Fatalf("expected both project rules only: %+v", got)
	}
}

func TestRuleNamesAreUniqueAndUnnamedRulesHaveFallback(t *testing.T) {
	if _, err := parseRuleFile("rules.json", []byte(`{"rules":[{"name":"same","path":"a","rule":"one"},{"name":"same","path":"b","rule":"two"}]}`)); err == nil || !strings.Contains(err.Error(), "duplicate rule name") {
		t.Fatalf("expected duplicate name error, got %v", err)
	}
	resolver := Resolver{Layers: []Layer{{Source: SourceProject, File: RuleFile{Rules: []RuleEntry{{Path: "**/*.go", Rule: "one"}, {Path: "**/*.go", Rule: "two"}}}}}}
	got := resolver.ResolveAll("main.go")
	if len(got) != 2 || got[0].Name != "Project config #1" || got[1].Name != "Project config #2" {
		t.Fatalf("unexpected unnamed rule labels: %+v", got)
	}
}

func TestCustomFallsThroughProjectGlobalAndSystem(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	oldHome := userHomeDir
	userHomeDir = func() string { return home }
	defer func() { userHomeDir = oldHome }()

	project := filepath.Join(dir, ".zadig-review", "rules.json")
	if err := os.MkdirAll(filepath.Dir(project), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte(`{"rules":[{"path":"**/*.go","rule":"project"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(home, ".zadig-review", "rules.json")
	if err := os.MkdirAll(filepath.Dir(global), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte(`{"rules":[{"path":"**/*.py","rule":"global"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(dir, "custom.json")
	if err := os.WriteFile(custom, []byte(`{"rules":[{"path":"**/*.java","rule":"custom"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(dir, custom)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path   string
		source string
		rule   string
	}{
		{"src/Main.java", SourceCustom, "custom"},
		{"main.go", SourceProject, "project"},
		{"tools/check.py", SourceGlobal, "global"},
	} {
		got := resolver.Resolve(test.path)
		if got.Source != test.source || got.Rule != test.rule {
			t.Fatalf("resolve %s: got %+v", test.path, got)
		}
	}
	system := resolver.Resolve("deploy.yaml")
	if system.Source != SourceSystem || system.Pattern != "**/*.{yaml,yml}" || !strings.Contains(system.Rule, "duplicate keys") {
		t.Fatalf("resolve system fallback: got %+v", system)
	}
	if len(resolver.Layers) != 4 {
		t.Fatalf("expected custom, project, global and system layers: %+v", resolver.Layers)
	}
}

func TestFilterConfigFallsThroughEmptyCustomLayer(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, ".zadig-review", "rules.json")
	if err := os.MkdirAll(filepath.Dir(project), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte(`{"exclude":["vendor/**"],"rules":[{"path":"*","rule":"project"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(dir, "custom.json")
	if err := os.WriteFile(custom, []byte(`{"rules":[{"path":"**/*.go","rule":"custom"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(dir, custom)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolver.FilterFile.Exclude) != 1 || resolver.FilterFile.Exclude[0] != "vendor/**" {
		t.Fatalf("expected project filter fallback: %+v", resolver.FilterFile)
	}
}

func TestMatchBraceExpansion(t *testing.T) {
	if !Match("src/app.TSX", "src/**/*.{ts,tsx}") {
		t.Fatal("expected brace glob to match case-insensitively")
	}
}

func TestSystemRuleFileLoadsEmbeddedJSON(t *testing.T) {
	file, err := SystemRuleFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Rules) == 0 {
		t.Fatal("expected embedded system rules")
	}
	for index, entry := range file.Rules {
		if entry.Name == "" {
			t.Fatalf("system rule %d has no name", index)
		}
	}
	got := Resolver{Layers: []Layer{{Source: SourceSystem, File: file}}}.Resolve("src/main.java")
	if got.Source != SourceSystem || got.Pattern != "**/*.java" {
		t.Fatalf("unexpected system rule: %+v", got)
	}
	goRule := Resolver{Layers: []Layer{{Source: SourceSystem, File: file}}}.Resolve("internal/reviewer/reviewer.go")
	if goRule.Source != SourceSystem || goRule.Pattern != "**/*.go" || !strings.Contains(goRule.Rule, "#### Context and Cancellation") {
		t.Fatalf("unexpected Go system rule: %+v", goRule)
	}
	fallback := Resolver{Layers: []Layer{{Source: SourceSystem, File: file}}}.Resolve("nested/unknown.xyz")
	if fallback.Source != SourceSystem || fallback.Pattern != "**" || fallback.Name != "General review" || !strings.Contains(fallback.Rule, "#### Correctness") {
		t.Fatalf("unexpected system fallback: %+v", fallback)
	}
}

func TestMergeSystemRule(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, ".zadig-review", "rules.json")
	if err := os.MkdirAll(filepath.Dir(project), 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"rules":[{"path":"**/*.java","rule":"project requirement","merge_system_rule":true},{"path":"**/*.java","rule":"second requirement","merge_system_rule":true}]}`
	if err := os.WriteFile(project, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	got := resolver.ResolveAll("src/main.java")
	if len(got) != 3 || got[0].Source != SourceProject || got[0].Rule != "project requirement" || got[1].Rule != "second requirement" || got[2].Source != SourceSystem || got[2].Name != "Java review" || !strings.Contains(got[2].Rule, "Logic Error Detection") {
		t.Fatalf("unexpected merged rules: %+v", got)
	}
}

func TestEmptyRuleFallsThroughUnlessMergingSystem(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, ".zadig-review", "rules.json")
	if err := os.MkdirAll(filepath.Dir(project), 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"rules":[{"path":"**/*.go","rule":""},{"path":"**/*.java","rule":"","merge_system_rule":true}]}`
	if err := os.WriteFile(project, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := resolver.Resolve("main.go"); got.Source != SourceSystem {
		t.Fatalf("empty rule must fall through: %+v", got)
	}
	if got := resolver.ResolveAll("main.java"); len(got) != 1 || got[0].Source != SourceSystem || !strings.Contains(got[0].Rule, "Logic Error Detection") {
		t.Fatalf("empty merged rule must retain system rule: %+v", got)
	}
}

func TestFailedMergedRuleReferenceFallsThroughToProject(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, ".zadig-review", "rules.json")
	if err := os.MkdirAll(filepath.Dir(project), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte(`{"rules":[{"name":"Project Go","path":"**/*.go","rule":"project checks"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(dir, "custom.json")
	if err := os.WriteFile(custom, []byte(`{"rules":[{"name":"Broken Go","path":"**/*.go","rule":"missing.md","merge_system_rule":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(dir, custom)
	if err != nil {
		t.Fatal(err)
	}
	got := resolver.ResolveAll("main.go")
	if len(resolver.Warnings) != 1 || len(got) != 1 || got[0].Source != SourceProject || got[0].Name != "Project Go" {
		t.Fatalf("failed custom reference must fall through: rules=%+v warnings=%v", got, resolver.Warnings)
	}
}

func TestRuleFileReferencesUseLayerBaseDirectories(t *testing.T) {
	repo := t.TempDir()
	project := filepath.Join(repo, ".zadig-review", "rules.json")
	if err := os.MkdirAll(filepath.Dir(project), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs", "go.md"), []byte("project file rule\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte(`{"rules":[{"path":"**/*.go","rule":"docs/go.md"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	customDir := t.TempDir()
	custom := filepath.Join(customDir, "custom.json")
	if err := os.WriteFile(filepath.Join(customDir, "java.txt"), []byte("custom file rule\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(custom, []byte(`{"rules":[{"path":"**/*.java","rule":"java.txt"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(repo, custom)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolver.Resolve("main.java"); got.Rule != "custom file rule" {
		t.Fatalf("custom reference must resolve beside custom JSON: %+v", got)
	}
	if got := resolver.Resolve("main.go"); got.Rule != "project file rule" {
		t.Fatalf("project reference must resolve from repository root: %+v", got)
	}
}

func TestUnsafeOrMissingRuleReferenceWarnsAndFallsThrough(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, ".zadig-review", "rules.json")
	if err := os.MkdirAll(filepath.Dir(project), 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"rules":[{"path":"**/*.go","rule":"../outside.md"},{"path":"**/*.java","rule":"missing.md"}]}`
	if err := os.WriteFile(project, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(resolver.Warnings) != 2 || !strings.Contains(resolver.Warnings[0], "escapes rule base") || !strings.Contains(resolver.Warnings[1], "missing.md") {
		t.Fatalf("unexpected rule reference warnings: %v", resolver.Warnings)
	}
	if got := resolver.Resolve("main.go"); got.Source != SourceSystem {
		t.Fatalf("invalid reference must fall through: %+v", got)
	}
}

func TestRuleReferenceRejectsSymlinkToUnsupportedExtension(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "secret.json")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "rule.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readRuleReference(link, dir); err == nil || !strings.Contains(err.Error(), "unsupported resolved extension") {
		t.Fatalf("expected symlink target extension rejection, got %v", err)
	}
}

func TestRuleReferenceRejectsSensitivePathAndSymlink(t *testing.T) {
	dir := t.TempDir()
	secretDir := filepath.Join(dir, ".env")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(secretDir, "private.md")
	if err := os.WriteFile(secret, []byte("SECRET_MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRuleReference(".env/private.md", dir); err == nil || !strings.Contains(err.Error(), "sensitive rule reference is blocked") {
		t.Fatalf("sensitive rule reference was accepted: %v", err)
	}
	link := filepath.Join(dir, "rule.md")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := readRuleReference("rule.md", dir); err == nil || !strings.Contains(err.Error(), "sensitive rule reference is blocked") {
		t.Fatalf("sensitive symlink target was accepted: %v", err)
	}
}

func TestCustomRuleFileRejectsSensitivePath(t *testing.T) {
	dir := t.TempDir()
	secretDir := filepath.Join(dir, ".env")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(secretDir, "rules.json")
	if err := os.WriteFile(path, []byte(`{"rules":[{"path":"**","rule":"SECRET_MARKER"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewResolver(dir, path); err == nil || !strings.Contains(err.Error(), "sensitive rule file path is blocked") {
		t.Fatalf("sensitive custom rule file was accepted: %v", err)
	}
	link := filepath.Join(dir, "rules.json")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := NewResolver(dir, link); err == nil || !strings.Contains(err.Error(), "sensitive rule file path is blocked") {
		t.Fatalf("sensitive custom rule symlink was accepted: %v", err)
	}
}

func TestRuleReferenceRejectsOversizedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.md")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 512*1024+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRuleReference(path, dir); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected oversized rule rejection, got %v", err)
	}
}
