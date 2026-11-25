package claudemanagedruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/multica-ai/multica/server/internal/cloudagent"
	"github.com/multica-ai/multica/server/internal/daemon"
)

const workspaceID = "11111111-1111-4111-8111-111111111111"

type harness struct {
	b                       *Bridge
	status                  string
	task                    *daemon.Task
	events                  []map[string]any
	creates, sends, cancels int
	lostSend                bool
	complete, failure       map[string]any
	messages                []map[string]any
	prompt                  string
	sessionAgent            string
	initialEvents           int
	priorMetadata           map[string]string
	// lostCreates answers that many session creates with a gateway timeout;
	// lostCreatePersists makes those requests still create the session.
	lostCreates        int
	lostCreatePersists bool
	remote             []map[string]any
	listQueries        []url.Values
	// foreignSession is another task's session that every list returns.
	foreignSession map[string]any
	// listStatus answers session listings with this HTTP status when set.
	listStatus int
	// endlessList makes every listing page point to a further page.
	endlessList bool
	// initialEcho replaces the prompt text in the event history of a new session.
	initialEcho string
	// remoteStatus is sess_1's status; empty reads as idle.
	remoteStatus string
	// history, when set, replaces the session's event history whenever a
	// prompt is submitted, e.g. with a recorded one.
	history func(prompt string) []map[string]any
	// vaultIDs is the vault_ids of the last session create; nil if absent.
	vaultIDs      []string
	profile       Profile
	runtimeConfig string

	// Chat attachment and file-delivery observations.
	uploadedFiles    []string
	uploadedTypes    []string
	initialContent   []map[string]any
	sendContent      []map[string]any
	initialResources []map[string]any
	mountedFiles     []map[string]any
	uploads          int
	multicaUploads   []string
	sessionResources []map[string]any
	fileContent      map[string]string
	fileScope        string

	// Issue attachments returned by the issue endpoint.
	issueAttachments []map[string]any
	// Custom tool results and resource credential updates sent to the provider.
	customToolResults []map[string]any
	resourceUpdates   []map[string]any
	// Quick-create and squad-leader writes to Multica.
	createdIssues      []map[string]any
	issueComments      []map[string]any
	squadEvaluations   []map[string]any
	createdIssueStatus int
}

func setup(t *testing.T) *harness {
	return setupWith(t, ClaudeProfile(), "claude_agent_id")
}

func setupOMA(t *testing.T) *harness {
	return setupWith(t, OMAProfile(), "oma_agent_id")
}

func setupWith(t *testing.T, profile Profile, runtimeConfigKey string) *harness {
	t.Helper()
	h := &harness{status: "dispatched", profile: profile, runtimeConfig: runtimeConfigKey, foreignSession: map[string]any{"id": "sess_other", "agent": map[string]string{"id": "agent_1"}, "created_at": time.Now().UTC().Format(time.RFC3339), "metadata": map[string]string{"multica_task_id": "task-other", "multica_workspace_id": workspaceID}}}
	h.task = &daemon.Task{ID: "task-1", RuntimeID: "runtime-1", WorkspaceID: workspaceID, IssueID: "issue-1", AuthToken: "mat_task", Agent: &daemon.AgentData{Instructions: "Run tests", RuntimeConfig: json.RawMessage(fmt.Sprintf(`{"%s":"agent_1"}`, runtimeConfigKey)), Skills: []daemon.SkillData{{Name: "multica-platform"}}}}
	m := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A storage download URL is pre-signed and carries no Multica headers.
		if !strings.HasPrefix(r.URL.Path, "/download/") {
			if r.Header.Get("X-Workspace-ID") != workspaceID {
				t.Error("missing workspace")
			}
			auth := "Bearer multica-secret"
			if r.URL.Path == "/api/issues" || strings.HasPrefix(r.URL.Path, "/api/issues/") ||
				strings.HasPrefix(r.URL.Path, "/api/attachments/") ||
				strings.HasSuffix(r.URL.Path, "/upload-file") {
				auth = "Bearer mat_task"
			}
			if r.Header.Get("Authorization") != auth {
				t.Error("wrong credential scope")
			}
		}
		if r.Header.Get("X-Client-Capabilities") != "" {
			t.Error("unsupported capabilities advertised")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/tasks/claim"):
			json.NewEncoder(w).Encode(map[string]any{"task": h.task})
			h.task = nil
		case r.URL.Path == "/api/issues" && r.Method == http.MethodPost:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			h.createdIssues = append(h.createdIssues, body)
			status := h.createdIssueStatus
			if status == 0 {
				status = http.StatusCreated
			}
			if status >= 400 {
				w.WriteHeader(status)
				io.WriteString(w, `{"code":"active_duplicate_issue","error":"duplicate"}`)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"identifier": "MUL-42"})
		case strings.HasSuffix(r.URL.Path, "/comments"):
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			h.issueComments = append(h.issueComments, body)
			json.NewEncoder(w).Encode(map[string]any{"trigger_outcomes": []map[string]any{}})
		case strings.HasSuffix(r.URL.Path, "/squad-evaluated"):
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			h.squadEvaluations = append(h.squadEvaluations, body)
			io.WriteString(w, `{}`)
		case strings.HasPrefix(r.URL.Path, "/api/issues/"):
			json.NewEncoder(w).Encode(map[string]any{"title": "Fix bug", "description": "Use repository context", "attachments": h.issueAttachments})
		case strings.HasPrefix(r.URL.Path, "/api/attachments/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/attachments/")
			filename, contentType := "notes.txt", "text/plain"
			if id == "img-1" {
				filename, contentType = "shot.png", "image/png"
			}
			json.NewEncoder(w).Encode(map[string]any{
				"id": id, "filename": filename, "content_type": contentType,
				"size_bytes": 5, "download_url": "http://" + r.Host + "/download/" + id,
			})
		case strings.HasPrefix(r.URL.Path, "/download/"):
			io.WriteString(w, "hello")
		case strings.HasSuffix(r.URL.Path, "/upload-file"):
			h.uploads++
			if err := r.ParseMultipartForm(1 << 20); err == nil {
				if part, header, err := r.FormFile("file"); err == nil {
					io.Copy(io.Discard, part)
					part.Close()
					h.multicaUploads = append(h.multicaUploads, header.Filename)
				}
			}
			io.WriteString(w, `{"id":"att_1"}`)
		case strings.HasSuffix(r.URL.Path, "/status"):
			json.NewEncoder(w).Encode(map[string]string{"status": h.status})
		case strings.HasSuffix(r.URL.Path, "/start"):
			h.status = "running"
			io.WriteString(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/complete"):
			json.NewDecoder(r.Body).Decode(&h.complete)
			h.status = "completed"
			io.WriteString(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/fail"):
			json.NewDecoder(r.Body).Decode(&h.failure)
			h.status = "failed"
			io.WriteString(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/messages"):
			var req struct {
				Messages []map[string]any `json:"messages"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			h.messages = append(h.messages, req.Messages...)
			io.WriteString(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/session"), strings.HasSuffix(r.URL.Path, "/cancel-ack"):
			io.WriteString(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/usage"):
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected Multica request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.Close)
	q := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer anthropic-secret" {
			t.Error("wrong Anthropic credential")
		}
		switch {
		case r.URL.Path == "/sessions" && r.Method == "POST":
			h.creates++
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			var input struct {
				Agent         string            `json:"agent"`
				Metadata      map[string]string `json:"metadata"`
				InitialEvents []event           `json:"initial_events"`
				Resources     []map[string]any  `json:"resources"`
				VaultIDs      []string          `json:"vault_ids"`
			}
			if err := json.Unmarshal(body, &input); err != nil {
				t.Fatal(err)
			}
			h.initialResources = input.Resources
			var raw struct {
				InitialEvents []struct {
					Content []map[string]any `json:"content"`
				} `json:"initial_events"`
			}
			_ = json.Unmarshal(body, &raw)
			if len(raw.InitialEvents) > 0 {
				h.initialContent = raw.InitialEvents[0].Content
			}
			lost := h.lostCreates > 0
			if lost {
				h.lostCreates--
				if !h.lostCreatePersists {
					http.Error(w, "upstream timeout", http.StatusGatewayTimeout)
					return
				}
			}
			h.sessionAgent = input.Agent
			h.vaultIDs = input.VaultIDs
			h.initialEvents = len(input.InitialEvents)
			if len(input.InitialEvents) == 1 && input.InitialEvents[0].Type == "user.message" {
				h.prompt = input.InitialEvents[0].text()
				echo := h.prompt
				if h.initialEcho != "" {
					echo = h.initialEcho
				}
				if h.history != nil {
					h.events = h.history(echo)
				} else {
					h.events = append(h.events, map[string]any{"id": "evt_user", "type": "user.message", "content": []map[string]string{{"type": "text", "text": echo}}})
				}
			}
			h.remote = append(h.remote, map[string]any{"id": "sess_1", "agent": map[string]string{"id": input.Agent}, "created_at": time.Now().UTC().Format(time.RFC3339), "metadata": input.Metadata})
			h.remoteStatus = "running"
			if lost {
				http.Error(w, "upstream timeout", http.StatusGatewayTimeout)
				return
			}
			session := fixture(t, "session_create.json")
			session["agent"].(map[string]any)["id"] = input.Agent
			session["metadata"] = input.Metadata
			json.NewEncoder(w).Encode(session)
		case r.URL.Path == "/sessions" && r.Method == "GET":
			query := r.URL.Query()
			h.listQueries = append(h.listQueries, query)
			if h.listStatus != 0 {
				w.WriteHeader(h.listStatus)
				return
			}
			from, err := time.Parse(time.RFC3339, query.Get("created_at[gte]"))
			if err != nil {
				t.Errorf("list without created_at[gte]: %v", err)
			}
			data := []map[string]any{h.foreignSession}
			for _, s := range h.remote {
				created, _ := time.Parse(time.RFC3339, s["created_at"].(string))
				if s["agent"].(map[string]string)["id"] == query.Get("agent_id") && !created.Before(from) {
					data = append(data, s)
				}
			}
			if h.endlessList {
				// Only foreign sessions, always with another page.
				json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{h.foreignSession}, "next_page": fmt.Sprintf("page_%d", len(h.listQueries))})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data, "next_page": nil})
		case r.URL.Path == "/sessions/sess_1/events" && r.Method == "POST":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			var req struct {
				Events []event `json:"events"`
			}
			json.Unmarshal(body, &req)
			var raw struct {
				Events []struct {
					Content []map[string]any `json:"content"`
				} `json:"events"`
			}
			json.Unmarshal(body, &raw)
			if len(raw.Events) > 0 {
				h.sendContent = raw.Events[0].Content
			}
			if len(req.Events) == 1 && req.Events[0].Type == "user.interrupt" {
				h.cancels++
				h.remoteStatus = "idle"
				json.NewEncoder(w).Encode(fixture(t, "events_interrupt.json"))
				return
			}
			if len(req.Events) == 1 && req.Events[0].Type == "user.custom_tool_result" {
				var raw struct {
					Events []map[string]any `json:"events"`
				}
				json.Unmarshal(body, &raw)
				h.customToolResults = append(h.customToolResults, raw.Events...)
				json.NewEncoder(w).Encode(map[string]any{})
				return
			}
			h.sends++
			h.remoteStatus = "running"
			h.prompt = req.Events[0].text()
			if h.history != nil {
				h.events = h.history(h.prompt)
			} else {
				h.events = append(h.events, map[string]any{"id": "evt_user", "type": "user.message", "content": []map[string]string{{"type": "text", "text": h.prompt}}})
			}
			if h.lostSend {
				http.Error(w, "unknown", 502)
				return
			}
			json.NewEncoder(w).Encode(fixture(t, "events_send.json"))
		case r.URL.Path == "/sessions/sess_1/events" && r.Method == "GET":
			if r.URL.Query().Get("page") != "" {
				t.Error("event ID used as page cursor")
			}
			data := h.events
			if r.URL.Query().Get("order") == "desc" {
				data = append([]map[string]any(nil), h.events...)
				for i, j := 0, len(data)-1; i < j; i, j = i+1, j-1 {
					data[i], data[j] = data[j], data[i]
				}
				if len(data) > 1 {
					data = data[:1]
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data})
		case r.URL.Path == "/files" && r.Method == "POST":
			if r.URL.Query().Get("beta") != "true" {
				t.Error("provider file upload without beta")
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			if _, header, err := r.FormFile("file"); err == nil {
				h.uploadedFiles = append(h.uploadedFiles, header.Filename)
				h.uploadedTypes = append(h.uploadedTypes, header.Header.Get("Content-Type"))
			}
			json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("file_%d", len(h.uploadedFiles))})
		case r.URL.Path == "/sessions/sess_1/resources" && r.Method == "POST":
			if r.URL.Query().Get("beta") != "true" {
				t.Error("provider resource mount without beta")
			}
			var resource map[string]any
			json.NewDecoder(r.Body).Decode(&resource)
			h.mountedFiles = append(h.mountedFiles, resource)
			json.NewEncoder(w).Encode(map[string]any{"type": "file", "id": "sesrsc_1"})
		case r.URL.Path == "/sessions/sess_1/resources" && r.Method == "GET":
			if r.URL.Query().Get("beta") != "true" {
				t.Error("provider resource list without beta")
			}
			json.NewEncoder(w).Encode(map[string]any{"data": h.sessionResources})
		case strings.HasPrefix(r.URL.Path, "/sessions/sess_1/resources/") && r.Method == "POST":
			if r.URL.Query().Get("beta") != "true" {
				t.Error("provider resource update without beta")
			}
			var update map[string]any
			json.NewDecoder(r.Body).Decode(&update)
			h.resourceUpdates = append(h.resourceUpdates, update)
			json.NewEncoder(w).Encode(map[string]any{})
		case strings.HasPrefix(r.URL.Path, "/files/") && strings.HasSuffix(r.URL.Path, "/content"):
			if r.URL.Query().Get("beta") != "true" {
				t.Error("provider file download without beta")
			}
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/files/"), "/content")
			content, ok := h.fileContent[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			io.WriteString(w, content)
		case strings.HasPrefix(r.URL.Path, "/files/"):
			if r.URL.Query().Get("beta") != "true" {
				t.Error("provider file metadata without beta")
			}
			id := strings.TrimPrefix(r.URL.Path, "/files/")
			content, ok := h.fileContent[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			name := "output.txt"
			for _, resource := range h.sessionResources {
				if resource["file_id"] != id {
					continue
				}
				if mount, _ := resource["mount_path"].(string); mount != "" {
					parts := strings.Split(mount, "/")
					name = parts[len(parts)-1]
				}
			}
			scope := h.fileScope
			if scope == "" {
				scope = "sess_1"
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"id": id, "filename": name, "size_bytes": len(content),
				"scope": map[string]any{"id": scope, "type": "session"}, "downloadable": true,
			})
		case r.URL.Path == "/sessions/sess_1":
			session := fixture(t, "session_get.json")
			session["status"] = "idle"
			if h.remoteStatus != "" {
				session["status"] = h.remoteStatus
			}
			session["metadata"] = h.priorMetadata
			json.NewEncoder(w).Encode(session)
		default:
			t.Errorf("unexpected Claude request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(q.Close)
	var err error
	h.b, err = NewWithProfile(Config{MulticaURL: m.URL, MulticaToken: "multica-secret", WorkspaceID: workspaceID, AnthropicURL: q.URL, AnthropicToken: "anthropic-secret", EnvironmentID: "env_1", StateDir: t.TempDir(), IdleGrace: time.Nanosecond}, h.profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.b.state = &journal{Binding: "test", RuntimeID: "runtime-1"}
	return h
}

// followUp makes the claimed task continue the idle session sess_1, so the
// prompt goes out as a separate user.message.
func (h *harness) followUp() {
	h.task.PriorSessionID = "sess_1"
	h.priorMetadata = map[string]string{resourcesMetadataKey: resourceFingerprint(nil)}
}
func (h *harness) finish(reason string) {
	h.remoteStatus = "idle"
	h.events = append(h.events, map[string]any{"id": "evt_reply", "type": "agent.message", "content": []map[string]string{{"type": "text", "text": "Tests passed. PR: https://example.com/pr/1"}}}, map[string]any{"id": "evt_idle", "type": "session.status_idle", "stop_reason": map[string]string{"type": reason}})
}
func TestIssueRun(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	if err := h.b.step(ctx); err != nil {
		t.Fatal(err)
	}
	if h.complete != nil {
		t.Fatal("completed before user turn")
	}
	if !strings.Contains(h.prompt, "Fix bug") || strings.Contains(h.prompt, "secret") || strings.Contains(h.prompt, "mat_task") {
		t.Fatalf("incorrect prompt: %s", h.prompt)
	}
	h.finish("end_turn")
	if err := h.b.step(ctx); err != nil {
		t.Fatal(err)
	}
	if h.complete["session_id"] != "sess_1" || !strings.Contains(fmt.Sprint(h.complete["output"]), "Tests passed") {
		t.Fatalf("result: %#v", h.complete)
	}
	if h.creates != 1 || h.initialEvents != 1 || h.sends != 0 || len(h.messages) != 1 || h.b.state.first() != nil {
		t.Fatalf("incorrect lifecycle: creates=%d initial_events=%d sends=%d", h.creates, h.initialEvents, h.sends)
	}
	data, err := os.ReadFile(h.b.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") {
		t.Fatal("credential persisted")
	}
}
func TestOMAIssueRun(t *testing.T) {
	h := setupOMA(t)
	ctx := context.Background()
	if err := h.b.step(ctx); err != nil {
		t.Fatal(err)
	}
	if h.complete != nil {
		t.Fatal("completed before user turn")
	}
	if h.b.profile.Provider != "oma_cloud" {
		t.Fatalf("provider = %q", h.b.profile.Provider)
	}
	if !strings.Contains(h.prompt, "OMA Managed Agents environment") || strings.Contains(h.prompt, "Claude") {
		t.Fatalf("OMA prompt named the wrong provider: %s", h.prompt)
	}
	h.finish("end_turn")
	if err := h.b.step(ctx); err != nil {
		t.Fatal(err)
	}
	if h.complete["session_id"] != "sess_1" || !strings.Contains(fmt.Sprint(h.complete["output"]), "Tests passed") {
		t.Fatalf("result: %#v", h.complete)
	}
	if h.creates != 1 || h.initialEvents != 1 || h.sends != 0 || len(h.messages) != 1 || h.b.state.first() != nil {
		t.Fatalf("incorrect OMA lifecycle: creates=%d initial_events=%d sends=%d", h.creates, h.initialEvents, h.sends)
	}
}

// chatTask turns the claimed task into a chat turn served by the OMA profile.
func chatTask(h *harness, message string) {
	h.task.IssueID = ""
	h.task.ChatSessionID = "chat-1"
	h.task.ChatMessage = message
}

func TestOMAChatRun(t *testing.T) {
	h := setupOMA(t)
	chatTask(h, "Explain the build")
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.complete != nil {
		t.Fatal("completed before the agent replied")
	}
	for _, want := range []string{"chatting with a user", "Run: task-1", "Explain the build"} {
		if !strings.Contains(h.prompt, want) {
			t.Fatalf("chat prompt missing %q: %s", want, h.prompt)
		}
	}
	for _, unwanted := range []string{"Complete the following Multica issue", "pull request", "read issue"} {
		if strings.Contains(h.prompt, unwanted) {
			t.Fatalf("chat prompt carried issue-only %q: %s", unwanted, h.prompt)
		}
	}
	h.finish("end_turn")
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.complete["session_id"] != "sess_1" || !strings.Contains(fmt.Sprint(h.complete["output"]), "Tests passed") {
		t.Fatalf("result: %#v", h.complete)
	}
	if h.creates != 1 || h.initialEvents != 1 || h.sends != 0 || h.b.state.first() != nil {
		t.Fatalf("incorrect chat lifecycle: creates=%d initial_events=%d sends=%d", h.creates, h.initialEvents, h.sends)
	}
}

func TestOMAChatNewSessionAttachments(t *testing.T) {
	h := setupOMA(t)
	chatTask(h, "Read these")
	h.task.ChatMessageAttachments = []daemon.ChatAttachmentMeta{
		{ID: "img-1", Filename: "shot.png", ContentType: "image/png"},
		{ID: "doc-1", Filename: "notes.txt", ContentType: "text/plain"},
	}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.failure != nil {
		t.Fatalf("run failed: %v", h.failure)
	}
	if len(h.uploadedFiles) != 2 || h.uploadedFiles[0] != "shot.png" || h.uploadedFiles[1] != "notes.txt" {
		t.Fatalf("provider uploads = %v", h.uploadedFiles)
	}
	if len(h.uploadedTypes) != 2 || h.uploadedTypes[0] != "image/png" || h.uploadedTypes[1] != "text/plain" {
		t.Fatalf("provider upload types = %v", h.uploadedTypes)
	}
	if !strings.Contains(h.prompt, "- /mnt/session/uploads/task-1/notes.txt (mounted file)") {
		t.Fatalf("mounted file missing from prompt: %s", h.prompt)
	}
	if !strings.Contains(h.prompt, "- shot.png (image attached to this message)") {
		t.Fatalf("image missing from prompt: %s", h.prompt)
	}
	if len(h.initialContent) != 2 || h.initialContent[1]["type"] != "image" {
		t.Fatalf("initial content = %#v", h.initialContent)
	}
	source, _ := h.initialContent[1]["source"].(map[string]any)
	if source["type"] != "file" || source["file_id"] != "file_1" {
		t.Fatalf("image source = %#v", source)
	}
	if len(h.initialResources) != 1 || h.initialResources[0]["mount_path"] != "/uploads/task-1/notes.txt" {
		t.Fatalf("session resources = %#v", h.initialResources)
	}
	if h.creates != 1 {
		t.Fatalf("creates = %d", h.creates)
	}
}

func TestOMAChatFollowUpMountsAttachments(t *testing.T) {
	h := setupOMA(t)
	chatTask(h, "One more look")
	h.task.PriorSessionID = "sess_1"
	h.priorMetadata = map[string]string{resourcesMetadataKey: resourceFingerprint(nil)}
	h.task.ChatMessageAttachments = []daemon.ChatAttachmentMeta{
		{ID: "img-1", Filename: "shot.png", ContentType: "image/png"},
		{ID: "doc-1", Filename: "notes.txt", ContentType: "text/plain"},
	}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.failure != nil {
		t.Fatalf("run failed: %v", h.failure)
	}
	if h.creates != 0 || h.sends != 1 {
		t.Fatalf("creates=%d sends=%d, want the prior session continued", h.creates, h.sends)
	}
	if len(h.mountedFiles) != 1 || h.mountedFiles[0]["mount_path"] != "/uploads/task-1/notes.txt" {
		t.Fatalf("mounted files = %#v", h.mountedFiles)
	}
	if len(h.sendContent) != 2 || h.sendContent[1]["type"] != "image" {
		t.Fatalf("sent content = %#v", h.sendContent)
	}
}

func outputResource(fileID, mountPath string) map[string]any {
	return map[string]any{"type": "file", "file_id": fileID, "mount_path": mountPath}
}

func TestOMAChatDeliversOutputFiles(t *testing.T) {
	h := setupOMA(t)
	chatTask(h, "Write the report")
	h.sessionResources = []map[string]any{outputResource("file_out", "/mnt/user-data/outputs/report.txt")}
	h.fileContent = map[string]string{"file_out": "report body"}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.prompt, "Files you write under /mnt/user-data/outputs/ are attached to your reply") {
		t.Fatalf("prompt missing the output-file instruction: %s", h.prompt)
	}
	h.finish("end_turn")
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.uploads != 1 || len(h.multicaUploads) != 1 || h.multicaUploads[0] != "report.txt" {
		t.Fatalf("uploads=%d names=%v", h.uploads, h.multicaUploads)
	}
	if h.complete == nil || !strings.Contains(fmt.Sprint(h.complete["output"]), "Tests passed") {
		t.Fatalf("complete = %#v", h.complete)
	}
	if strings.Contains(fmt.Sprint(h.complete["output"]), "Attachment delivery notice") {
		t.Fatalf("unexpected delivery notice: %v", h.complete["output"])
	}
}

// A continued chat must attach only the files its own turn produced.
func TestOMAChatDeliversOnlyNewOutputFiles(t *testing.T) {
	h := setupOMA(t)
	chatTask(h, "Add another file")
	h.task.PriorSessionID = "sess_1"
	h.priorMetadata = map[string]string{resourcesMetadataKey: resourceFingerprint(nil)}
	h.sessionResources = []map[string]any{outputResource("file_old", "/mnt/user-data/outputs/earlier.txt")}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.sessionResources = append(h.sessionResources, outputResource("file_new", "/mnt/user-data/outputs/new.txt"))
	h.fileContent = map[string]string{
		"file_old": "earlier body",
		"file_new": "new body",
	}
	h.finish("end_turn")
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.uploads != 1 || len(h.multicaUploads) != 1 || h.multicaUploads[0] != "new.txt" {
		t.Fatalf("uploads=%d names=%v, want only new.txt", h.uploads, h.multicaUploads)
	}
}

func TestOMAChatRefusesUnscopedOutputFile(t *testing.T) {
	h := setupOMA(t)
	chatTask(h, "Write the report")
	h.sessionResources = []map[string]any{outputResource("file_out", "/mnt/user-data/outputs/report.txt")}
	h.fileContent = map[string]string{"file_out": "report body"}
	h.fileScope = "another_session"
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.finish("end_turn")
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.uploads != 0 {
		t.Fatalf("uploads = %d, want the unscoped file refused", h.uploads)
	}
	if h.complete == nil || !strings.Contains(fmt.Sprint(h.complete["output"]), "access to the file could not be verified") {
		t.Fatalf("complete = %#v, want a delivery notice", h.complete)
	}
}

func TestClaudeProfileRejectsChat(t *testing.T) {
	h := setup(t)
	chatTask(h, "hi")
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.creates != 0 || h.failure == nil || !strings.Contains(fmt.Sprint(h.failure["error"]), "chat runs are not supported") {
		t.Fatalf("creates=%d failure=%v, want a chat refusal", h.creates, h.failure)
	}
}

// A continued chat whose earlier turn used the same words must not treat that
// earlier turn's idle event as this run's completion.
func TestChatFollowUpSkipsEarlierIdenticalMessage(t *testing.T) {
	h := setupOMA(t)
	chatTask(h, "hi")
	h.task.PriorSessionID = "sess_1"
	h.priorMetadata = map[string]string{resourcesMetadataKey: resourceFingerprint(nil)}
	old := []map[string]any{
		{"id": "evt_old_user", "type": "user.message", "content": []map[string]string{{"type": "text", "text": "hi"}}, "processed_at": "2026-01-01T00:00:00Z"},
		{"id": "evt_old_reply", "type": "agent.message", "content": []map[string]string{{"type": "text", "text": "old reply"}}, "processed_at": "2026-01-01T00:00:01Z"},
		{"id": "evt_old_idle", "type": "session.status_idle", "stop_reason": map[string]string{"type": "end_turn"}, "processed_at": "2026-01-01T00:00:02Z"},
	}
	h.events = old
	h.history = func(prompt string) []map[string]any {
		return append(append([]map[string]any{}, old...),
			map[string]any{"id": "evt_new_user", "type": "user.message", "content": []map[string]string{{"type": "text", "text": prompt}}, "processed_at": "2026-01-02T00:00:00Z"},
			map[string]any{"id": "evt_new_reply", "type": "agent.message", "content": []map[string]string{{"type": "text", "text": "new reply"}}, "processed_at": "2026-01-02T00:00:01Z"},
			map[string]any{"id": "evt_new_idle", "type": "session.status_idle", "stop_reason": map[string]string{"type": "end_turn"}, "processed_at": "2026-01-02T00:00:02Z"},
		)
	}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.creates != 0 || h.sends != 1 {
		t.Fatalf("creates=%d sends=%d, want the idle prior session continued", h.creates, h.sends)
	}
	if h.complete == nil || h.complete["output"] != "new reply" {
		t.Fatalf("complete = %#v, want this turn's reply", h.complete)
	}
	if h.b.state.first() != nil {
		t.Fatal("run was not removed after completion")
	}
}

func TestTransientIdleDoesNotCompleteRun(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	if err := h.b.step(ctx); err != nil {
		t.Fatal(err)
	}
	h.b.cfg.IdleGrace = time.Hour
	h.finish("end_turn")
	if err := h.b.step(ctx); err != nil {
		t.Fatal(err)
	}
	if h.complete != nil {
		t.Fatal("completed on an idle that the provider immediately resumed")
	}
	r := h.b.state.first()
	if r == nil || r.Phase != "idle" {
		t.Fatalf("run = %+v, want pending idle", r)
	}
	h.b.cfg.IdleGrace = time.Nanosecond
	h.events = append(h.events, map[string]any{"id": "evt_resumed", "type": "session.status_running"})
	if err := h.b.step(ctx); err != nil {
		t.Fatal(err)
	}
	if h.complete != nil || r.Phase != "running" {
		t.Fatalf("resumed idle completed: complete=%v run=%+v", h.complete, r)
	}
	h.finish("end_turn")
	if err := h.b.step(ctx); err != nil {
		t.Fatal(err)
	}
	if h.complete == nil {
		t.Fatal("final idle did not complete")
	}
}

func TestAmbiguousSubmitReconcilesAfterRestart(t *testing.T) {
	h := setup(t)
	h.followUp()
	h.lostSend = true
	if err := h.b.step(context.Background()); err == nil {
		t.Fatal("expected ambiguous failure")
	}
	j, err := readJournal(h.b.path, "test")
	if err != nil {
		t.Fatal(err)
	}
	h.b.state = j
	if j.first().Phase != "sending" || j.first().SessionID != "sess_1" {
		t.Fatal("missing intent")
	}
	h.finish("end_turn")
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.sends != 1 || h.creates != 0 || h.complete == nil {
		t.Fatal("restart duplicated/lost execution")
	}
}
func TestUnconfirmedSubmissionDoesNotResend(t *testing.T) {
	h := setup(t)
	h.followUp()
	h.lostSend = true
	_ = h.b.step(context.Background())
	h.events = nil
	for range 2 {
		if err := h.b.step(context.Background()); err == nil {
			t.Fatal("expected reconciliation warning")
		}
	}
	if h.sends != 1 {
		t.Fatal("duplicated submission")
	}
}
func TestCancelStopsRemote(t *testing.T) {
	h := setup(t)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.status = "cancelled"
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.cancels != 1 || h.b.state.first() != nil || h.complete != nil {
		t.Fatal("incorrect cancellation")
	}
}
func TestRequiresActionIsNotSuccess(t *testing.T) {
	h := setup(t)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.finish("requires_action")
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.complete != nil || !strings.Contains(fmt.Sprint(h.failure["error"]), "requires_action") {
		t.Fatalf("unexpected report: %#v", h.failure)
	}
}
func TestLocalDirectoryFailsBeforeExecution(t *testing.T) {
	h := setup(t)
	h.task.ProjectResources = []daemon.ProjectResourceData{{ResourceType: "local_directory"}}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.creates != 0 || h.failure == nil {
		t.Fatal("local directory ignored")
	}
}
func TestStateLockAndBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	f, err := lockState(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockState(path); err == nil {
		second.Close()
		t.Fatal("second process acquired lock")
	}
	f.Close()
	f, err = lockState(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	path = filepath.Join(t.TempDir(), "state.json")
	if err := save(path, &journal{Binding: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := readJournal(path, "b"); err == nil {
		t.Fatal("wrong binding accepted")
	}
}
func TestAPIRejectsRedirectsAndRedactsErrors(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed redirect") }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(302)
		io.WriteString(w, "sensitive-secret")
	}))
	defer source.Close()
	a, err := newAPI(source.URL, "credential", "")
	if err != nil {
		t.Fatal(err)
	}
	err = a.call(context.Background(), http.MethodGet, "/", nil, nil)
	if err == nil || strings.Contains(err.Error(), "sensitive-secret") {
		t.Fatalf("unsafe error: %v", err)
	}
	if _, err := newAPI("http://remote.example", "token", ""); err == nil {
		t.Fatal("plaintext remote URL accepted")
	}
}

func TestOpaquePageCursorSurvivesRestart(t *testing.T) {
	h := setup(t)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests := 0
	q := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			if r.URL.Query().Get("page") != "" {
				t.Error("unexpected page cursor")
			}
			io.WriteString(w, `{"data":[{"id":"evt_user","type":"user.message"}],"next_page":"opaque-cursor"}`)
		} else {
			if r.URL.Query().Get("page") != "opaque-cursor" {
				t.Error("incorrect page request")
			}
			io.WriteString(w, `{"data":[{"id":"evt_done","type":"session.status_idle","stop_reason":{"type":"end_turn"}}],"next_page":null}`)
		}
	}))
	defer q.Close()
	h.b.anthropic.base = q.URL
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	j, err := readJournal(h.b.path, "test")
	if err != nil {
		t.Fatal(err)
	}
	h.b.state = j
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.complete == nil || requests != 2 {
		t.Fatal("pagination lost terminal event")
	}
}

func TestTerminalReportSurvivesRestart(t *testing.T) {
	h := setup(t)
	h.b.state.Runs = []*run{&run{TaskID: "task-1", SessionID: "sess_1", Phase: "final", Outcome: "completed", Output: "Recovered result"}}
	if err := h.b.persist(); err != nil {
		t.Fatal(err)
	}
	j, err := readJournal(h.b.path, "test")
	if err != nil {
		t.Fatal(err)
	}
	h.b.state = j
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.complete["output"] != "Recovered result" || h.creates != 0 || h.sends != 0 {
		t.Fatal("terminal recovery re-executed task")
	}
}

func TestRepositoryTokenStaysOutOfErrorsAndJournal(t *testing.T) {
	const token = "ghp_never_logged"
	for _, status := range []int{400, 502} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h := setup(t)
			var logs strings.Builder
			h.b.log = slog.New(slog.NewTextHandler(&logs, nil))
			h.task.Agent.RuntimeConfig = json.RawMessage(`{"claude_agent_id":"agent_1","repository":{"url":"https://github.com/acme/app.git","ref":"main"}}`)
			h.b.cfg.GitHubTokens = map[string]string{"https://github.com/acme/app": token}
			var sent string
			q := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					raw, _ := io.ReadAll(r.Body)
					sent = string(raw)
				}
				http.Error(w, "invalid authorization_token "+token, status)
			}))
			defer q.Close()
			h.b.anthropic.base = q.URL
			err := h.b.step(context.Background())
			if !strings.Contains(sent, `"authorization_token":"`+token+`"`) || !strings.Contains(sent, `"url":"https://github.com/acme/app"`) {
				t.Fatalf("create body lacks repository credential: %s", sent)
			}
			journal, _ := os.ReadFile(h.b.path)
			for name, text := range map[string]string{"error": fmt.Sprint(err), "failure": fmt.Sprint(h.failure), "logs": logs.String(), "journal": string(journal)} {
				if strings.Contains(text, token) {
					t.Errorf("token leaked into %s: %s", name, text)
				}
			}
		})
	}
}

func TestRejectedCreationFailsRun(t *testing.T) {
	h := setup(t)
	q := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad initial event", 400)
	}))
	defer q.Close()
	h.b.anthropic.base = q.URL
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.failure == nil || h.b.state.first() != nil {
		t.Fatal("definite rejection left run pending")
	}
}

func TestRegistrationAndShutdown(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspaces/" + workspaceID:
			json.NewEncoder(w).Encode(map[string]string{"id": workspaceID})
		case "/api/daemon/register":
			var req struct {
				Runtimes []struct {
					Type string `json:"type"`
					Mode string `json:"runtime_mode"`
				} `json:"runtimes"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			if len(req.Runtimes) != 1 || req.Runtimes[0].Type != Provider || req.Runtimes[0].Mode != "cloud" {
				t.Error("wrong registration provider")
			}
			io.WriteString(w, `{"runtimes":[{"id":"runtime-1","provider":"claude_managed"}]}`)
		case "/api/daemon/heartbeat":
			io.WriteString(w, `{}`)
			cancel()
		default:
			t.Errorf("unexpected startup request %s", r.URL)
		}
	}))
	defer m.Close()
	h.b.multica.base = m.URL
	q := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/agent_1" && r.URL.Path != "/environments/env_1" {
			t.Error("unexpected remote validation")
		}
		if r.URL.Path == "/environments/env_1" {
			json.NewEncoder(w).Encode(fixture(t, "environment_get.json"))
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"id": "agent_1"})
	}))
	defer q.Close()
	h.b.anthropic.base = q.URL
	err := h.b.Run(ctx)
	if err != nil && ctx.Err() == nil {
		t.Fatal(err)
	}
}

func TestPerAgentSessionBinding(t *testing.T) {
	for _, id := range []string{"agent_first", "agent_second"} {
		t.Run(id, func(t *testing.T) {
			h := setup(t)
			h.task.Agent.RuntimeConfig = json.RawMessage(fmt.Sprintf(`{"claude_agent_id":%q}`, id))
			if err := h.b.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if h.sessionAgent != id {
				t.Fatalf("session agent = %q, want %q", h.sessionAgent, id)
			}
			restored, err := readJournal(h.b.path, "test")
			if err != nil || restored.first().AgentID != id {
				t.Fatal("per-run agent binding not persisted")
			}
		})
	}
}
func TestMissingAgentBindingFailsBeforeSession(t *testing.T) {
	for _, raw := range []string{`{}`, `{"claude_agent_id":"../agents"}`, `{"claude_agent_id":1}`, `{"claude_agent_id":"agent_"}`} {
		h := setup(t)
		h.task.Agent.RuntimeConfig = json.RawMessage(raw)
		if err := h.b.step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if h.creates != 0 || h.status != "failed" {
			t.Fatal("unbound task created a Claude session")
		}
	}
}

func TestRunDeadlineReleasesQueue(t *testing.T) {
	h := setup(t)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.b.state.first().Deadline = time.Now().Add(-time.Second)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.failure == nil || h.b.state.first() != nil || h.cancels != 1 {
		t.Fatal("expired run did not cancel and finish")
	}
}

func TestAmbiguousCreationRetriesOnceWhenNotListed(t *testing.T) {
	h := setup(t)
	h.lostCreates = 1
	if err := h.b.step(context.Background()); err == nil {
		t.Fatal("expected ambiguous create to surface")
	}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.creates != 2 || len(h.listQueries) != 1 || h.failure != nil || h.b.state.first().SessionID != "sess_1" {
		t.Fatalf("retry: creates=%d lists=%d failure=%v", h.creates, len(h.listQueries), h.failure)
	}
	h.finish("end_turn")
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.complete["session_id"] != "sess_1" {
		t.Fatalf("retried run did not complete: %#v", h.complete)
	}
}

func TestAmbiguousCreationFailsAfterOneRetry(t *testing.T) {
	h := setup(t)
	h.lostCreates = 3
	for range 2 {
		if err := h.b.step(context.Background()); err == nil {
			t.Fatal("expected ambiguous create to surface")
		}
	}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.creates != 2 || !strings.Contains(fmt.Sprint(h.failure["error"]), "session creation was not confirmed") || h.b.state.first() != nil {
		t.Fatalf("creates=%d failure=%v", h.creates, h.failure)
	}
}

func TestAmbiguousCreationReattachesListedSession(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) {
			h := setup(t)
			h.lostCreates, h.lostCreatePersists = 1, true
			before := time.Now()
			if err := h.b.step(context.Background()); err == nil {
				t.Fatal("expected ambiguous create to surface")
			}
			if h.failure != nil || len(h.listQueries) != 0 {
				t.Fatal("ambiguous create was finalized or reconciled in the same request")
			}
			if restart {
				j, err := readJournal(h.b.path, "test")
				if err != nil {
					t.Fatal(err)
				}
				if j.first().Phase != "creating" || j.first().CreateAttemptAt.Before(before) {
					t.Fatalf("attempt not journaled: %#v", j.first())
				}
				h.b.state = j
			}
			if err := h.b.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if h.creates != 1 || h.sends != 0 || h.b.state.first().SessionID != "sess_1" {
				t.Fatalf("not reattached: creates=%d sends=%d run=%#v", h.creates, h.sends, h.b.state.first())
			}
			q := h.listQueries[0]
			from, _ := time.Parse(time.RFC3339, q.Get("created_at[gte]"))
			if q.Get("agent_id") != "agent_1" || q.Get("order") != "desc" || !from.Before(before.Add(-time.Minute)) {
				t.Fatalf("list query: %v", q)
			}
			h.finish("end_turn")
			if err := h.b.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if h.complete["session_id"] != "sess_1" || h.creates != 1 {
				t.Fatalf("reattached run did not complete: %#v", h.complete)
			}
		})
	}
}

func TestTimeoutKeepsFailedCancellationForRecovery(t *testing.T) {
	h := setup(t)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer remote.Close()
	h.b.anthropic.base = remote.URL
	h.b.state.first().Deadline = time.Now().Add(-time.Second)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.b.state.first() != nil || len(h.b.state.Cleanup) != 1 || h.failure == nil {
		t.Fatal("failed cancellation blocked queue or lost session")
	}
	j, err := readJournal(h.b.path, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Cleanup) != 1 || j.Cleanup[0] != "sess_1" {
		t.Fatal("cleanup not durable")
	}
}

func TestDeletedRemoteSessionFailsRun(t *testing.T) {
	h := setup(t)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	remote := httptest.NewServer(http.NotFoundHandler())
	defer remote.Close()
	h.b.anthropic.base = remote.URL
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.failure == nil || h.b.state.first() != nil {
		t.Fatal("deleted session not terminal")
	}
}

func TestRateLimitedCreationRetriesAfterDelay(t *testing.T) {
	h := setup(t)
	attempts := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { attempts++; w.WriteHeader(http.StatusTooManyRequests) }))
	defer remote.Close()
	h.b.anthropic.base = remote.URL
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.b.state.first() == nil || h.b.state.first().Phase != "prepared" || h.failure != nil {
		t.Fatal("rate limit became permanent failure")
	}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatal("rate limit retry ignored backoff")
	}
}

func TestSubmissionDeadlineReleasesUnconfirmedRun(t *testing.T) {
	h := setup(t)
	h.followUp()
	h.lostSend = true
	if err := h.b.step(context.Background()); err == nil {
		t.Fatal("expected lost response")
	}
	h.events = nil
	h.b.state.first().SubmissionDeadline = time.Now().Add(-time.Second)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.failure == nil || h.b.state.first() != nil || h.sends != 1 {
		t.Fatal("unconfirmed submission blocked queue or resent")
	}
}

func TestManagedJournalPersistsWithoutLocalFiles(t *testing.T) {
	h := setup(t)
	var saved []byte
	h.b.cfg.SaveState = func(_ context.Context, raw []byte) error { saved = append([]byte(nil), raw...); return nil }
	h.b.path = "/not/a/writable/path"
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, err := decodeJournal(saved, "test")
	if err != nil {
		t.Fatal(err)
	}
	if restored.first() == nil || restored.first().SessionID != "sess_1" || restored.first().Deadline.IsZero() {
		t.Fatal("managed recovery lost session or deadline")
	}
}

func TestToolResultsPreserveCallIdentity(t *testing.T) {
	for _, prefix := range []string{"agent.", "agent.mcp_", "agent.custom_"} {
		t.Run(prefix, func(t *testing.T) {
			h := setup(t)
			ctx := context.Background()
			if err := h.b.step(ctx); err != nil {
				t.Fatal(err)
			}
			h.events = append(h.events,
				map[string]any{"id": "call-a", "type": prefix + "tool_use", "name": "Read"},
				map[string]any{"id": "call-b", "type": prefix + "tool_use", "name": "Read"},
				map[string]any{"id": "result-b", "type": prefix + "tool_result", "tool_use_id": "call-b"},
				map[string]any{"id": "result-a", "type": prefix + "tool_result", "tool_use_id": "call-a"},
			)
			if err := h.b.step(ctx); err != nil {
				t.Fatal(err)
			}
			if len(h.messages) != 4 {
				t.Fatalf("got %d messages", len(h.messages))
			}
			for i, want := range []string{"call-a", "call-b", "call-b", "call-a"} {
				if h.messages[i]["call_id"] != want {
					t.Errorf("message %d: call_id = %v, want %s", i, h.messages[i]["call_id"], want)
				}
			}
		})
	}
}

// first returns the oldest active run, which single-run tests inspect.
func (j *journal) first() *run {
	if len(j.Runs) == 0 {
		return nil
	}
	return j.Runs[0]
}

func TestJournalStorageConfig(t *testing.T) {
	base := Config{MulticaURL: "http://127.0.0.1:1", MulticaToken: "m", WorkspaceID: workspaceID, AnthropicURL: "http://127.0.0.1:2", AnthropicToken: "q", EnvironmentID: "env_1"}
	load := func(context.Context) ([]byte, error) { return nil, nil }
	save := func(context.Context, []byte) error { return nil }

	external := base
	external.LoadState, external.SaveState = load, save
	if _, err := New(external, nil); err != nil {
		t.Fatalf("external journal needs no state directory: %v", err)
	}
	onDisk := base
	onDisk.StateDir = t.TempDir()
	if _, err := New(onDisk, nil); err != nil {
		t.Fatalf("on-disk journal rejected: %v", err)
	}
	for name, cfg := range map[string]Config{
		"no journal":     base,
		"load only":      func() Config { c := base; c.LoadState = load; c.StateDir = t.TempDir(); return c }(),
		"save only":      func() Config { c := base; c.SaveState = save; c.StateDir = t.TempDir(); return c }(),
		"no environment": func() Config { c := external; c.EnvironmentID = ""; return c }(),
	} {
		if _, err := New(cfg, nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestDeadlineCancelsSessionFromUnconfirmedCreate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(*harness)
	}{
		{"creating", func(h *harness) {
			h.lostCreates, h.lostCreatePersists = 1, true
			if err := h.b.step(context.Background()); err == nil {
				t.Fatal("expected ambiguous create to surface")
			}
		}},
		{"prepared for the retry", func(h *harness) {
			h.task = nil
			h.status = "running"
			h.remoteStatus = "running"
			h.remote = append(h.remote, map[string]any{"id": "sess_1", "agent": map[string]string{"id": "agent_1"}, "created_at": time.Now().UTC().Format(time.RFC3339), "metadata": map[string]string{"multica_task_id": "task-1", "multica_workspace_id": workspaceID}})
			h.b.state.Runs = []*run{{TaskID: "task-1", AgentID: "agent_1", Phase: "prepared", CreateAttemptAt: time.Now(), CreateRetried: true, RetryAt: time.Now().Add(time.Hour)}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setup(t)
			tc.start(h)
			h.b.state.first().Deadline = time.Now().Add(-time.Second)
			if err := h.b.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(fmt.Sprint(h.failure["error"]), "deadline") || h.b.state.first() != nil {
				t.Fatalf("expired run not failed: %v", h.failure)
			}
			if h.cancels != 1 || len(h.b.state.Cleanup) != 0 {
				t.Fatalf("session from the lost create not cancelled: cancels=%d cleanup=%v", h.cancels, h.b.state.Cleanup)
			}
		})
	}
}

func TestEndedTaskCancelsSessionFromUnconfirmedCreate(t *testing.T) {
	for _, status := range []string{"cancelled", "failed", "completed"} {
		t.Run(status, func(t *testing.T) {
			h := setup(t)
			h.lostCreates, h.lostCreatePersists = 1, true
			if err := h.b.step(context.Background()); err == nil {
				t.Fatal("expected ambiguous create to surface")
			}
			h.status = status
			if err := h.b.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if h.b.state.first() != nil || h.failure != nil || h.complete != nil {
				t.Fatalf("ended task not removed quietly: failure=%v complete=%v", h.failure, h.complete)
			}
			if h.cancels != 1 || len(h.b.state.Cleanup) != 0 {
				t.Fatalf("session from the lost create not cancelled: cancels=%d cleanup=%v", h.cancels, h.b.state.Cleanup)
			}
		})
	}
}

func TestDeadlineRejectsWhenCreateLookupFails(t *testing.T) {
	h := setup(t)
	var logs strings.Builder
	h.b.log = slog.New(slog.NewTextHandler(&logs, nil))
	h.lostCreates = 1
	if err := h.b.step(context.Background()); err == nil {
		t.Fatal("expected ambiguous create to surface")
	}
	h.listStatus = http.StatusServiceUnavailable
	h.b.state.first().Deadline = time.Now().Add(-time.Second)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.failure == nil || h.b.state.first() != nil {
		t.Fatal("failed lookup blocked the deadline")
	}
	if !strings.Contains(logs.String(), "remote session may exist") {
		t.Fatalf("possible remote session not logged: %s", logs.String())
	}
}

func TestRejectedCreateLookupFailsRun(t *testing.T) {
	h := setup(t)
	var logs strings.Builder
	h.b.log = slog.New(slog.NewTextHandler(&logs, nil))
	h.lostCreates = 1
	if err := h.b.step(context.Background()); err == nil {
		t.Fatal("expected ambiguous create to surface")
	}
	h.listStatus = http.StatusForbidden
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.failure == nil || h.b.state.first() != nil || h.creates != 1 {
		t.Fatalf("definite lookup rejection left run pending: creates=%d failure=%v", h.creates, h.failure)
	}
	if !strings.Contains(logs.String(), "remote session may exist") {
		t.Fatalf("possible remote session not logged: %s", logs.String())
	}
}

func TestCreateLookupBeyondPageLimitCountsAsNotFound(t *testing.T) {
	h := setup(t)
	var logs strings.Builder
	h.b.log = slog.New(slog.NewTextHandler(&logs, nil))
	h.lostCreates = 1
	h.endlessList = true
	if err := h.b.step(context.Background()); err == nil {
		t.Fatal("expected ambiguous create to surface")
	}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.creates != 2 || len(h.listQueries) != maxCreateLookupPages || h.failure != nil || h.b.state.first().SessionID != "sess_1" {
		t.Fatalf("creates=%d lists=%d failure=%v", h.creates, len(h.listQueries), h.failure)
	}
	if !strings.Contains(logs.String(), "page limit") {
		t.Fatalf("truncated lookup not logged: %s", logs.String())
	}
}

func TestNewSessionStartsWithoutPromptEcho(t *testing.T) {
	for _, reattach := range []bool{false, true} {
		t.Run(fmt.Sprintf("reattach=%v", reattach), func(t *testing.T) {
			h := setup(t)
			h.initialEcho = "prompt as normalized by Claude"
			if reattach {
				h.lostCreates, h.lostCreatePersists = 1, true
				if err := h.b.step(context.Background()); err == nil {
					t.Fatal("expected ambiguous create to surface")
				}
			}
			h.finish("end_turn")
			if err := h.b.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if h.complete["session_id"] != "sess_1" || !strings.Contains(fmt.Sprint(h.complete["output"]), "Tests passed") || h.creates != 1 {
				t.Fatalf("new session never started: creates=%d complete=%v", h.creates, h.complete)
			}
		})
	}
}

func TestTransientCreateLookupFailureBacksOff(t *testing.T) {
	h := setup(t)
	h.lostCreates = 1
	if err := h.b.step(context.Background()); err == nil {
		t.Fatal("expected ambiguous create to surface")
	}
	h.listStatus = http.StatusServiceUnavailable
	if err := h.b.step(context.Background()); err == nil {
		t.Fatal("transient lookup failure was not reported")
	}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.listQueries) != 1 || h.failure != nil || h.b.state.first().Phase != "creating" {
		t.Fatalf("lookup retried without delay: lists=%d failure=%v", len(h.listQueries), h.failure)
	}
}

func TestVaultIDsAttachToNewSessions(t *testing.T) {
	h := setup(t)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.creates != 1 || h.vaultIDs != nil {
		t.Fatalf("unconfigured vaults sent: creates=%d vault_ids=%v", h.creates, h.vaultIDs)
	}

	h = setup(t)
	h.b.cfg.VaultIDs = []string{"vlt_github", "vlt_other"}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.creates != 1 || strings.Join(h.vaultIDs, ",") != "vlt_github,vlt_other" {
		t.Fatalf("vault_ids = %v", h.vaultIDs)
	}
}

// vault_ids cannot be changed on an existing session, so a follow-up only
// reuses a session created with the same vaults.
func TestFollowUpReusesSessionOnlyWithSameVaults(t *testing.T) {
	h := setup(t)
	h.followUp()
	h.b.cfg.VaultIDs = []string{"vlt_github"}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.creates != 1 || h.sends != 0 {
		t.Fatalf("session without the vault reused: creates=%d sends=%d", h.creates, h.sends)
	}

	h = setup(t)
	h.followUp()
	h.b.cfg.VaultIDs = []string{"vlt_github"}
	h.priorMetadata = map[string]string{resourcesMetadataKey: h.b.sessionFingerprint(nil)}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.creates != 0 || h.sends != 1 {
		t.Fatalf("session with the same vault not reused: creates=%d sends=%d", h.creates, h.sends)
	}
}

func TestNewRejectsMalformedVaultID(t *testing.T) {
	_, err := New(Config{MulticaURL: "http://127.0.0.1:1", MulticaToken: "multica-secret", WorkspaceID: workspaceID, AnthropicURL: "http://127.0.0.1:2", AnthropicToken: "anthropic-secret", EnvironmentID: "env_1", StateDir: t.TempDir(), VaultIDs: []string{"github"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "vlt_") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckValidatesVaults(t *testing.T) {
	h := setup(t)
	h.b.cfg.VaultIDs = []string{"vlt_github"}
	m := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"id": workspaceID})
	}))
	defer m.Close()
	h.b.multica.base = m.URL
	var paths []string
	vault := "vlt_github"
	q := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/environments/env_1":
			json.NewEncoder(w).Encode(fixture(t, "environment_get.json"))
		case "/vaults/vlt_github":
			json.NewEncoder(w).Encode(map[string]string{"id": vault})
		default:
			http.NotFound(w, r)
		}
	}))
	defer q.Close()
	h.b.anthropic.base = q.URL
	if err := h.b.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "/environments/env_1,/vaults/vlt_github" {
		t.Fatalf("checked %v", paths)
	}
	vault = "vlt_someone_else"
	if err := h.b.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "vault") {
		t.Fatalf("mismatched vault accepted: %v", err)
	}
}

// A vault can hand the session a GitHub MCP server that reaches repositories
// beyond the mounted one, so the prompt names the one repository it may change
// and forbids changes when none is mounted.
func TestPromptScopesChangesToTheMountedRepository(t *testing.T) {
	h := setup(t)
	h.task.IssueIdentifier = "MUL-1"
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.prompt, "No repository is mounted") || strings.Contains(h.prompt, "push a new branch") {
		t.Fatalf("unmounted run may change repositories:\n%s", h.prompt)
	}

	h = setup(t)
	h.task.IssueIdentifier = "MUL-1"
	h.task.Agent.RuntimeConfig = json.RawMessage(`{"claude_agent_id":"agent_1","repository":{"url":"https://github.com/owner/repo"}}`)
	h.b.cfg.GitHubTokens = map[string]string{"https://github.com/owner/repo": "token"}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.prompt, "Change only the mounted repository https://github.com/owner/repo") || strings.Contains(h.prompt, "No repository is mounted") {
		t.Fatalf("mounted run not scoped to its repository:\n%s", h.prompt)
	}
}

func TestCancelledTaskKeepsUnresolvedCreateUntilLookupRecovers(t *testing.T) {
	h := setup(t)
	h.lostCreates, h.lostCreatePersists = 1, true
	if err := h.b.step(context.Background()); err == nil {
		t.Fatal("expected ambiguous create to surface")
	}
	h.status = "cancelled"
	h.listStatus = http.StatusServiceUnavailable
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.b.state.first() != nil || h.cancels != 0 {
		t.Fatalf("cancelled run kept or session stopped early: cancels=%d", h.cancels)
	}
	// The bridge restarts and the provider recovers.
	j, err := readJournal(h.b.path, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Unresolved) != 1 || j.Unresolved[0].TaskID != "task-1" || j.Unresolved[0].AgentID != "agent_1" || j.Unresolved[0].CreateAttemptAt.IsZero() {
		t.Fatalf("unresolved creation not persisted: %+v", j.Unresolved)
	}
	h.b.state = j
	h.b.cleanup(context.Background())
	if len(h.b.state.Unresolved) != 1 || h.cancels != 0 {
		t.Fatalf("lookup outage dropped the creation: unresolved=%+v cancels=%d", h.b.state.Unresolved, h.cancels)
	}
	h.listStatus = 0
	h.b.cleanup(context.Background())
	if h.cancels != 1 || len(h.b.state.Unresolved) != 0 || len(h.b.state.Cleanup) != 0 {
		t.Fatalf("session from the lost create not stopped: cancels=%d unresolved=%+v cleanup=%v", h.cancels, h.b.state.Unresolved, h.b.state.Cleanup)
	}
}

func TestOMAIssueAttachments(t *testing.T) {
	h := setupOMA(t)
	base := h.b.multica.base
	h.issueAttachments = []map[string]any{
		{"id": "img-1", "filename": "shot.png", "content_type": "image/png", "size_bytes": 5, "download_url": base + "/download/img-1"},
		{"id": "doc-1", "filename": "notes.txt", "content_type": "text/plain", "size_bytes": 5, "download_url": base + "/download/doc-1"},
	}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.creates != 1 || len(h.uploadedFiles) != 2 {
		t.Fatalf("issue attachments not uploaded: creates=%d uploads=%v", h.creates, h.uploadedFiles)
	}
	if len(h.initialResources) != 1 {
		t.Fatalf("resources = %#v", h.initialResources)
	}
	var mounted bool
	for _, resource := range h.initialResources {
		if resource["type"] == "file" && resource["mount_path"] == "/uploads/task-1/notes.txt" {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("issue file not mounted: %#v", h.initialResources)
	}
	var image bool
	for _, block := range h.initialContent {
		if block["type"] == "image" {
			image = true
		}
	}
	if !image {
		t.Fatalf("issue image missing from message: %#v", h.initialContent)
	}
	if !strings.Contains(h.prompt, "Issue attachments:") ||
		!strings.Contains(h.prompt, "/mnt/session/uploads/task-1/notes.txt (mounted file)") ||
		!strings.Contains(h.prompt, "shot.png (image attached to this message)") {
		t.Fatalf("attachment prompt missing:\n%s", h.prompt)
	}
}

func TestClaudeRejectsIssueAttachments(t *testing.T) {
	h := setup(t)
	h.issueAttachments = []map[string]any{{"id": "doc-1", "filename": "notes.txt", "content_type": "text/plain", "size_bytes": 5}}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.failure == nil || !strings.Contains(fmt.Sprint(h.failure["error"]), "issue attachments are not supported") {
		t.Fatalf("failure = %#v", h.failure)
	}
}

func TestOMACustomToolResult(t *testing.T) {
	h := setupOMA(t)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.remoteStatus = "running"
	h.events = append(h.events,
		map[string]any{"id": "evt_tool", "type": "agent.custom_tool_use", "name": "Lookup", "input": map[string]any{}},
		map[string]any{"id": "evt_pause", "type": "session.status_idle", "stop_reason": map[string]any{"type": "requires_action", "event_ids": []string{"evt_tool"}}},
	)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.customToolResults) != 1 {
		t.Fatalf("custom tool result = %#v", h.customToolResults)
	}
	result := h.customToolResults[0]
	if result["type"] != "user.custom_tool_result" || result["custom_tool_use_id"] != "evt_tool" || result["is_error"] != true {
		t.Fatalf("result = %#v", result)
	}
	if h.complete != nil || h.failure != nil {
		t.Fatalf("run finalized while paused: complete=%v failure=%v", h.complete, h.failure)
	}
	if r := h.b.state.first(); r == nil || r.Phase != "running" || !r.CustomToolAnswered["evt_tool"] {
		t.Fatalf("run state = %#v", r)
	}
	h.finish("end_turn")
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.complete == nil {
		t.Fatal("run did not complete after the custom tool was answered")
	}
}

func TestOMACustomToolRequiresActionFailsForConfirmations(t *testing.T) {
	h := setupOMA(t)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.remoteStatus = "running"
	h.events = append(h.events,
		map[string]any{"id": "evt_confirm", "type": "agent.tool_use", "name": "bash", "input": map[string]any{}},
		map[string]any{"id": "evt_pause", "type": "session.status_idle", "stop_reason": map[string]any{"type": "requires_action", "event_ids": []string{"evt_confirm"}}},
	)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.failure == nil || !strings.Contains(fmt.Sprint(h.failure["error"]), "requires_action") {
		t.Fatalf("failure = %#v", h.failure)
	}
}

func TestOMAGitHubAppCredentialFallbackAndRefresh(t *testing.T) {
	h := setupOMA(t)
	calls := 0
	h.b.cfg.GitHubAppToken = func(context.Context, string, string) (string, int64, error) {
		calls++
		if calls == 1 {
			return "ghs_first", 77, nil
		}
		return "ghs_second", 77, nil
	}
	h.task.Agent.RuntimeConfig = json.RawMessage(`{"oma_agent_id":"agent_1","repository":{"url":"https://github.com/acme/app","ref":"refs/heads/main"}}`)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.initialResources) != 1 || h.initialResources[0]["authorization_token"] != "ghs_first" {
		t.Fatalf("resources = %#v", h.initialResources)
	}
	if r := h.b.state.first(); r == nil || r.CredentialIssuedAt.IsZero() {
		t.Fatalf("credential timestamp not recorded: %#v", r)
	}
	h.sessionResources = []map[string]any{{"id": "res_1", "type": "github_repository", "url": "https://github.com/acme/app"}}
	h.b.state.first().CredentialIssuedAt = time.Now().Add(-credentialRefreshInterval - time.Minute)
	h.remoteStatus = "running"
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.resourceUpdates) != 1 || h.resourceUpdates[0]["authorization_token"] != "ghs_second" {
		t.Fatalf("refresh = %#v", h.resourceUpdates)
	}
}

func TestOMAGitHubAppNotCoveredReportsMissingCredential(t *testing.T) {
	h := setupOMA(t)
	h.b.cfg.GitHubAppToken = func(context.Context, string, string) (string, int64, error) {
		return "", 0, cloudagent.ErrGitHubAppNotCovered
	}
	h.task.Agent.RuntimeConfig = json.RawMessage(`{"oma_agent_id":"agent_1","repository":{"url":"https://github.com/acme/app"}}`)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.failure == nil || !strings.Contains(fmt.Sprint(h.failure["error"]), "no GitHub credential configured") {
		t.Fatalf("failure = %#v", h.failure)
	}
}

func TestOMASquadLeaderDeliversCommentAndEvaluation(t *testing.T) {
	h := setupOMA(t)
	h.task.IsLeaderTask = true
	h.task.TriggerCommentID = "comment-1"
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.prompt, "Squad leader on OMA Managed Agents") ||
		!strings.Contains(h.prompt, "OUTCOME: action | no_action | failed") ||
		strings.Contains(h.prompt, "push a new branch") {
		t.Fatalf("leader prompt:\n%s", h.prompt)
	}
	h.remoteStatus = "running"
	h.events = append(h.events,
		map[string]any{"id": "evt_reply", "type": "agent.message", "content": []map[string]string{{"type": "text", "text": "OUTCOME: action\nREASON: delegate\nPlease pick this up."}}},
		map[string]any{"id": "evt_idle", "type": "session.status_idle", "stop_reason": map[string]any{"type": "end_turn"}},
	)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.issueComments) != 1 || h.issueComments[0]["content"] != "Please pick this up." || h.issueComments[0]["parent_id"] != "comment-1" {
		t.Fatalf("comments = %#v", h.issueComments)
	}
	if len(h.squadEvaluations) != 1 || h.squadEvaluations[0]["outcome"] != "action" || h.squadEvaluations[0]["reason"] != "delegate" {
		t.Fatalf("evaluations = %#v", h.squadEvaluations)
	}
	if h.complete == nil {
		t.Fatal("leader run did not complete")
	}
}

func TestClaudeRejectsSquadLeader(t *testing.T) {
	h := setup(t)
	h.task.IsLeaderTask = true
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.failure == nil || !strings.Contains(fmt.Sprint(h.failure["error"]), "squad leader runs") {
		t.Fatalf("failure = %#v", h.failure)
	}
}

func TestOMAQuickCreateDraftsAndCreatesIssue(t *testing.T) {
	h := setupOMA(t)
	h.task.IssueID = ""
	h.task.QuickCreatePrompt = "the login button is broken"
	h.task.QuickCreatePriority = "high"
	h.task.SquadID = "squad-1"
	h.task.Agent.ID = "agent-uuid"
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.prompt, "Draft the issue") || !strings.Contains(h.prompt, "the login button is broken") {
		t.Fatalf("quick-create prompt:\n%s", h.prompt)
	}
	if h.creates != 1 || h.sends != 0 {
		t.Fatalf("creates=%d sends=%d", h.creates, h.sends)
	}
	h.remoteStatus = "running"
	h.events = append(h.events,
		map[string]any{"id": "evt_reply", "type": "agent.message", "content": []map[string]string{{"type": "text", "text": `{"title":"Fix login","description":"The login button is broken.","priority":"low"}`}}},
		map[string]any{"id": "evt_idle", "type": "session.status_idle", "stop_reason": map[string]any{"type": "end_turn"}},
	)
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.createdIssues) != 1 {
		t.Fatalf("created issues = %#v", h.createdIssues)
	}
	created := h.createdIssues[0]
	if created["title"] != "Fix login" || created["priority"] != "high" || created["assignee_type"] != "squad" || created["assignee_id"] != "squad-1" || created["origin_type"] != "quick_create" {
		t.Fatalf("issue body = %#v", created)
	}
	if h.complete == nil || !strings.Contains(fmt.Sprint(h.complete["output"]), "MUL-42") {
		t.Fatalf("complete = %#v", h.complete)
	}
}

func TestClaudeRejectsQuickCreate(t *testing.T) {
	h := setup(t)
	h.task.IssueID = ""
	h.task.QuickCreatePrompt = "file a bug"
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.failure == nil || !strings.Contains(fmt.Sprint(h.failure["error"]), "supports existing issue runs only") {
		t.Fatalf("failure = %#v", h.failure)
	}
}

func TestOMAQuickCreateRejectsAttachments(t *testing.T) {
	h := setupOMA(t)
	h.task.IssueID = ""
	h.task.QuickCreatePrompt = "file a bug"
	h.task.QuickCreateAttachmentIDs = []string{"att-1"}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.failure == nil || !strings.Contains(fmt.Sprint(h.failure["error"]), "attachments are not supported") {
		t.Fatalf("failure = %#v", h.failure)
	}
}

func TestRepositoryResourceGitSuffixByProfile(t *testing.T) {
	oma, err := repositoryResourceFor(Repository{URL: "https://github.com/acme/app", Ref: "refs/heads/main"}, OMAProfile())
	if err != nil {
		t.Fatal(err)
	}
	if oma["url"] != "https://github.com/acme/app.git" {
		t.Fatalf("OMA repository URL = %q", oma["url"])
	}
	claude, err := repositoryResourceFor(Repository{URL: "https://github.com/acme/app", Ref: "refs/heads/main"}, ClaudeProfile())
	if err != nil {
		t.Fatal(err)
	}
	if claude["url"] != "https://github.com/acme/app" {
		t.Fatalf("Claude repository URL = %q", claude["url"])
	}
	if _, err := repositoryResourceFor(Repository{URL: "https://github.com/acme/app.git"}, OMAProfile()); err != nil {
		t.Fatalf("already-suffixed OMA repository rejected: %v", err)
	}
}

func TestUniqueAttachmentNameTruncatesOnRuneBoundary(t *testing.T) {
	name := strings.Repeat("é", 150)
	got := uniqueAttachmentName(name, "fallback", map[string]bool{})
	if len(got) > 200 || !utf8.ValidString(got) {
		t.Fatalf("truncated name is invalid: len=%d valid=%v", len(got), utf8.ValidString(got))
	}
}

func TestOMAIssueAttachmentsStartFreshSession(t *testing.T) {
	h := setupOMA(t)
	h.followUp()
	h.issueAttachments = []map[string]any{{
		"id": "doc-1", "filename": "notes.txt", "content_type": "text/plain", "size_bytes": 5,
		"download_url": h.b.multica.base + "/download/doc-1",
	}}
	if err := h.b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.creates != 1 || h.sends != 0 {
		t.Fatalf("issue attachments reused the prior session: creates=%d sends=%d", h.creates, h.sends)
	}
}
