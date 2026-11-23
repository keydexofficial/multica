package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func cleanupOMAConnection(t *testing.T) {
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM daemon_token WHERE id IN (SELECT token_id FROM cloud_agent_connection WHERE provider='oma_cloud' AND workspace_id=$1)`, testWorkspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM cloud_agent_connection WHERE provider='oma_cloud' AND workspace_id=$1`, testWorkspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE provider='oma_cloud' AND workspace_id=$1`, testWorkspaceID)
	})
}

func TestOMAConnectionSecrets(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withVCSBox(t)
	cleanupOMAConnection(t)
	body := map[string]any{
		"name":           "OMA",
		"base_url":       "http://127.0.0.1:38080",
		"environment_id": "env_test",
		"api_token":      "oma-test-secret",
		"vault_ids":      []string{"vlt_test"},
		"github_tokens":  map[string]string{"https://github.com/owner/repo": "github-test-secret"},
	}
	var view cloudAgentView
	testutil.Call(t, testHandler.SaveOMAConnection, vcsHandlerRequest("PUT", "/oma", body, "")).Want(200).JSON(&view)
	if !view.Configured || !view.HasToken || view.BaseURL != "http://127.0.0.1:38080" || len(view.Repositories) != 1 || strings.Join(view.VaultIDs, ",") != "vlt_test" {
		t.Fatalf("unexpected view: %+v", view)
	}
	var encrypted string
	dbfx.QueryRow(t, `SELECT config_encrypted FROM cloud_agent_connection WHERE provider='oma_cloud' AND workspace_id=$1`, testWorkspaceID).Scan(&encrypted)
	raw, _ := json.Marshal(view)
	for _, secret := range []string{"oma-test-secret", "github-test-secret"} {
		if strings.Contains(encrypted, secret) || strings.Contains(string(raw), secret) {
			t.Fatal("plaintext credential exposed")
		}
	}

	body["api_token"] = ""
	body["github_tokens"] = map[string]string{"https://github.com/owner/repo": ""}
	testutil.Call(t, testHandler.SaveOMAConnection, vcsHandlerRequest("PUT", "/oma", body, "")).Want(200)
	cfg, _, _, _, err := testHandler.readCloudAgentConfig(context.Background(), testWorkspaceID, testHandler.omaSpec())
	if err != nil || cfg.Token != "oma-test-secret" || cfg.GitHubTokens["https://github.com/owner/repo"] != "github-test-secret" || cfg.BaseURL != "http://127.0.0.1:38080" {
		t.Fatalf("blank field did not retain credentials: %+v %v", cfg, err)
	}
	var daemonID string
	dbfx.QueryRow(t, `SELECT daemon_id FROM daemon_token WHERE token_hash=$1`, auth.HashToken(cfg.MulticaToken)).Scan(&daemonID)
	if daemonID != "oma-managed-"+testWorkspaceID {
		t.Fatalf("service credential daemon_id = %q", daemonID)
	}

	for name, bad := range map[string]map[string]any{
		"missing environment": {"name": "OMA", "base_url": "http://127.0.0.1:38080", "environment_id": "", "api_token": "k"},
		"foreign environment": {"name": "OMA", "base_url": "http://127.0.0.1:38080", "environment_id": "qca_env", "api_token": "k"},
		"non-HTTP base URL":   {"name": "OMA", "base_url": "ftp://127.0.0.1", "environment_id": "env_test", "api_token": "k"},
		"credential base URL": {"name": "OMA", "base_url": "http://user:pass@127.0.0.1", "environment_id": "env_test", "api_token": "k"},
		"query base URL":      {"name": "OMA", "base_url": "http://127.0.0.1?x=1", "environment_id": "env_test", "api_token": "k"},
		"concurrency":         {"name": "OMA", "base_url": "http://127.0.0.1:38080", "environment_id": "env_test", "api_token": "k", "max_concurrency": 99},
		"unknown field":       {"name": "OMA", "base_url": "http://127.0.0.1:38080", "environment_id": "env_test", "anthropic_token": "k"},
	} {
		t.Run(name, func(t *testing.T) {
			testutil.Call(t, testHandler.SaveOMAConnection, vcsHandlerRequest("PUT", "/oma", bad, "")).Want(400)
		})
	}
}

func TestOMACheckAndCatalog(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withVCSBox(t)
	cleanupOMAConnection(t)
	oma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer oma-test-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/v1/environments/env_check":
			if r.URL.Query().Get("beta") != "true" {
				t.Error("environment check omitted beta=true")
			}
			json.NewEncoder(w).Encode(map[string]string{"id": "env_check"})
		case r.URL.Path == "/v1/vaults/vlt_check":
			if r.URL.Query().Get("beta") != "true" {
				t.Error("vault check omitted beta=true")
			}
			json.NewEncoder(w).Encode(map[string]string{"id": "vlt_check"})
		case r.URL.Path == "/v1/environments":
			if r.URL.Query().Get("beta") != "true" {
				t.Error("environment catalog omitted beta=true")
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data":      []map[string]string{{"id": "env_check", "name": "Check environment"}},
				"next_page": "",
			})
		case r.URL.Path == "/v1/agents":
			json.NewEncoder(w).Encode(map[string]any{
				"data":      []map[string]string{{"id": "agent_check", "name": "Checker"}},
				"next_page": "",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(oma.Close)
	input := map[string]any{
		"name":           "OMA",
		"base_url":       oma.URL + "/v1",
		"environment_id": "env_check",
		"api_token":      "oma-test-secret",
		"vault_ids":      []string{"vlt_check"},
	}
	testutil.Call(t, testHandler.CheckOMAConnection, vcsHandlerRequest("POST", "/oma/check", input, "")).Want(200)
	testutil.Call(t, testHandler.SaveOMAConnection, vcsHandlerRequest("PUT", "/oma", input, "")).Want(200)
	testutil.Call(t, testHandler.ListOMAEnvironments, vcsHandlerRequest("POST", "/oma/environments", map[string]string{"base_url": "http://other-oma:38080/v1"}, "")).Want(400)

	var environments struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	testutil.Call(t, testHandler.ListOMAEnvironments, vcsHandlerRequest("POST", "/oma/environments", map[string]string{"base_url": oma.URL + "/v1", "api_token": "oma-test-secret"}, "")).Want(200).JSON(&environments)
	if len(environments.Items) != 1 || environments.Items[0].ID != "env_check" {
		t.Fatalf("environments = %+v", environments.Items)
	}
	var agents struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	testutil.Call(t, testHandler.ListOMAAgents, vcsHandlerRequest("GET", "/oma/agents", nil, "")).Want(200).JSON(&agents)
	if len(agents.Items) != 1 || agents.Items[0].ID != "agent_check" {
		t.Fatalf("agents = %+v", agents.Items)
	}
}

func TestOMADisconnectRemovesConnectionAndUnbindsAgents(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withVCSBox(t)
	cleanupOMAConnection(t)
	input := map[string]any{"name": "OMA", "base_url": "http://oma.internal:38080/v1", "environment_id": "env_test", "api_token": "fixture-token"}
	testutil.Call(t, testHandler.SaveOMAConnection, vcsHandlerRequest("PUT", "/oma", input, "")).Want(200)
	runtimeID := dbfx.Runtime(t, "OMA", testutil.Cols{"provider": "oma_cloud", "runtime_mode": "cloud", "daemon_id": "oma-managed-" + testWorkspaceID})
	agentID := dbfx.Agent(t, "OMA agent", runtimeID)

	deleteReq := newRequest("DELETE", "/api/runtimes/"+runtimeID, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("runtimeId", runtimeID)
	deleteReq = deleteReq.WithContext(context.WithValue(deleteReq.Context(), chi.RouteCtxKey, rctx))
	var managed map[string]any
	testutil.Call(t, testHandler.DeleteAgentRuntime, deleteReq).Want(409).JSON(&managed)
	if managed["code"] != "runtime_managed_by_connection" {
		t.Fatalf("generic delete of an OMA runtime: %v", managed)
	}

	var changed map[string]any
	testutil.Call(t, testHandler.DisconnectOMAConnection, vcsHandlerRequest("DELETE", "/oma", map[string]any{"expected_active_agent_ids": []string{}}, "")).Want(409).JSON(&changed)
	if changed["code"] != "runtime_delete_plan_changed" {
		t.Fatalf("unconfirmed agents should refuse with the latest plan: %v", changed)
	}
	testutil.Call(t, testHandler.DisconnectOMAConnection, vcsHandlerRequest("DELETE", "/oma", map[string]any{"expected_active_agent_ids": []string{"not-a-uuid"}}, "")).Want(400)

	body := map[string]any{"expected_active_agent_ids": []string{agentID}}
	testutil.Call(t, testHandler.DisconnectOMAConnection, vcsHandlerRequest("DELETE", "/oma", body, "")).Want(200)

	var connections, runtimes, agents int
	var bound bool
	dbfx.QueryRow(t, `SELECT count(*) FROM cloud_agent_connection WHERE provider='oma_cloud' AND workspace_id=$1`, testWorkspaceID).Scan(&connections)
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_runtime WHERE id=$1`, runtimeID).Scan(&runtimes)
	dbfx.QueryRow(t, `SELECT count(*), bool_or(runtime_id IS NOT NULL) FROM agent WHERE id=$1`, agentID).Scan(&agents, &bound)
	if connections != 0 || runtimes != 0 || agents != 1 || bound {
		t.Fatalf("disconnect: connections=%d runtimes=%d agents=%d bound=%v", connections, runtimes, agents, bound)
	}
	testutil.Call(t, testHandler.DisconnectOMAConnection, vcsHandlerRequest("DELETE", "/oma", body, "")).Want(404)
}

func TestOMAManagedRegistrationAndRevocation(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withVCSBox(t)
	cleanupOMAConnection(t)
	input := map[string]any{"name": "OMA", "base_url": "http://127.0.0.1:38080", "environment_id": "env_test", "api_token": "fixture-token"}
	testutil.Call(t, testHandler.SaveOMAConnection, vcsHandlerRequest("PUT", "/oma", input, "")).Want(200)
	cfg, _, _, _, err := testHandler.readCloudAgentConfig(context.Background(), testWorkspaceID, testHandler.omaSpec())
	if err != nil {
		t.Fatal(err)
	}
	daemonID := "oma-managed-" + testWorkspaceID
	register := func(provider string) *testutil.Response {
		req := newDaemonTokenRequest("POST", "/api/daemon/register", map[string]any{"workspace_id": testWorkspaceID, "daemon_id": daemonID, "runtimes": []map[string]any{{"name": "OMA", "type": provider, "runtime_mode": "cloud", "status": "online"}}}, testWorkspaceID, daemonID)
		req.Header.Set("Authorization", "Bearer "+cfg.MulticaToken)
		return testutil.Call(t, testHandler.DaemonRegister, req)
	}
	register("qoder_cloud").Want(403)
	register("claude_managed").Want(403)
	register("oma_cloud").Want(200)

	testutil.Call(t, testHandler.StopOMAConnection, vcsHandlerRequest("POST", "/oma/stop", nil, "")).Want(200)
	req := newDaemonTokenRequest("POST", "/api/daemon/register", nil, testWorkspaceID, daemonID)
	req.Header.Set("Authorization", "Bearer "+cfg.MulticaToken)
	if allowed, err := testHandler.managedCredentialAllowed(req, testWorkspaceID, daemonID); err != nil || allowed {
		t.Fatalf("stopped credential accepted: %v %v", allowed, err)
	}
}

func TestOMAConfigurationRetainsPendingCleanup(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, queue := range []string{"unresolved", "cleanup"} {
		for _, action := range []string{"save", "resume"} {
			t.Run(queue+"/"+action, func(t *testing.T) {
				withVCSBox(t)
				cleanupOMAConnection(t)
				input := map[string]any{"name": "OMA", "base_url": "http://oma.internal:38080/v1", "environment_id": "env_test", "api_token": "fixture-token"}
				testutil.Call(t, testHandler.SaveOMAConnection, vcsHandlerRequest("PUT", "/oma", input, "")).Want(http.StatusOK)
				state := map[string]any{"runtime_id": "existing-runtime"}
				if queue == "unresolved" {
					state[queue] = []map[string]any{{"task_id": "11111111-1111-4111-8111-111111111111", "agent_id": "agent_1", "create_attempt_at": time.Now().UTC()}}
				} else {
					state[queue] = []string{"sess_1"}
				}
				raw, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				sealed, err := testHandler.sealVCSSecret(string(raw))
				if err != nil {
					t.Fatal(err)
				}
				dbfx.Exec(t, `UPDATE cloud_agent_connection SET journal_encrypted=$2,enabled=false WHERE provider='oma_cloud' AND workspace_id=$1`, testWorkspaceID, sealed)
				call := func() *testutil.Response {
					if action == "resume" {
						return testutil.Call(t, testHandler.ResumeOMAConnection, vcsHandlerRequest("POST", "/oma/resume", input, ""))
					}
					return testutil.Call(t, testHandler.SaveOMAConnection, vcsHandlerRequest("PUT", "/oma", input, ""))
				}
				input["environment_id"] = "env_other"
				call().Want(http.StatusConflict)
				input["environment_id"] = "env_test"
				input["api_token"] = "replacement-token"
				input["name"] = "Renamed OMA"
				call().Want(http.StatusOK)
				var remaining string
				dbfx.QueryRow(t, `SELECT journal_encrypted FROM cloud_agent_connection WHERE provider='oma_cloud' AND workspace_id=$1`, testWorkspaceID).Scan(&remaining)
				if remaining != sealed {
					t.Fatal("configuration update changed the pending cleanup journal")
				}
				cfg, enabled, _, _, err := testHandler.readCloudAgentConfig(context.Background(), testWorkspaceID, testHandler.omaSpec())
				if err != nil || cfg.Token != "replacement-token" || cfg.Name != "Renamed OMA" || enabled != (action == "resume") {
					t.Fatalf("configuration did not recover: enabled=%v err=%v", enabled, err)
				}
			})
		}
	}
}

func TestInactiveOMACleanupRecoversAfterCredentialReplacement(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			withVCSBox(t)
			cleanupOMAConnection(t)
			stopped := false
			oma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer replacement-token" {
					w.WriteHeader(status)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/v1/sessions" && r.Method == http.MethodGet:
					json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "sess_1", "agent": map[string]string{"id": "agent_1"}, "created_at": time.Now().UTC().Format(time.RFC3339), "metadata": map[string]string{"multica_task_id": "11111111-1111-4111-8111-111111111111", "multica_workspace_id": testWorkspaceID}}}})
				case r.URL.Path == "/v1/sessions/sess_1" && r.Method == http.MethodGet:
					remoteStatus := "running"
					if stopped {
						remoteStatus = "idle"
					}
					json.NewEncoder(w).Encode(map[string]string{"status": remoteStatus})
				case r.URL.Path == "/v1/sessions/sess_1/events" && r.Method == http.MethodPost:
					stopped = true
					json.NewEncoder(w).Encode(map[string]any{})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(oma.Close)
			input := map[string]any{"name": "OMA", "base_url": oma.URL + "/v1", "environment_id": "env_test", "api_token": "revoked-token"}
			testutil.Call(t, testHandler.SaveOMAConnection, vcsHandlerRequest("PUT", "/oma", input, "")).Want(http.StatusOK)
			created := time.Now().UTC()
			raw, err := json.Marshal(map[string]any{"unresolved": []map[string]any{{"task_id": "11111111-1111-4111-8111-111111111111", "agent_id": "agent_1", "create_attempt_at": created.Add(-time.Minute)}}})
			if err != nil {
				t.Fatal(err)
			}
			sealed, err := testHandler.sealVCSSecret(string(raw))
			if err != nil {
				t.Fatal(err)
			}
			dbfx.Exec(t, `UPDATE cloud_agent_connection SET journal_encrypted=$2,enabled=false WHERE provider='oma_cloud' AND workspace_id=$1`, testWorkspaceID, sealed)
			testHandler.cleanupInactiveCloudAgent(context.Background(), testHandler.omaSpec())
			var remaining string
			var view map[string]any
			testutil.Call(t, testHandler.GetOMAConnection, vcsHandlerRequest("GET", "/oma", nil, "")).Want(http.StatusOK).JSON(&view)
			dbfx.QueryRow(t, `SELECT journal_encrypted FROM cloud_agent_connection WHERE provider='oma_cloud' AND workspace_id=$1`, testWorkspaceID).Scan(&remaining)
			lastError, _ := view["last_error"].(string)
			if remaining != sealed || stopped || view["status"] != "error" || !strings.Contains(lastError, "API key") {
				t.Fatalf("authorization failure was not retained and exposed: stopped=%v status=%v error=%s", stopped, view["status"], lastError)
			}
			input["api_token"] = "replacement-token"
			testutil.Call(t, testHandler.SaveOMAConnection, vcsHandlerRequest("PUT", "/oma", input, "")).Want(http.StatusOK)
			testHandler.cleanupInactiveCloudAgent(context.Background(), testHandler.omaSpec())
			dbfx.QueryRow(t, `SELECT journal_encrypted FROM cloud_agent_connection WHERE provider='oma_cloud' AND workspace_id=$1`, testWorkspaceID).Scan(&remaining)
			testutil.Call(t, testHandler.GetOMAConnection, vcsHandlerRequest("GET", "/oma", nil, "")).Want(http.StatusOK).JSON(&view)
			if remaining != "" || !stopped || view["status"] != "stopped" || view["last_error"] != "" {
				t.Fatalf("cleanup did not recover after credential replacement: stopped=%v status=%v error=%v", stopped, view["status"], view["last_error"])
			}
		})
	}
}

func TestOMAWorkspaceDeletionCleansUpRemoteSessions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withVCSBox(t)
	cleanupOMAConnection(t)
	stopped := false
	oma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/sessions/sess_1" && r.Method == http.MethodGet:
			remoteStatus := "running"
			if stopped {
				remoteStatus = "idle"
			}
			json.NewEncoder(w).Encode(map[string]string{"status": remoteStatus})
		case r.URL.Path == "/v1/sessions/sess_1/events" && r.Method == http.MethodPost:
			stopped = true
			json.NewEncoder(w).Encode(map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(oma.Close)
	input := map[string]any{"name": "OMA", "base_url": oma.URL + "/v1", "environment_id": "env_test", "api_token": "fixture-token"}
	testutil.Call(t, testHandler.SaveOMAConnection, vcsHandlerRequest("PUT", "/oma", input, "")).Want(http.StatusOK)
	raw, err := json.Marshal(map[string]any{"cleanup": []string{"sess_1"}})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := testHandler.sealVCSSecret(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE cloud_agent_connection SET journal_encrypted=$2 WHERE provider='oma_cloud' AND workspace_id=$1`, testWorkspaceID, sealed)
	if err := testHandler.prepareOMAWorkspaceDeletion(context.Background(), testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	var remaining string
	var enabled bool
	dbfx.QueryRow(t, `SELECT journal_encrypted,enabled FROM cloud_agent_connection WHERE provider='oma_cloud' AND workspace_id=$1`, testWorkspaceID).Scan(&remaining, &enabled)
	if remaining != "" || enabled || !stopped {
		t.Fatalf("workspace deletion left cleanup pending: journal=%d enabled=%v stopped=%v", len(remaining), enabled, stopped)
	}
}

func TestOMASupervisorStartsBridge(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	withVCSBox(t)
	cleanupOMAConnection(t)
	oma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/environments/env_test" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("beta") != "true" {
			t.Error("supervisor check omitted beta=true")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": "env_test"})
	}))
	t.Cleanup(oma.Close)
	registered := make(chan string, 1)
	multica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/workspaces/"+testWorkspaceID:
			json.NewEncoder(w).Encode(map[string]string{"id": testWorkspaceID})
		case r.URL.Path == "/api/daemon/register":
			var req struct {
				Runtimes []struct {
					Type string `json:"type"`
					Mode string `json:"runtime_mode"`
				} `json:"runtimes"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			if len(req.Runtimes) != 1 || req.Runtimes[0].Type != "oma_cloud" || req.Runtimes[0].Mode != "cloud" {
				t.Errorf("registration = %+v", req.Runtimes)
			}
			select {
			case registered <- req.Runtimes[0].Type:
			default:
			}
			json.NewEncoder(w).Encode(map[string]any{"runtimes": []map[string]string{{"id": "22222222-2222-4222-8222-222222222222", "provider": "oma_cloud"}}})
		case r.URL.Path == "/api/daemon/heartbeat":
			json.NewEncoder(w).Encode(map[string]any{})
		case strings.HasSuffix(r.URL.Path, "/tasks/claim"):
			json.NewEncoder(w).Encode(map[string]any{"task": nil})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(multica.Close)
	input := map[string]any{"name": "OMA", "base_url": oma.URL + "/v1", "environment_id": "env_test", "api_token": "fixture-token"}
	testutil.Call(t, testHandler.SaveOMAConnection, vcsHandlerRequest("PUT", "/oma", input, "")).Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE cloud_agent_connection SET enabled=true WHERE provider='oma_cloud' AND workspace_id=$1`, testWorkspaceID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		testHandler.RunOMAConnections(ctx, testPool, multica.URL)
		close(done)
	}()
	deadline := time.Now().Add(15 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		if err := testPool.QueryRow(context.Background(), `SELECT status FROM cloud_agent_connection WHERE provider='oma_cloud' AND workspace_id=$1`, testWorkspaceID).Scan(&status); err == nil && status == "online" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	select {
	case provider := <-registered:
		if provider != "oma_cloud" {
			t.Fatalf("registered provider = %s", provider)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OMA bridge did not register")
	}
	if status != "online" {
		t.Fatalf("supervisor did not publish the connection: status=%s", status)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("OMA supervisor did not stop")
	}
}
