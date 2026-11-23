package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/claudemanagedruntime"
	"github.com/multica-ai/multica/server/internal/cloudagent"
)

const (
	defaultOMAConcurrency = 3
	defaultOMARunTimeout  = 60
	maxOMARunTimeout      = 240
)

type omaStoredConfig struct {
	Name              string            `json:"name"`
	BaseURL           string            `json:"base_url"`
	APIToken          string            `json:"api_token,omitempty"`
	EnvironmentID     string            `json:"environment_id"`
	GitHubTokens      map[string]string `json:"github_tokens,omitempty"`
	VaultIDs          []string          `json:"vault_ids,omitempty"`
	MulticaToken      string            `json:"multica_token,omitempty"`
	MaxConcurrency    int               `json:"max_concurrency,omitempty"`
	RunTimeoutMinutes int               `json:"run_timeout_minutes,omitempty"`
}

func (cfg omaStoredConfig) toCloudAgentConfig() cloudAgentConfig {
	return cloudAgentConfig{
		Name:              cfg.Name,
		BaseURL:           cfg.BaseURL,
		Token:             cfg.APIToken,
		EnvironmentID:     cfg.EnvironmentID,
		VaultIDs:          cfg.VaultIDs,
		GitHubTokens:      cfg.GitHubTokens,
		MulticaToken:      cfg.MulticaToken,
		MaxConcurrency:    cfg.MaxConcurrency,
		RunTimeoutMinutes: cfg.RunTimeoutMinutes,
	}
}

func cloudAgentConfigToOMA(cfg cloudAgentConfig) omaStoredConfig {
	return omaStoredConfig{
		Name:              cfg.Name,
		BaseURL:           cfg.BaseURL,
		APIToken:          cfg.Token,
		EnvironmentID:     cfg.EnvironmentID,
		GitHubTokens:      cfg.GitHubTokens,
		VaultIDs:          cfg.VaultIDs,
		MulticaToken:      cfg.MulticaToken,
		MaxConcurrency:    cfg.MaxConcurrency,
		RunTimeoutMinutes: cfg.RunTimeoutMinutes,
	}
}

func (h *Handler) omaSpec() cloudAgentSpec {
	descriptor, _ := cloudagent.Lookup(cloudagent.OMAProvider)
	return cloudAgentSpec{
		Descriptor:         descriptor,
		DefaultConcurrency: defaultOMAConcurrency,
		DefaultRunTimeout:  defaultOMARunTimeout,
		DecodeConfig: func(raw []byte) (cloudAgentConfig, error) {
			var cfg omaStoredConfig
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return cloudAgentConfig{}, err
			}
			return cfg.toCloudAgentConfig(), nil
		},
		EncodeConfig: func(cfg cloudAgentConfig) ([]byte, error) {
			return json.Marshal(cloudAgentConfigToOMA(cfg))
		},
		ParseInput: func(h *Handler, w http.ResponseWriter, r *http.Request, existing cloudAgentConfig) (cloudAgentConfig, bool) {
			return h.omaInput(w, r, existing)
		},
		TargetKey: func(cfg cloudAgentConfig) string {
			return cfg.BaseURL + "\x00" + cfg.EnvironmentID
		},
		BuildBridge:         buildOMABridge,
		CheckAccess:         checkOMAConnectionAccess,
		CancelRemoteSession: cancelOMARemoteSession,
		CancelUnconfirmed:   cancelOMAUnconfirmedSession,
	}
}

func buildOMABridge(cfg cloudAgentBridgeConfig, hooks cloudAgentHooks) (cloudAgentRunner, error) {
	return claudemanagedruntime.NewWithProfile(claudemanagedruntime.Config{
		DaemonID:       cfg.DaemonID,
		LoadState:      hooks.LoadState,
		SaveState:      hooks.SaveState,
		OnReady:        hooks.OnReady,
		BeforeStep:     hooks.BeforeStep,
		MulticaURL:     cfg.MulticaURL,
		MulticaToken:   cfg.MulticaToken,
		WorkspaceID:    cfg.WorkspaceID,
		AnthropicURL:   cfg.BaseURL,
		AnthropicToken: cfg.Token,
		EnvironmentID:  cfg.EnvironmentID,
		VaultIDs:       cfg.VaultIDs,
		Name:           cfg.Name,
		MaxConcurrency: cfg.MaxConcurrency,
		RunTimeout:     time.Duration(cfg.RunTimeoutMinutes) * time.Minute,
		GitHubTokens:   cfg.GitHubTokens,
	}, claudemanagedruntime.OMAProfile(), nil)
}

func checkOMAConnectionAccess(ctx context.Context, baseURL, token, environmentID string, vaultIDs []string) error {
	return claudemanagedruntime.CheckAccessWithProfile(ctx, baseURL, token, environmentID, vaultIDs, claudemanagedruntime.OMAProfile())
}

func listOMACatalog(ctx context.Context, baseURL, token, kind string) ([]claudemanagedruntime.CatalogItem, error) {
	return claudemanagedruntime.ListCatalogWithProfile(ctx, baseURL, token, kind, claudemanagedruntime.OMAProfile())
}

func cancelOMARemoteSession(ctx context.Context, baseURL, token, sessionID string) error {
	return claudemanagedruntime.CancelRemoteSessionWithProfile(ctx, baseURL, token, sessionID, claudemanagedruntime.OMAProfile())
}

func cancelOMAUnconfirmedSession(ctx context.Context, baseURL, token, workspaceID, agentID, taskID string, attemptAt time.Time) error {
	return claudemanagedruntime.CancelUnconfirmedSessionWithProfile(ctx, baseURL, token, workspaceID, agentID, taskID, attemptAt, claudemanagedruntime.OMAProfile())
}

func (h *Handler) omaInput(w http.ResponseWriter, r *http.Request, existing cloudAgentConfig) (cloudAgentConfig, bool) {
	var req struct {
		Name              string            `json:"name"`
		BaseURL           string            `json:"base_url"`
		APIToken          string            `json:"api_token"`
		EnvironmentID     string            `json:"environment_id"`
		GitHubTokens      map[string]string `json:"github_tokens"`
		VaultIDs          []string          `json:"vault_ids"`
		MaxConcurrency    int               `json:"max_concurrency"`
		RunTimeoutMinutes int               `json:"run_timeout_minutes"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, 400, "invalid OMA Managed Agents configuration")
		return cloudAgentConfig{}, false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		writeError(w, 400, "configuration must contain one JSON object")
		return cloudAgentConfig{}, false
	}
	cfg := existing
	cfg.Name = strings.TrimSpace(req.Name)
	cfg.EnvironmentID = strings.TrimSpace(req.EnvironmentID)
	cfg.BaseURL = strings.TrimSpace(req.BaseURL)
	if cfg.BaseURL == "" {
		cfg.BaseURL = existing.BaseURL
	}
	if cfg.Name == "" || cfg.EnvironmentID == "" || cfg.BaseURL == "" || !strings.HasPrefix(cfg.EnvironmentID, "env_") {
		writeError(w, 400, "name, base URL and environment are required")
		return cloudAgentConfig{}, false
	}
	if parsed, err := url.Parse(cfg.BaseURL); err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		writeError(w, 400, "base URL must be an HTTP(S) URL without credentials, query or fragment")
		return cloudAgentConfig{}, false
	}
	baseChanged := !sameOMAEndpoint(existing.BaseURL, cfg.BaseURL) &&
		(existing.BaseURL != "" || existing.Token != "" || len(existing.GitHubTokens) > 0)
	if req.MaxConcurrency != 0 {
		cfg.MaxConcurrency = req.MaxConcurrency
	}
	if req.RunTimeoutMinutes != 0 {
		cfg.RunTimeoutMinutes = req.RunTimeoutMinutes
	}
	cfg = cfg.withDefaults(h.omaSpec())
	if cfg.MaxConcurrency < 1 || cfg.MaxConcurrency > claudemanagedruntime.MaxConcurrencyLimit {
		writeError(w, 400, fmt.Sprintf("concurrency must be between 1 and %d", claudemanagedruntime.MaxConcurrencyLimit))
		return cloudAgentConfig{}, false
	}
	if cfg.RunTimeoutMinutes < 5 || cfg.RunTimeoutMinutes > maxOMARunTimeout {
		writeError(w, 400, fmt.Sprintf("run timeout must be between 5 and %d minutes", maxOMARunTimeout))
		return cloudAgentConfig{}, false
	}
	if token := strings.TrimSpace(req.APIToken); token != "" {
		cfg.Token = token
	} else if baseChanged {
		writeError(w, 400, "OMA API key is required when changing the API base URL")
		return cloudAgentConfig{}, false
	}
	if cfg.Token == "" || strings.ContainsAny(cfg.Token, "\r\n\t ") {
		writeError(w, 400, "OMA API key is required")
		return cloudAgentConfig{}, false
	}
	if req.VaultIDs != nil {
		vaults := []string{}
		for _, id := range req.VaultIDs {
			if id = strings.TrimSpace(id); id != "" {
				vaults = append(vaults, id)
			}
		}
		cfg.VaultIDs = vaults
	}
	if baseChanged {
		cfg.GitHubTokens = nil
	}
	if req.GitHubTokens != nil {
		next := map[string]string{}
		for raw, token := range req.GitHubTokens {
			key, err := claudemanagedruntime.RepositoryKey(raw)
			if err != nil {
				writeError(w, 400, "invalid GitHub repository URL")
				return cloudAgentConfig{}, false
			}
			if _, exists := next[key]; exists {
				writeError(w, 400, "duplicate repository binding")
				return cloudAgentConfig{}, false
			}
			if token == "" && !baseChanged {
				token = cfg.GitHubTokens[key]
			}
			if strings.TrimSpace(token) == "" || strings.ContainsAny(strings.TrimSpace(token), "\r\n\t ") {
				writeError(w, 400, "GitHub token is required for each repository")
				return cloudAgentConfig{}, false
			}
			next[key] = strings.TrimSpace(token)
		}
		cfg.GitHubTokens = next
	}
	return cfg, true
}

func sameOMAEndpoint(a, b string) bool {
	return strings.TrimRight(strings.TrimSpace(a), "/") == strings.TrimRight(strings.TrimSpace(b), "/")
}

func (h *Handler) GetOMAConnection(w http.ResponseWriter, r *http.Request) {
	h.getCloudAgentConnection(w, r, h.omaSpec())
}

func (h *Handler) CheckOMAConnection(w http.ResponseWriter, r *http.Request) {
	h.checkCloudAgentConnection(w, r, h.omaSpec())
}

func (h *Handler) SaveOMAConnection(w http.ResponseWriter, r *http.Request) {
	h.saveCloudAgentConnection(w, r, false, h.omaSpec())
}

func (h *Handler) ResumeOMAConnection(w http.ResponseWriter, r *http.Request) {
	h.saveCloudAgentConnection(w, r, true, h.omaSpec())
}

func (h *Handler) StopOMAConnection(w http.ResponseWriter, r *http.Request) {
	h.stopCloudAgentConnection(w, r, h.omaSpec())
}

func (h *Handler) DisconnectOMAConnection(w http.ResponseWriter, r *http.Request) {
	h.disconnectCloudAgentConnection(w, r, h.omaSpec())
}

func (h *Handler) RunOMAConnections(ctx context.Context, pool *pgxpool.Pool, serverURL string) {
	h.RunCloudAgentConnections(ctx, pool, serverURL, h.omaSpec())
}

func (h *Handler) prepareOMAWorkspaceDeletion(ctx context.Context, ws string) error {
	return h.prepareCloudAgentWorkspaceDeletion(ctx, ws, h.omaSpec())
}

func (h *Handler) ListOMAAgents(w http.ResponseWriter, r *http.Request) {
	ws, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace_id")
	if !ok {
		return
	}
	if h.QoderRateLimiter != nil && !h.QoderRateLimiter.Allow(r.Context(), "oma-catalog:"+uuidToString(ws)) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "OMA catalog request limit reached")
		return
	}
	cfg, _, _, _, err := h.readCloudAgentConfig(r.Context(), uuidToString(ws), h.omaSpec())
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 409, "connect OMA Managed Agents first")
		return
	}
	if err != nil {
		writeError(w, 500, "cannot read OMA Managed Agents configuration")
		return
	}
	items, err := listOMACatalog(r.Context(), cfg.BaseURL, cfg.Token, "agents")
	if err != nil {
		writeError(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (h *Handler) ListOMAEnvironments(w http.ResponseWriter, r *http.Request) {
	ws, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace_id")
	if !ok {
		return
	}
	if h.QoderRateLimiter != nil && !h.QoderRateLimiter.Allow(r.Context(), "oma-catalog:"+uuidToString(ws)) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "OMA catalog request limit reached")
		return
	}
	var input struct {
		BaseURL string `json:"base_url"`
		Token   string `json:"api_token"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&input) != nil {
		writeError(w, 400, "invalid OMA environment request")
		return
	}
	cfg, _, _, _, err := h.readCloudAgentConfig(r.Context(), uuidToString(ws), h.omaSpec())
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 500, "cannot read OMA Managed Agents configuration")
		return
	}
	baseURL := strings.TrimSpace(input.BaseURL)
	if baseURL == "" {
		baseURL = cfg.BaseURL
	}
	token := strings.TrimSpace(input.Token)
	if token == "" && strings.TrimSpace(input.BaseURL) != "" && !sameOMAEndpoint(baseURL, cfg.BaseURL) {
		writeError(w, 400, "OMA API key is required when changing the API base URL")
		return
	}
	if token == "" {
		token = cfg.Token
	}
	if baseURL == "" || token == "" {
		writeError(w, 400, "base URL and API key are required")
		return
	}
	items, err := listOMACatalog(r.Context(), baseURL, token, "environments")
	if err != nil {
		writeError(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
