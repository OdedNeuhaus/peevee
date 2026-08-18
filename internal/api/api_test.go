package api

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/OdedNeuhaus/peevee/internal/config"
)

func configWithSecrets() config.Config {
	c := config.Default()
	c.RemoteWrite.Enabled = true
	c.RemoteWrite.URL = "https://mimir.example.com/api/v1/push"
	c.RemoteWrite.BasicAuth = &config.BasicAuth{
		Username: "peevee",
		Password: "hunter2-do-not-leak",
	}
	c.RemoteWrite.Headers = map[string]string{
		"X-Api-Token":    "tok-do-not-leak",
		"Authorization":  "Bearer do-not-leak",
		"X-Scope-Secret": "shh-do-not-leak",
		"X-Environment":  "production",
	}
	return c
}

// Both config views are rendered into a browser, so neither may carry a
// credential. YAML is the riskier of the two: the struct's yaml tags include
// the password field that the JSON tags deliberately drop.
func TestRedactedConfigLeaksNoSecretsAsYAML(t *testing.T) {
	out, err := yaml.Marshal(redact(configWithSecrets()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rendered := string(out)

	if strings.Contains(rendered, "do-not-leak") {
		t.Errorf("a secret survived redaction:\n%s", rendered)
	}
	// Non-secret configuration must still be visible, or the view is useless.
	if !strings.Contains(rendered, "peevee") {
		t.Error("the basic auth username should still be shown")
	}
	if !strings.Contains(rendered, "production") {
		t.Error("a header with no secret-shaped name should be shown as-is")
	}
	if !strings.Contains(rendered, "mimir.example.com") {
		t.Error("the remote write URL should still be shown")
	}
}

// Redaction must not mutate the configuration the collector is running on.
func TestRedactDoesNotMutateOriginal(t *testing.T) {
	original := configWithSecrets()
	_ = redact(original)

	if original.RemoteWrite.BasicAuth.Password != "hunter2-do-not-leak" {
		t.Error("redact cleared the password on the live config, not just on the copy")
	}
	if original.RemoteWrite.Headers["X-Api-Token"] != "tok-do-not-leak" {
		t.Error("redact rewrote the live header map, which the remote write client sends")
	}
}

func TestRedactHeaderNameMatching(t *testing.T) {
	c := config.Default()
	c.RemoteWrite.Headers = map[string]string{
		"X-Api-Key":       "secret",
		"X-Auth-Thing":    "secret",
		"X-Session-Token": "secret",
		"X-Client-Secret": "secret",
		"X-Team":          "platform",
		"X-Region":        "eu-west-1",
	}
	got := redact(c).RemoteWrite.Headers

	for _, name := range []string{"X-Api-Key", "X-Auth-Thing", "X-Session-Token", "X-Client-Secret"} {
		if got[name] != "***redacted***" {
			t.Errorf("%s = %q, want it redacted", name, got[name])
		}
	}
	for _, name := range []string{"X-Team", "X-Region"} {
		if got[name] == "***redacted***" {
			t.Errorf("%s was redacted, but it carries no credential", name)
		}
	}
}

// The YAML view is meant to be pasted back into the chart, so its top level
// must match the keys under `config:` in values.yaml.
func TestYAMLShapeMatchesHelmValues(t *testing.T) {
	out, err := yaml.Marshal(redact(config.Default()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("the rendered YAML does not parse: %v", err)
	}
	for _, key := range []string{"server", "discovery", "collector", "remoteWrite", "ui"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("missing top-level key %q", key)
		}
	}
}
