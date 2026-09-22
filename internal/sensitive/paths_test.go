package sensitive

import "testing"

func TestIsPath(t *testing.T) {
	for _, name := range []string{
		".env", "config/.env.production", "service.env", "service.env.local",
		"certs/server.pem", "TLS/SERVER.KEY", "identity.p12", "identity.pfx",
		"android/release.keystore", "android/release.jks", "home/.ssh/id_rsa", "home/.ssh/ID_ED25519",
		"home/.ssh/id_rsa.backup", "auth/key.p8", "auth/key.ppk",
		".env/private.go", ".npmrc", ".netrc", ".pypirc", ".pgpass", ".envrc",
		".aws/credentials", ".aws/config", ".kube/config", ".docker/config.json",
		".config/gcloud/application_default_credentials.json", "application_default_credentials.json",
		".zadig-review-agent/config.yaml", ".zadig-review-agent.yaml",
		"infra/terraform.tfstate", "infra/terraform.tfstate.backup", "infra/deploy.tfplan",
	} {
		if !IsPath(name) {
			t.Errorf("sensitive path was accepted: %q", name)
		}
	}
	for _, name := range []string{"main.go", "config/env.go", "docs/key-management.md", "identity.pub", "server.pem.example", "application.json", "infra/main.tf", "infra/variables.tfvars", ".zadig-review-agent.example.yaml"} {
		if IsPath(name) {
			t.Errorf("ordinary path was blocked: %q", name)
		}
	}
}
