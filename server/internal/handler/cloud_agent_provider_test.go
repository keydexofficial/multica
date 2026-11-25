package handler

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOMAProviderIdentityAndCapabilities(t *testing.T) {
	if got := cloudAgentServiceName("oma_cloud"); got != "OMA Managed Agents" {
		t.Fatalf("service name = %q", got)
	}
	if got := cloudAgentConnectionName("oma_cloud"); got != "OMA Managed Agents" {
		t.Fatalf("connection name = %q", got)
	}
	if !cloudAgentSupportsChat("oma_cloud") || !cloudAgentSupportsQuickCreate("oma_cloud") {
		t.Fatal("OMA capabilities incorrect")
	}
	if !isCloudAgentProvider("oma_cloud") {
		t.Fatal("OMA is not a cloud provider")
	}
}

func TestOMAConfigValidation(t *testing.T) {
	for _, raw := range []string{
		`{"oma_agent_id":"agent_abc"}`,
		`{"oma_agent_id":"agent_abc","repository":{"url":"https://github.com/acme/app","ref":"refs/heads/main"}}`,
	} {
		if err := validateCloudAgentConfig("oma_cloud", json.RawMessage(raw)); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		`{}`,
		`{"claude_agent_id":"agent_abc"}`,
		`{"oma_agent_id":"not-an-agent"}`,
		`{"oma_agent_id":"agent_abc","repository":{"url":"http://github.com/acme/app"}}`,
	} {
		if err := validateCloudAgentConfig("oma_cloud", json.RawMessage(raw)); err == nil {
			t.Fatalf("%s: accepted invalid OMA config", raw)
		}
	}
}

func TestOMARejectsAgentOverrides(t *testing.T) {
	err := (qoderOverrides{skills: 1}).validate("oma_cloud")
	if err == nil || !strings.Contains(err.Error(), "OMA agents") || !strings.Contains(err.Error(), "OMA agent") {
		t.Fatalf("override error = %v", err)
	}
}
