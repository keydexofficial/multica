package claudemanagedruntime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/cloudagent"
)

func omaProfile() Profile {
	return OMAProfile()
}

func TestOMAProfileIdentity(t *testing.T) {
	profile := OMAProfile()
	if profile.Provider != "oma_cloud" || profile.StandalonePrefix != "oma-bridge-" || profile.AgentBindingKey != "oma_agent_id" || profile.Usage != UsageSession || !profile.AllowPlainHTTP || !profile.BetaQuery || !profile.SkipWorkspaceKeyDetection || !profile.Chat || !profile.IssueAttachments || !profile.Leader || !profile.QuickCreate || !profile.CustomTools {
		t.Fatalf("OMA profile = %+v", profile)
	}
}

// The bridge's own gate must agree with the server's provider capability, or a
// task the server enqueues would be rejected after the fact (or the reverse).
func TestProfileChatMatchesProviderCapability(t *testing.T) {
	if got, want := OMAProfile().Chat, cloudagent.SupportsChat(cloudagent.OMAProvider); got != want {
		t.Fatalf("OMA profile chat = %v, provider capability = %v", got, want)
	}
	if got, want := ClaudeProfile().Chat, cloudagent.SupportsChat(cloudagent.ClaudeProvider); got != want {
		t.Fatalf("Claude profile chat = %v, provider capability = %v", got, want)
	}
}

func TestProfileNormalizesPartialOMA(t *testing.T) {
	got := normalizeProfile(Profile{Provider: cloudagent.OMAProvider})
	want := OMAProfile()
	if got.DisplayName != want.DisplayName || got.AgentNoun != want.AgentNoun || got.AgentBindingKey != want.AgentBindingKey || got.FailureReason != want.FailureReason || got.StandalonePrefix != want.StandalonePrefix || got.Usage != want.Usage || !got.AllowPlainHTTP || !got.BetaQuery || !got.SkipWorkspaceKeyDetection || !got.Chat || !got.IssueAttachments || !got.Leader || !got.QuickCreate || !got.CustomTools {
		t.Fatalf("normalized OMA profile = %+v", got)
	}
}

func TestProfileNormalizesFromClaude(t *testing.T) {
	profile := normalizeProfile(Profile{})
	want := ClaudeProfile()
	if profile != want {
		t.Fatalf("profile = %+v, want %+v", profile, want)
	}
}

func TestPlainHTTPPolicy(t *testing.T) {
	if _, err := newClient("http://example.com/v1", "token", nil, false, false); err == nil {
		t.Fatal("non-loopback HTTP accepted without opt-in")
	}
	if _, err := newClient("http://example.com/v1", "token", nil, true, false); err != nil {
		t.Fatalf("opted-in HTTP rejected: %v", err)
	}
	if _, err := newClient("http://127.0.0.1:1/v1", "token", nil, false, false); err != nil {
		t.Fatalf("loopback HTTP rejected: %v", err)
	}
}

func TestRegisterUsesProfileIdentity(t *testing.T) {
	var registered string
	multica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			DaemonID string `json:"daemon_id"`
			Runtimes []struct {
				Type string `json:"type"`
			} `json:"runtimes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		if len(input.Runtimes) != 1 {
			t.Fatalf("runtimes = %+v", input.Runtimes)
		}
		registered = input.Runtimes[0].Type
		if !strings.HasPrefix(input.DaemonID, "oma-bridge-") {
			t.Fatalf("daemon id = %q", input.DaemonID)
		}
		json.NewEncoder(w).Encode(map[string]any{"runtimes": []map[string]string{{"id": "rt_1", "provider": registered}}})
	}))
	defer multica.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{}`)
	}))
	defer provider.Close()
	b, err := NewWithProfile(Config{MulticaURL: multica.URL, MulticaToken: "multica-secret", WorkspaceID: workspaceID, AnthropicURL: provider.URL, AnthropicToken: "provider-secret", EnvironmentID: "env_1", StateDir: t.TempDir()}, omaProfile(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b.state = &journal{Binding: "test"}
	if err := b.register(context.Background(), "0123456789abcdef01234567"); err != nil {
		t.Fatal(err)
	}
	if registered != "oma_cloud" || b.state.RuntimeID != "rt_1" {
		t.Fatalf("registered = %q, runtime = %q", registered, b.state.RuntimeID)
	}
}

func TestBetaQueryOnCheck(t *testing.T) {
	multica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"`+workspaceID+`"}`)
	}))
	defer multica.Close()
	var queries []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Path+"?"+r.URL.RawQuery)
		id := strings.TrimPrefix(r.URL.Path, "/environments/")
		if strings.HasPrefix(r.URL.Path, "/vaults/") {
			id = strings.TrimPrefix(r.URL.Path, "/vaults/")
		}
		json.NewEncoder(w).Encode(map[string]string{"id": id})
	}))
	defer provider.Close()
	b, err := NewWithProfile(Config{MulticaURL: multica.URL, MulticaToken: "multica-secret", WorkspaceID: workspaceID, AnthropicURL: provider.URL, AnthropicToken: "provider-secret", EnvironmentID: "env_1", VaultIDs: []string{"vlt_1"}, StateDir: t.TempDir()}, omaProfile(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 {
		t.Fatalf("queries = %v", queries)
	}
	for _, query := range queries {
		if !strings.HasSuffix(query, "?beta=true") {
			t.Errorf("query without beta: %s", query)
		}
	}
}

func TestParseOMAAgentConfig(t *testing.T) {
	cfg, err := ParseAgentConfigForProfile(json.RawMessage(`{"oma_agent_id":"agent_1","repository":{"url":"https://github.com/acme/app","ref":"main"}}`), OMAProfile())
	if err != nil || cfg.AgentID != "agent_1" || cfg.Repository == nil || cfg.Repository.Ref != "main" {
		t.Fatalf("config = %+v, err = %v", cfg, err)
	}
	if _, err := ParseAgentConfigForProfile(json.RawMessage(`{"claude_agent_id":"agent_1"}`), OMAProfile()); err == nil || err.Error() != OMAProfile().AgentBindingError {
		t.Fatalf("foreign key error = %v", err)
	}
	if _, err := ParseAgentConfigForProfile(json.RawMessage(`{"oma_agent_id":"agent_1","repository":{"url":"https://github.com/acme/app","ref":"refs/tags/v1"}}`), OMAProfile()); err == nil || !strings.Contains(err.Error(), "OMA Managed Agents") {
		t.Fatalf("repository error did not name OMA: %v", err)
	}
}
