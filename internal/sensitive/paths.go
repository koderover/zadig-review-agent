package sensitive

import (
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// These paths are never sent to the model, even when a review rule includes them.
// Match is case-insensitive so Git and filesystem access use the same policy.
var patterns = []string{
	"**/.env", "**/.env/**", "**/.env.*", "**/.env.*/**",
	"**/*.env", "**/*.env/**", "**/*.env.*", "**/*.env.*/**",
	"**/*.pem", "**/*.key", "**/*.p8", "**/*.pk8", "**/*.ppk",
	"**/*.p12", "**/*.pfx", "**/*.keystore", "**/*.jks",
	"**/id_rsa", "**/id_rsa.*", "**/id_dsa", "**/id_dsa.*",
	"**/id_ecdsa", "**/id_ecdsa.*", "**/id_ed25519", "**/id_ed25519.*",
	"**/.npmrc", "**/.netrc", "**/.pypirc", "**/.pgpass", "**/.envrc",
	"**/.aws/**", "**/.kube/**", "**/.docker/config.json",
	"**/.config/gcloud/**", "**/application_default_credentials.json",
	"**/.zadig-review-agent/**", "**/.zadig-review-agent.yaml",
	"**/*.tfstate", "**/*.tfstate.*", "**/*.tfplan",
}

func IsPath(name string) bool {
	name = strings.ToLower(strings.ReplaceAll(name, "\\", "/"))
	for _, pattern := range patterns {
		if matched, _ := doublestar.Match(pattern, name); matched {
			return true
		}
	}
	return false
}

func GitExcludePathspecs() []string {
	pathspecs := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		pathspecs = append(pathspecs, ":(exclude,icase,glob)"+pattern)
	}
	return pathspecs
}
