package claudemanagedruntime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUsageDeltaSubtractsContinuedBaseline(t *testing.T) {
	base := sessionUsage{InputTokens: 40, OutputTokens: 10, CacheReadInputTokens: 5}
	base.CacheCreation.Ephemeral5m = 7
	base.ListCost.Amount = "0.05"
	total := sessionUsage{InputTokens: 100, OutputTokens: 30, CacheReadInputTokens: 9}
	total.CacheCreation.Ephemeral5m = 15
	total.CacheCreation.Ephemeral1h = 3
	total.ListCost.Amount = "0.25"
	delta := usageDelta(total, base)
	if delta.InputTokens != 60 || delta.OutputTokens != 20 || delta.CacheReadInputTokens != 4 || delta.cacheWriteTokens() != 11 {
		t.Fatalf("delta = %+v", delta)
	}
	if delta.costTicks() != 20_000_000 {
		t.Fatalf("cost ticks = %d, want 20000000", delta.costTicks())
	}
	empty := usageDelta(base, base)
	if !empty.empty() {
		t.Fatalf("delta at baseline = %+v", empty)
	}
}

func TestReportUsageRetriesTransientPostFailure(t *testing.T) {
	posts := 0
	multica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/tasks/task-1/usage" {
			t.Errorf("unexpected Multica path %s", r.URL.Path)
		}
		posts++
		if posts == 1 {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer multica.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"usage": map[string]any{"input_tokens": 10}})
	}))
	defer provider.Close()
	bridge, err := NewWithProfile(Config{
		MulticaURL:     multica.URL,
		MulticaToken:   "multica-secret",
		WorkspaceID:    "00000000-0000-4000-8000-000000000001",
		AnthropicURL:   provider.URL,
		AnthropicToken: "oma-secret",
		EnvironmentID:  "env_1",
		StateDir:       t.TempDir(),
	}, OMAProfile(), nil)
	if err != nil {
		t.Fatal(err)
	}
	bridge.state = &journal{Binding: "test"}
	taskRun := &run{TaskID: "task-1", SessionID: "sess_1"}
	if err := bridge.reportUsage(context.Background(), taskRun); err == nil {
		t.Fatal("transient POST failure was treated as reported")
	}
	if taskRun.UsageReported {
		t.Fatal("transient POST failure marked usage reported")
	}
	if err := bridge.reportUsage(context.Background(), taskRun); err != nil {
		t.Fatal(err)
	}
	if !taskRun.UsageReported || posts != 2 {
		t.Fatalf("usage was not retried: reported=%v posts=%d", taskRun.UsageReported, posts)
	}
}

func TestCreateSessionResetsUsageBaseline(t *testing.T) {
	h := setupOMA(t)
	h.b.state = &journal{Binding: "test", RuntimeID: "runtime-1"}
	taskRun := &run{
		TaskID:        "task-1",
		AgentID:       "agent_1",
		Prompt:        "do work",
		UsageBase:     sessionUsage{InputTokens: 40},
		UsageReported: true,
	}
	if err := h.b.createSession(context.Background(), taskRun, nil, "fingerprint"); err != nil {
		t.Fatal(err)
	}
	if taskRun.UsageBase != (sessionUsage{}) || taskRun.UsageReported {
		t.Fatalf("fresh session kept stale usage baseline: %+v reported=%v", taskRun.UsageBase, taskRun.UsageReported)
	}
}

func TestCancelledTaskReportsUsageBeforeRemoval(t *testing.T) {
	var usagePosts, cancelAcks int
	multica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			json.NewEncoder(w).Encode(map[string]string{"status": "cancelled"})
		case strings.HasSuffix(r.URL.Path, "/usage"):
			usagePosts++
			json.NewEncoder(w).Encode(map[string]any{})
		case strings.HasSuffix(r.URL.Path, "/cancel-ack"):
			cancelAcks++
			json.NewEncoder(w).Encode(map[string]any{})
		default:
			t.Errorf("unexpected Multica path %s", r.URL.Path)
		}
	}))
	defer multica.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/sess_1" {
			t.Errorf("unexpected provider path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status": "idle",
			"usage":  map[string]any{"input_tokens": 10, "output_tokens": 5},
		})
	}))
	defer provider.Close()
	bridge, err := NewWithProfile(Config{
		MulticaURL:     multica.URL,
		MulticaToken:   "multica-secret",
		WorkspaceID:    "00000000-0000-4000-8000-000000000001",
		AnthropicURL:   provider.URL,
		AnthropicToken: "oma-secret",
		EnvironmentID:  "env_1",
		StateDir:       t.TempDir(),
	}, OMAProfile(), nil)
	if err != nil {
		t.Fatal(err)
	}
	bridge.state = &journal{Binding: "test", RuntimeID: "runtime-1", Runs: []*run{{TaskID: "task-1", SessionID: "sess_1", Phase: "running"}}}
	if err := bridge.stepRun(context.Background(), bridge.state.first()); err != nil {
		t.Fatal(err)
	}
	if usagePosts != 1 || cancelAcks != 1 || bridge.state.first() != nil {
		t.Fatalf("cancelled task cleanup: usage=%d acks=%d runs=%d", usagePosts, cancelAcks, len(bridge.state.Runs))
	}
}

func TestReportUsagePostsSessionDelta(t *testing.T) {
	var received map[string]any
	multica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/tasks/task-1/usage" {
			t.Errorf("unexpected Multica path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer multica.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/sess_1" {
			t.Errorf("unexpected provider path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]any{
				"input_tokens":            100,
				"output_tokens":           30,
				"cache_read_input_tokens": 9,
				"cache_creation": map[string]any{
					"ephemeral_5m_input_tokens": 15,
					"ephemeral_1h_input_tokens": 3,
				},
				"list_cost": map[string]any{"amount": "0.25", "currency": "USD"},
			},
		})
	}))
	defer provider.Close()
	bridge, err := NewWithProfile(Config{
		MulticaURL:     multica.URL,
		MulticaToken:   "multica-secret",
		WorkspaceID:    "00000000-0000-4000-8000-000000000001",
		AnthropicURL:   provider.URL,
		AnthropicToken: "oma-secret",
		EnvironmentID:  "env_1",
		StateDir:       t.TempDir(),
	}, OMAProfile(), nil)
	if err != nil {
		t.Fatal(err)
	}
	bridge.state = &journal{Binding: "test"}
	taskRun := &run{TaskID: "task-1", SessionID: "sess_1", UsageBase: sessionUsage{InputTokens: 40, OutputTokens: 10}}
	taskRun.UsageBase.CacheCreation.Ephemeral5m = 7
	taskRun.UsageBase.ListCost.Amount = "0.05"
	if err := bridge.reportUsage(context.Background(), taskRun); err != nil {
		t.Fatal(err)
	}
	if !taskRun.UsageReported {
		t.Fatal("usage not marked reported")
	}
	usage, _ := received["usage"].([]any)
	if len(usage) != 1 {
		t.Fatalf("usage payload = %#v", received)
	}
	item := usage[0].(map[string]any)
	if item["provider"] != OMAProfile().Provider || item["input_tokens"] != float64(60) || item["output_tokens"] != float64(20) || item["cost_usd_ticks"] != float64(20_000_000) {
		t.Fatalf("usage item = %#v", item)
	}
}
