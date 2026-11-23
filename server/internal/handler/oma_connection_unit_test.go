package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOMAInputRetainsExistingSecrets(t *testing.T) {
	handler := &Handler{}
	existing := cloudAgentConfig{
		Name:          "saved",
		BaseURL:       "http://127.0.0.1:38080/v1",
		Token:         "saved-token",
		EnvironmentID: "env_saved",
		GitHubTokens:  map[string]string{"https://github.com/owner/repo": "saved-github"},
	}
	req := httptest.NewRequest(http.MethodPut, "/oma", strings.NewReader(`{"name":"OMA","environment_id":"env_next","github_tokens":{"https://github.com/owner/repo":""}}`))
	rec := httptest.NewRecorder()
	cfg, ok := handler.omaInput(rec, req, existing)
	if !ok {
		t.Fatalf("input rejected: %d %s", rec.Code, rec.Body.String())
	}
	if cfg.Token != "saved-token" || cfg.BaseURL != existing.BaseURL || cfg.GitHubTokens["https://github.com/owner/repo"] != "saved-github" || cfg.Name != "OMA" || cfg.EnvironmentID != "env_next" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.MaxConcurrency != defaultOMAConcurrency || cfg.RunTimeoutMinutes != defaultOMARunTimeout {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestOMAInputRequiresNewSecretsWhenBaseChanges(t *testing.T) {
	handler := &Handler{}
	existing := cloudAgentConfig{
		Name:          "saved",
		BaseURL:       "http://old-oma:38080/v1",
		Token:         "saved-token",
		EnvironmentID: "env_saved",
		GitHubTokens:  map[string]string{"https://github.com/owner/repo": "saved-github"},
	}
	call := func(body string) (cloudAgentConfig, bool, *httptest.ResponseRecorder) {
		req := httptest.NewRequest(http.MethodPut, "/oma", strings.NewReader(body))
		rec := httptest.NewRecorder()
		cfg, ok := handler.omaInput(rec, req, existing)
		return cfg, ok, rec
	}
	if _, ok, rec := call(`{"name":"OMA","base_url":"http://new-oma:38080/v1","environment_id":"env_saved"}`); ok || rec.Code != http.StatusBadRequest {
		t.Fatalf("base change without a new key was accepted: %d %s", rec.Code, rec.Body.String())
	}
	cfg, ok, _ := call(`{"name":"OMA","base_url":"http://new-oma:38080/v1","environment_id":"env_saved","api_token":"next-token"}`)
	if !ok || cfg.Token != "next-token" || cfg.GitHubTokens != nil {
		t.Fatalf("base change did not clear saved GitHub tokens: %+v", cfg)
	}
	if _, ok, rec := call(`{"name":"OMA","base_url":"http://new-oma:38080/v1","environment_id":"env_saved","api_token":"next-token","github_tokens":{"https://github.com/owner/repo":""}}`); ok || rec.Code != http.StatusBadRequest {
		t.Fatalf("base change reused a saved GitHub token: %d %s", rec.Code, rec.Body.String())
	}
	cfg, ok, _ = call(`{"name":"OMA","base_url":"http://new-oma:38080/v1","environment_id":"env_saved","api_token":"next-token","github_tokens":{"https://github.com/owner/repo":"next-github"}}`)
	if !ok || cfg.GitHubTokens["https://github.com/owner/repo"] != "next-github" {
		t.Fatalf("base change did not accept replacement GitHub token: %+v", cfg)
	}
}

func TestOMAInputRejectsInvalidTargets(t *testing.T) {
	handler := &Handler{}
	for name, body := range map[string]string{
		"missing base URL":     `{"name":"OMA","environment_id":"env_1","api_token":"k"}`,
		"missing token":        `{"name":"OMA","base_url":"http://127.0.0.1:38080/v1","environment_id":"env_1"}`,
		"bad environment":      `{"name":"OMA","base_url":"http://127.0.0.1:38080/v1","environment_id":"prod","api_token":"k"}`,
		"credentials in URL":   `{"name":"OMA","base_url":"http://user:pass@127.0.0.1:38080/v1","environment_id":"env_1","api_token":"k"}`,
		"unsupported scheme":   `{"name":"OMA","base_url":"ftp://127.0.0.1/v1","environment_id":"env_1","api_token":"k"}`,
		"unknown provider key": `{"name":"OMA","base_url":"http://127.0.0.1:38080/v1","environment_id":"env_1","anthropic_token":"k"}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/oma", strings.NewReader(body))
			rec := httptest.NewRecorder()
			if _, ok := handler.omaInput(rec, req, cloudAgentConfig{}); ok {
				t.Fatalf("accepted %s", body)
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestOMAConfigRoundTripsThroughStorage(t *testing.T) {
	spec := (&Handler{}).omaSpec()
	cfg := cloudAgentConfig{Name: "OMA", BaseURL: "http://127.0.0.1:38080/v1", Token: "secret", EnvironmentID: "env_1", MaxConcurrency: 3, RunTimeoutMinutes: 60}
	raw, err := spec.EncodeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "anthropic_token") {
		t.Fatal("OMA config reused the Anthropic token key")
	}
	decoded, err := spec.DecodeConfig(raw)
	if err != nil || decoded.Name != cfg.Name || decoded.BaseURL != cfg.BaseURL || decoded.Token != cfg.Token || decoded.EnvironmentID != cfg.EnvironmentID || decoded.MaxConcurrency != cfg.MaxConcurrency || decoded.RunTimeoutMinutes != cfg.RunTimeoutMinutes {
		t.Fatalf("decoded = %+v, err = %v", decoded, err)
	}
	if spec.TargetKey(cfg) == spec.TargetKey(cloudAgentConfig{BaseURL: cfg.BaseURL, EnvironmentID: "env_other"}) {
		t.Fatal("environment change did not alter the journal target")
	}
}
