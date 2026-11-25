package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/claudemanagedruntime"
	"github.com/multica-ai/multica/server/internal/cloudagent"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type cloudAgentConfig struct {
	Name              string
	BaseURL           string
	Token             string
	EnvironmentID     string
	VaultIDs          []string
	GitHubTokens      map[string]string
	MulticaToken      string
	MaxConcurrency    int
	RunTimeoutMinutes int
}

type cloudAgentBridgeConfig struct {
	DaemonID          string
	MulticaURL        string
	MulticaToken      string
	WorkspaceID       string
	BaseURL           string
	Token             string
	EnvironmentID     string
	VaultIDs          []string
	GitHubTokens      map[string]string
	GitHubAppToken    func(context.Context, string, string) (string, int64, error)
	Name              string
	MaxConcurrency    int
	RunTimeoutMinutes int
}

type cloudAgentHooks struct {
	LoadState  func(context.Context) ([]byte, error)
	SaveState  func(context.Context, []byte) error
	OnReady    func(context.Context, string) error
	BeforeStep func(context.Context) (func(), error)
}

type cloudAgentRunner interface {
	Run(context.Context) error
}

type cloudAgentSpec struct {
	Descriptor          cloudagent.Descriptor
	FixedBaseURL        string
	DefaultConcurrency  int
	DefaultRunTimeout   int
	DecodeConfig        func([]byte) (cloudAgentConfig, error)
	EncodeConfig        func(cloudAgentConfig) ([]byte, error)
	ParseInput          func(*Handler, http.ResponseWriter, *http.Request, cloudAgentConfig) (cloudAgentConfig, bool)
	TargetKey           func(cloudAgentConfig) string
	BuildBridge         func(cloudAgentBridgeConfig, cloudAgentHooks) (cloudAgentRunner, error)
	CheckAccess         func(context.Context, string, string, string, []string) error
	CancelRemoteSession func(context.Context, string, string, string) error
	CancelUnconfirmed   func(context.Context, string, string, string, string, string, time.Time) error
	// GitHubAppToken optionally mints a short-lived repository token for a
	// workspace. The workspace ID is supplied by the supervisor.
	GitHubAppToken func(context.Context, string, string, string) (string, int64, error)
}

type cloudAgentView struct {
	Configured        bool     `json:"configured"`
	Available         bool     `json:"available"`
	Name              string   `json:"name"`
	BaseURL           string   `json:"base_url,omitempty"`
	EnvironmentID     string   `json:"environment_id"`
	HasToken          bool     `json:"has_token"`
	Repositories      []string `json:"repositories"`
	VaultIDs          []string `json:"vault_ids"`
	Enabled           bool     `json:"enabled"`
	Status            string   `json:"status"`
	LastError         string   `json:"last_error"`
	MaxConcurrency    int      `json:"max_concurrency"`
	RunTimeoutMinutes int      `json:"run_timeout_minutes"`
	ActiveRuns        int      `json:"active_runs"`
	QueuedRuns        int      `json:"queued_runs"`
}

func (c cloudAgentConfig) withDefaults(spec cloudAgentSpec) cloudAgentConfig {
	if c.MaxConcurrency == 0 {
		c.MaxConcurrency = spec.DefaultConcurrency
	}
	if c.RunTimeoutMinutes == 0 {
		c.RunTimeoutMinutes = spec.DefaultRunTimeout
	}
	return c
}

func (spec cloudAgentSpec) baseURL(cfg cloudAgentConfig) string {
	if spec.FixedBaseURL != "" {
		return spec.FixedBaseURL
	}
	return cfg.BaseURL
}

func (spec cloudAgentSpec) failureReason() string {
	return spec.Descriptor.Provider + "_execution"
}

func (h *Handler) readCloudAgentConfig(ctx context.Context, ws string, spec cloudAgentSpec) (cloudAgentConfig, bool, string, string, error) {
	var encrypted, status, lastError string
	var enabled bool
	err := h.DB.QueryRow(ctx, `SELECT config_encrypted, enabled, status, last_error FROM cloud_agent_connection WHERE provider=$2 AND workspace_id=$1`, ws, spec.Descriptor.Provider).Scan(&encrypted, &enabled, &status, &lastError)
	if err != nil {
		return cloudAgentConfig{}, false, "", "", err
	}
	raw, err := h.openVCSSecret(encrypted)
	if err != nil {
		return cloudAgentConfig{}, false, "", "", fmt.Errorf("cannot decrypt %s configuration", spec.Descriptor.ServiceName)
	}
	cfg, err := spec.DecodeConfig([]byte(raw))
	return cfg.withDefaults(spec), enabled, status, lastError, err
}

func (h *Handler) getCloudAgentConnection(w http.ResponseWriter, r *http.Request, spec cloudAgentSpec) {
	ws, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace_id")
	if !ok {
		return
	}
	view := cloudAgentView{Available: h.VCSSecretBox != nil, Repositories: []string{}, VaultIDs: []string{}}
	if !view.Available {
		writeJSON(w, 200, view)
		return
	}
	cfg, enabled, status, lastError, err := h.readCloudAgentConfig(r.Context(), uuidToString(ws), spec)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, 200, view)
		return
	}
	if err != nil {
		writeError(w, 500, "cannot read "+spec.Descriptor.ServiceName+" configuration")
		return
	}
	daemonID := spec.Descriptor.DaemonPrefix + uuidToString(ws)
	var online bool
	_ = h.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM agent_runtime WHERE workspace_id=$1 AND provider=$3 AND daemon_id=$2 AND last_seen_at>now()-interval '45 seconds' AND status='online')`, ws, daemonID, spec.Descriptor.Provider).Scan(&online)
	if status == "online" && enabled && !online {
		status = "offline"
	}
	view.Configured = true
	view.Name = cfg.Name
	view.BaseURL = spec.baseURL(cfg)
	view.EnvironmentID = cfg.EnvironmentID
	view.HasToken = cfg.Token != ""
	view.Enabled = enabled
	view.Status = status
	view.LastError = lastError
	view.MaxConcurrency = cfg.MaxConcurrency
	view.RunTimeoutMinutes = cfg.RunTimeoutMinutes
	if cfg.VaultIDs != nil {
		view.VaultIDs = cfg.VaultIDs
	}
	_ = h.DB.QueryRow(r.Context(), `SELECT count(*) FILTER (WHERE t.status IN ('dispatched','running')), count(*) FILTER (WHERE t.status='queued') FROM agent_task_queue t JOIN agent_runtime rt ON rt.id=t.runtime_id WHERE rt.workspace_id=$1 AND rt.provider=$4 AND rt.daemon_id=$2 AND t.status IN ('queued','dispatched','running')`, ws, daemonID, spec.Descriptor.Provider).Scan(&view.ActiveRuns, &view.QueuedRuns)
	for repo := range cfg.GitHubTokens {
		view.Repositories = append(view.Repositories, repo)
	}
	sort.Strings(view.Repositories)
	writeJSON(w, 200, view)
}

func (h *Handler) cloudAgentInput(w http.ResponseWriter, r *http.Request, ws string, spec cloudAgentSpec) (cloudAgentConfig, bool) {
	if h.VCSSecretBox == nil {
		writeError(w, 503, spec.Descriptor.ServiceName+" requires MULTICA_VCS_SECRET_KEY on the server")
		return cloudAgentConfig{}, false
	}
	existing, _, _, _, err := h.readCloudAgentConfig(r.Context(), ws, spec)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 500, "cannot read "+spec.Descriptor.ServiceName+" configuration")
		return cloudAgentConfig{}, false
	}
	return spec.ParseInput(h, w, r, existing)
}

func (h *Handler) checkCloudAgentConnection(w http.ResponseWriter, r *http.Request, spec cloudAgentSpec) {
	ws, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace_id")
	if !ok {
		return
	}
	cfg, ok := h.cloudAgentInput(w, r, uuidToString(ws), spec)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := spec.CheckAccess(ctx, spec.baseURL(cfg), cfg.Token, cfg.EnvironmentID, cfg.VaultIDs); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (h *Handler) saveCloudAgentConnection(w http.ResponseWriter, r *http.Request, resume bool, spec cloudAgentSpec) {
	ws, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace_id")
	if !ok {
		return
	}
	user, ok := requireUserID(w, r)
	if !ok {
		return
	}
	cfg, ok := h.cloudAgentInput(w, r, uuidToString(ws), spec)
	if !ok {
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "cannot save "+spec.Descriptor.ServiceName+" configuration")
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1, $2))`, uuidToString(ws), spec.Descriptor.LockSeed); err != nil {
		writeError(w, 500, "cannot lock "+spec.Descriptor.ServiceName+" configuration")
		return
	}
	var busy bool
	if err := tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM cloud_agent_connection WHERE provider=$2 AND workspace_id=$1 AND busy_until>now())`, ws, spec.Descriptor.Provider).Scan(&busy); err != nil {
		writeError(w, 500, "cannot check "+spec.Descriptor.ServiceName+" connection")
		return
	}
	if busy {
		writeError(w, 409, spec.Descriptor.ServiceName+" is synchronizing; retry shortly")
		return
	}
	var journal, previousConfig string
	if err := tx.QueryRow(r.Context(), `SELECT journal_encrypted,config_encrypted FROM cloud_agent_connection WHERE provider=$2 AND workspace_id=$1`, ws, spec.Descriptor.Provider).Scan(&journal, &previousConfig); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 500, "cannot read execution state")
		return
	}
	if journal != "" {
		raw, err := h.openVCSSecret(journal)
		if err != nil {
			writeError(w, 500, "cannot read execution state")
			return
		}
		var state struct {
			Runs       []json.RawMessage `json:"runs"`
			Cleanup    []string          `json:"cleanup"`
			Unresolved []json.RawMessage `json:"unresolved"`
		}
		if json.Unmarshal([]byte(raw), &state) != nil {
			writeError(w, 500, "cannot read execution state")
			return
		}
		if len(state.Runs) > 0 {
			writeError(w, 409, "wait for "+spec.Descriptor.ServiceName+" execution cleanup before changing configuration")
			return
		}
		if len(state.Cleanup) > 0 || len(state.Unresolved) > 0 {
			rawConfig, err := h.openVCSSecret(previousConfig)
			var previous cloudAgentConfig
			if err != nil {
				writeError(w, 500, "cannot read "+spec.Descriptor.ServiceName+" configuration")
				return
			}
			previous, err = spec.DecodeConfig([]byte(rawConfig))
			if err != nil || spec.TargetKey(previous) != spec.TargetKey(cfg) {
				writeError(w, 409, "wait for "+spec.Descriptor.ServiceName+" execution cleanup before changing environment")
				return
			}
		} else {
			journal = ""
		}
	}
	var revokedHash string
	_ = tx.QueryRow(r.Context(), `SELECT d.token_hash FROM daemon_token d JOIN cloud_agent_connection c ON c.token_id=d.id AND c.provider=$2 WHERE c.workspace_id=$1`, ws, spec.Descriptor.Provider).Scan(&revokedHash)
	daemonID := spec.Descriptor.DaemonPrefix + uuidToString(ws)
	var active bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM agent_task_queue t JOIN agent_runtime rt ON rt.id=t.runtime_id WHERE rt.workspace_id=$1 AND rt.provider=$3 AND rt.daemon_id=$2 AND t.status IN ('running','dispatched'))`, ws, daemonID, spec.Descriptor.Provider).Scan(&active)
	if err != nil {
		writeError(w, 500, "cannot check active "+spec.Descriptor.ServiceName+" tasks")
		return
	}
	if active {
		writeError(w, 409, "wait for active "+spec.Descriptor.ServiceName+" tasks to finish before changing configuration")
		return
	}
	rawToken, err := auth.GenerateDaemonToken()
	if err != nil {
		writeError(w, 500, "cannot generate service credential")
		return
	}
	q := h.Queries.WithTx(tx)
	pat, err := q.CreateDaemonToken(r.Context(), db.CreateDaemonTokenParams{WorkspaceID: ws, DaemonID: daemonID, TokenHash: auth.HashToken(rawToken), ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}})
	if err != nil {
		writeError(w, 500, "cannot create service credential")
		return
	}
	cfg.MulticaToken = rawToken
	raw, err := spec.EncodeConfig(cfg)
	if err != nil {
		writeError(w, 500, "cannot encode "+spec.Descriptor.ServiceName+" configuration")
		return
	}
	encrypted, err := h.sealVCSSecret(string(raw))
	if err != nil {
		writeError(w, 500, "cannot encrypt "+spec.Descriptor.ServiceName+" configuration")
		return
	}
	_, err = tx.Exec(r.Context(), `DELETE FROM daemon_token WHERE id IN (SELECT token_id FROM cloud_agent_connection WHERE provider=$2 AND workspace_id=$1)`, ws, spec.Descriptor.Provider)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO cloud_agent_connection(workspace_id,provider,owner_id,token_id,config_encrypted) VALUES($1,$2,$3,$4,$5) ON CONFLICT(workspace_id,provider) DO UPDATE SET owner_id=excluded.owner_id,token_id=excluded.token_id,config_encrypted=excluded.config_encrypted,enabled=($6 OR cloud_agent_connection.enabled),journal_encrypted=$7,revision=cloud_agent_connection.revision+1,status=CASE WHEN $6 OR cloud_agent_connection.enabled THEN 'starting' ELSE cloud_agent_connection.status END,last_error='',updated_at=now()`, ws, spec.Descriptor.Provider, parseUUID(user), pat.ID, encrypted, resume, journal)
	}
	if err != nil || tx.Commit(r.Context()) != nil {
		writeError(w, 500, "cannot save "+spec.Descriptor.ServiceName+" configuration")
		return
	}
	h.DaemonTokenCache.Invalidate(r.Context(), revokedHash)
	h.getCloudAgentConnection(w, r, spec)
}

func (h *Handler) RunCloudAgentConnections(ctx context.Context, pool *pgxpool.Pool, serverURL string, spec cloudAgentSpec) {
	if h.VCSSecretBox == nil {
		return
	}
	for ctx.Err() == nil {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.WarnContext(ctx, spec.Descriptor.ServiceName+" supervisor database connection unavailable", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
		var locked bool
		err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(71243, $1::int)`, spec.Descriptor.SupervisorLockKey).Scan(&locked)
		if err == nil && locked {
			h.runCloudAgentSupervisor(ctx, conn, serverURL, spec)
		}
		if locked {
			_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(71243,$1::int)`, spec.Descriptor.SupervisorLockKey)
		}
		conn.Release()
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

type managedCloudAgentRun struct {
	revision int64
	cancel   context.CancelFunc
	done     chan error
}

func (h *Handler) runCloudAgentSupervisor(ctx context.Context, conn *pgxpool.Conn, serverURL string, spec cloudAgentSpec) {
	runs := map[string]managedCloudAgentRun{}
	retryAt := map[string]time.Time{}
	failures := map[string]int{}
	defer func() {
		for _, run := range runs {
			run.cancel()
			<-run.done
		}
	}()
	for ctx.Err() == nil {
		_, _ = conn.Exec(ctx, `UPDATE daemon_token d SET expires_at=now()+interval '1 hour' FROM cloud_agent_connection c JOIN member m ON m.workspace_id=c.workspace_id AND m.user_id=c.owner_id WHERE c.provider=$1 AND d.id=c.token_id AND c.enabled=true AND m.role IN ('owner','admin') AND d.expires_at<now()+interval '30 minutes'`, spec.Descriptor.Provider)
		revoked, revokeErr := conn.Query(ctx, `DELETE FROM daemon_token d USING cloud_agent_connection c WHERE c.provider=$1 AND d.id=c.token_id AND (NOT c.enabled OR NOT EXISTS(SELECT 1 FROM member m WHERE m.workspace_id=c.workspace_id AND m.user_id=c.owner_id AND m.role IN ('owner','admin'))) RETURNING d.token_hash`, spec.Descriptor.Provider)
		if revokeErr != nil {
			return
		}
		for revoked.Next() {
			var hash string
			if revoked.Scan(&hash) == nil {
				h.DaemonTokenCache.Invalidate(ctx, hash)
			}
		}
		revoked.Close()
		rows, err := conn.Query(ctx, `SELECT c.workspace_id::text,c.revision FROM cloud_agent_connection c JOIN workspace w ON w.id=c.workspace_id JOIN member m ON m.workspace_id=c.workspace_id AND m.user_id=c.owner_id WHERE c.provider=$1 AND c.enabled=true AND m.role IN ('owner','admin')`, spec.Descriptor.Provider)
		if err != nil {
			return
		}
		desired := map[string]int64{}
		for rows.Next() {
			var ws string
			var rev int64
			if rows.Scan(&ws, &rev) != nil {
				rows.Close()
				return
			}
			desired[ws] = rev
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return
		}
		retryLater := map[string]bool{}
		for ws, run := range runs {
			if desired[ws] != run.revision {
				run.cancel()
				<-run.done
				delete(runs, ws)
				continue
			}
			select {
			case err := <-run.done:
				message := "execution service stopped; retrying"
				if err != nil {
					slog.WarnContext(ctx, spec.Descriptor.ServiceName+" bridge stopped", "workspace_id", ws, "error", err)
					message = "remote execution unavailable; retrying"
					if errors.Is(err, claudemanagedruntime.ErrCleanupUnauthorized) {
						message = claudemanagedruntime.ErrCleanupUnauthorized.Error()
					}
				}
				_, _ = conn.Exec(ctx, `UPDATE cloud_agent_connection SET status='error',last_error=$3 WHERE provider=$2 AND workspace_id=$1`, ws, spec.Descriptor.Provider, message)
				delete(runs, ws)
				retryLater[ws] = true
				failures[ws]++
				delay := 10 * time.Second << min(failures[ws]-1, 5)
				retryAt[ws] = time.Now().Add(delay)
			default:
			}
		}
		h.cleanupInactiveCloudAgent(ctx, spec)
		for ws, rev := range desired {
			if retryLater[ws] || time.Now().Before(retryAt[ws]) {
				continue
			}
			if _, ok := runs[ws]; ok {
				continue
			}
			cfg, _, _, _, err := h.readCloudAgentConfig(ctx, ws, spec)
			if err != nil {
				continue
			}
			bridge, err := spec.BuildBridge(cloudAgentBridgeConfig{
				DaemonID:      spec.Descriptor.DaemonPrefix + ws,
				MulticaURL:    serverURL,
				MulticaToken:  cfg.MulticaToken,
				WorkspaceID:   ws,
				BaseURL:       spec.baseURL(cfg),
				Token:         cfg.Token,
				EnvironmentID: cfg.EnvironmentID,
				VaultIDs:      cfg.VaultIDs,
				GitHubTokens:  cfg.GitHubTokens,
				GitHubAppToken: func(tokenCtx context.Context, owner, repo string) (string, int64, error) {
					if spec.GitHubAppToken == nil {
						return "", 0, cloudagent.ErrGitHubAppNotCovered
					}
					return spec.GitHubAppToken(tokenCtx, ws, owner, repo)
				},
				Name:              cfg.Name,
				MaxConcurrency:    cfg.MaxConcurrency,
				RunTimeoutMinutes: cfg.RunTimeoutMinutes,
			}, cloudAgentHooks{
				LoadState: func(loadCtx context.Context) ([]byte, error) {
					var encrypted string
					if err := h.DB.QueryRow(loadCtx, `SELECT journal_encrypted FROM cloud_agent_connection WHERE provider=$2 AND workspace_id=$1`, ws, spec.Descriptor.Provider).Scan(&encrypted); err != nil {
						return nil, err
					}
					if encrypted == "" {
						return nil, nil
					}
					raw, err := h.openVCSSecret(encrypted)
					return []byte(raw), err
				},
				SaveState: func(saveCtx context.Context, raw []byte) error {
					encrypted, err := h.sealVCSSecret(string(raw))
					if err != nil {
						return err
					}
					_, err = h.DB.Exec(saveCtx, `UPDATE cloud_agent_connection SET journal_encrypted=$3 WHERE provider=$2 AND workspace_id=$1`, ws, spec.Descriptor.Provider, encrypted)
					return err
				},
				OnReady: func(readyCtx context.Context, runtimeID string) error {
					return h.cloudAgentReady(readyCtx, ws, rev, runtimeID, spec)
				},
				BeforeStep: func(stepCtx context.Context) (func(), error) {
					return h.acquireCloudAgentStep(stepCtx, ws, rev, spec)
				},
			})
			if err != nil {
				_, _ = conn.Exec(ctx, `UPDATE cloud_agent_connection SET status='error',last_error='invalid execution configuration' WHERE provider=$2 AND workspace_id=$1`, ws, spec.Descriptor.Provider)
				continue
			}
			runCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			runs[ws] = managedCloudAgentRun{revision: rev, cancel: cancel, done: done}
			_, _ = conn.Exec(ctx, `UPDATE cloud_agent_connection SET status='starting',last_error='' WHERE provider=$2 AND workspace_id=$1`, ws, spec.Descriptor.Provider)
			go func() { done <- bridge.Run(runCtx) }()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (h *Handler) stopCloudAgentConnection(w http.ResponseWriter, r *http.Request, spec cloudAgentSpec) {
	ws, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace_id")
	if !ok {
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "cannot stop "+spec.Descriptor.ServiceName+" runtime")
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,$2))`, uuidToString(ws), spec.Descriptor.LockSeed); err != nil {
		writeError(w, 500, "cannot lock "+spec.Descriptor.ServiceName+" configuration")
		return
	}
	var busy bool
	if err := tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM cloud_agent_connection WHERE provider=$2 AND workspace_id=$1 AND busy_until>now())`, ws, spec.Descriptor.Provider).Scan(&busy); err != nil {
		writeError(w, 500, "cannot check "+spec.Descriptor.ServiceName+" connection")
		return
	}
	if busy {
		writeError(w, 409, spec.Descriptor.ServiceName+" is synchronizing; retry shortly")
		return
	}
	var revokedHash string
	_ = tx.QueryRow(r.Context(), `SELECT d.token_hash FROM daemon_token d JOIN cloud_agent_connection c ON c.token_id=d.id AND c.provider=$2 WHERE c.workspace_id=$1`, ws, spec.Descriptor.Provider).Scan(&revokedHash)
	daemonID := spec.Descriptor.DaemonPrefix + uuidToString(ws)
	var active bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM agent_task_queue t JOIN agent_runtime rt ON rt.id=t.runtime_id WHERE rt.workspace_id=$1 AND rt.provider=$3 AND rt.daemon_id=$2 AND t.status IN ('running','dispatched'))`, ws, daemonID, spec.Descriptor.Provider).Scan(&active)
	if err != nil {
		writeError(w, 500, "cannot check active "+spec.Descriptor.ServiceName+" tasks")
		return
	}
	if active {
		writeError(w, 409, "wait for or cancel active "+spec.Descriptor.ServiceName+" tasks before stopping")
		return
	}
	if _, err = tx.Exec(r.Context(), `DELETE FROM daemon_token WHERE id IN (SELECT token_id FROM cloud_agent_connection WHERE provider=$2 AND workspace_id=$1)`, ws, spec.Descriptor.Provider); err != nil {
		writeError(w, 500, "cannot revoke runtime credential")
		return
	}
	_, err = tx.Exec(r.Context(), `UPDATE cloud_agent_connection SET enabled=false,revision=revision+1,status='stopped',last_error='' WHERE provider=$2 AND workspace_id=$1`, ws, spec.Descriptor.Provider)
	if err != nil || tx.Commit(r.Context()) != nil {
		writeError(w, 500, "cannot stop "+spec.Descriptor.ServiceName+" runtime")
		return
	}
	h.DaemonTokenCache.Invalidate(r.Context(), revokedHash)
	h.getCloudAgentConnection(w, r, spec)
}

func (h *Handler) cloudAgentReady(ctx context.Context, ws string, revision int64, runtimeID string, spec cloudAgentSpec) error {
	if _, err := h.DB.Exec(ctx, `UPDATE agent_runtime SET visibility='public',owner_id=(SELECT owner_id FROM cloud_agent_connection WHERE provider=$3 AND workspace_id=$2) WHERE id=$1 AND workspace_id=$2`, runtimeID, ws, spec.Descriptor.Provider); err != nil {
		return fmt.Errorf("cannot publish managed runtime")
	}
	_, err := h.DB.Exec(ctx, `UPDATE cloud_agent_connection SET status='online',last_error='' WHERE provider=$3 AND workspace_id=$1 AND revision=$2`, ws, revision, spec.Descriptor.Provider)
	return err
}

func (h *Handler) acquireCloudAgentStep(stepCtx context.Context, ws string, rev int64, spec cloudAgentSpec) (func(), error) {
	tx, err := h.TxStarter.Begin(stepCtx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(stepCtx)
	release := func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = h.DB.Exec(releaseCtx, `UPDATE cloud_agent_connection SET busy_until=NULL WHERE provider=$3 AND workspace_id=$1 AND revision=$2`, ws, rev, spec.Descriptor.Provider)
	}
	if _, err = tx.Exec(stepCtx, `SELECT pg_advisory_xact_lock(hashtextextended($1,$2))`, ws, spec.Descriptor.LockSeed); err != nil {
		return nil, err
	}
	var valid bool
	err = tx.QueryRow(stepCtx, `SELECT EXISTS(SELECT 1 FROM cloud_agent_connection c JOIN member m ON m.workspace_id=c.workspace_id AND m.user_id=c.owner_id WHERE c.provider=$3 AND c.workspace_id=$1 AND c.revision=$2 AND c.enabled=true AND m.role IN ('owner','admin') AND (c.busy_until IS NULL OR c.busy_until<now()))`, ws, rev, spec.Descriptor.Provider).Scan(&valid)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, context.Canceled
	}
	if _, err := tx.Exec(stepCtx, `UPDATE cloud_agent_connection SET busy_until=now()+interval '45 seconds' WHERE provider=$3 AND workspace_id=$1 AND revision=$2`, ws, rev, spec.Descriptor.Provider); err != nil {
		return nil, err
	}
	if err := tx.Commit(stepCtx); err != nil {
		return nil, err
	}
	return release, nil
}

func (h *Handler) disconnectCloudAgentConnection(w http.ResponseWriter, r *http.Request, spec cloudAgentSpec) {
	ws, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace_id")
	if !ok {
		return
	}
	user, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var req disconnectQoderRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	expected, ok := parseExpectedActiveAgentIDs(req.ExpectedActiveAgentIDs)
	if !ok {
		writeError(w, http.StatusBadRequest, "expected_active_agent_ids must be a list of valid UUIDs")
		return
	}
	wsID := uuidToString(ws)
	if !h.stopCloudAgentForDisconnect(w, r, ws, expected, spec) {
		return
	}
	if err := h.cleanupCloudAgentJournal(r.Context(), wsID, spec); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn(spec.Descriptor.ServiceName+" disconnect waiting on remote cleanup", "workspace_id", wsID, "error", err)
		writeError(w, http.StatusConflict, spec.Descriptor.ServiceName+" is stopped and still cancelling remote sessions; retry shortly")
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot disconnect "+spec.Descriptor.ServiceName)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,$2))`, wsID, spec.Descriptor.LockSeed); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot lock "+spec.Descriptor.ServiceName+" configuration")
		return
	}
	qtx := h.Queries.WithTx(tx)
	runtimeIDs, refused := h.checkCloudAgentDisconnectPlan(w, r.Context(), tx, qtx, ws, expected, spec)
	if refused {
		return
	}
	teardowns := make([]service.RuntimeTeardownResult, 0, len(runtimeIDs))
	for _, runtimeID := range runtimeIDs {
		teardown, err := service.TeardownRuntime(r.Context(), qtx, runtimeID, service.RuntimeTeardownOptions{CancelNonTerminalTasks: true})
		if err != nil {
			if errors.Is(err, service.ErrRuntimeNotDrained) {
				writeError(w, http.StatusConflict, spec.Descriptor.ServiceName+" still has tasks in flight; retry in a moment")
				return
			}
			slog.Error(spec.Descriptor.ServiceName+" disconnect teardown failed", "runtime_id", uuidToString(runtimeID), "error", err)
			writeError(w, http.StatusInternalServerError, "cannot disconnect "+spec.Descriptor.ServiceName)
			return
		}
		if err := qtx.DeleteAgentRuntime(r.Context(), runtimeID); err != nil {
			writeError(w, http.StatusInternalServerError, "cannot disconnect "+spec.Descriptor.ServiceName)
			return
		}
		teardowns = append(teardowns, teardown)
	}
	tag, err := tx.Exec(r.Context(), `DELETE FROM cloud_agent_connection WHERE provider=$2 AND workspace_id=$1 AND journal_encrypted=''`, ws, spec.Descriptor.Provider)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot disconnect "+spec.Descriptor.ServiceName)
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusConflict, spec.Descriptor.ServiceName+" is stopped and still cancelling remote sessions; retry shortly")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot disconnect "+spec.Descriptor.ServiceName)
		return
	}
	for i, runtimeID := range runtimeIDs {
		h.NotifyRuntimeGone(uuidToString(runtimeID))
		h.publishRuntimeTeardown(r.Context(), teardowns[i], wsID, user)
	}
	slog.Info(spec.Descriptor.Provider+" disconnected", "workspace_id", wsID, "disconnected_by", user, "runtimes_removed", len(runtimeIDs))
	h.getCloudAgentConnection(w, r, spec)
}

func (h *Handler) stopCloudAgentForDisconnect(w http.ResponseWriter, r *http.Request, ws pgtype.UUID, expected map[string]struct{}, spec cloudAgentSpec) bool {
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot disconnect "+spec.Descriptor.ServiceName)
		return false
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,$2))`, uuidToString(ws), spec.Descriptor.LockSeed); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot lock "+spec.Descriptor.ServiceName+" configuration")
		return false
	}
	var exists, busy bool
	if err := tx.QueryRow(r.Context(), `SELECT true, COALESCE(busy_until>now(),false) FROM cloud_agent_connection WHERE provider=$2 AND workspace_id=$1`, ws, spec.Descriptor.Provider).Scan(&exists, &busy); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, spec.Descriptor.ServiceName+" is not connected")
			return false
		}
		writeError(w, http.StatusInternalServerError, "cannot check "+spec.Descriptor.ServiceName+" connection")
		return false
	}
	if busy {
		writeError(w, http.StatusConflict, spec.Descriptor.ServiceName+" is synchronizing; retry shortly")
		return false
	}
	if _, refused := h.checkCloudAgentDisconnectPlan(w, r.Context(), tx, h.Queries.WithTx(tx), ws, expected, spec); refused {
		return false
	}
	var revokedHash string
	_ = tx.QueryRow(r.Context(), `SELECT d.token_hash FROM daemon_token d JOIN cloud_agent_connection c ON c.token_id=d.id AND c.provider=$2 WHERE c.workspace_id=$1`, ws, spec.Descriptor.Provider).Scan(&revokedHash)
	if _, err = tx.Exec(r.Context(), `DELETE FROM daemon_token WHERE id IN (SELECT token_id FROM cloud_agent_connection WHERE provider=$2 AND workspace_id=$1)`, ws, spec.Descriptor.Provider); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot revoke runtime credential")
		return false
	}
	if _, err = tx.Exec(r.Context(), `UPDATE cloud_agent_connection SET enabled=false,revision=revision+1,status='stopped',last_error='' WHERE provider=$2 AND workspace_id=$1`, ws, spec.Descriptor.Provider); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot stop "+spec.Descriptor.ServiceName+" runtime")
		return false
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot stop "+spec.Descriptor.ServiceName+" runtime")
		return false
	}
	if revokedHash != "" {
		h.DaemonTokenCache.Invalidate(r.Context(), revokedHash)
	}
	return true
}

func (h *Handler) checkCloudAgentDisconnectPlan(w http.ResponseWriter, ctx context.Context, tx pgx.Tx, qtx *db.Queries, ws pgtype.UUID, expected map[string]struct{}, spec cloudAgentSpec) ([]pgtype.UUID, bool) {
	daemonID := spec.Descriptor.DaemonPrefix + uuidToString(ws)
	rows, err := tx.Query(ctx, `SELECT id FROM agent_runtime WHERE workspace_id=$1 AND provider=$3 AND daemon_id=$2 ORDER BY id FOR UPDATE`, ws, daemonID, spec.Descriptor.Provider)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot load "+spec.Descriptor.ServiceName+" runtime")
		return nil, true
	}
	runtimeIDs, err := pgx.CollectRows(rows, pgx.RowTo[pgtype.UUID])
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot load "+spec.Descriptor.ServiceName+" runtime")
		return nil, true
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_task_queue t JOIN agent_runtime rt ON rt.id=t.runtime_id WHERE rt.workspace_id=$1 AND rt.provider=$3 AND rt.daemon_id=$2 AND t.status IN ('running','dispatched'))`, ws, daemonID, spec.Descriptor.Provider).Scan(&active); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot check active "+spec.Descriptor.ServiceName+" tasks")
		return nil, true
	}
	if active {
		writeError(w, http.StatusConflict, "wait for or cancel active "+spec.Descriptor.ServiceName+" tasks before disconnecting")
		return nil, true
	}
	var agents []db.Agent
	for _, runtimeID := range runtimeIDs {
		bound, err := qtx.ListActiveAgentsByRuntimeForUpdate(ctx, runtimeID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "cannot check "+spec.Descriptor.ServiceName+" agents")
			return nil, true
		}
		agents = append(agents, bound...)
	}
	if !activeAgentSetMatches(agents, expected) {
		body := h.runtimeHasActiveAgentsResponse(agents)
		body["code"] = "runtime_delete_plan_changed"
		body["error"] = "the agents on " + spec.Descriptor.ServiceName + " changed; please review and confirm again."
		writeJSON(w, http.StatusConflict, body)
		return nil, true
	}
	return runtimeIDs, false
}

func (h *Handler) cleanupInactiveCloudAgent(ctx context.Context, spec cloudAgentSpec) {
	rows, err := h.DB.Query(ctx, `SELECT c.workspace_id::text FROM cloud_agent_connection c WHERE c.provider=$1 AND c.journal_encrypted<>'' AND (NOT c.enabled OR NOT EXISTS(SELECT 1 FROM member m WHERE m.workspace_id=c.workspace_id AND m.user_id=c.owner_id AND m.role IN ('owner','admin')))`, spec.Descriptor.Provider)
	if err != nil {
		return
	}
	var workspaces []string
	for rows.Next() {
		var ws string
		if rows.Scan(&ws) == nil {
			workspaces = append(workspaces, ws)
		}
	}
	rows.Close()
	for _, ws := range workspaces {
		cleanupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := h.cleanupCloudAgentJournal(cleanupCtx, ws, spec); errors.Is(err, claudemanagedruntime.ErrCleanupUnauthorized) {
			_, _ = h.DB.Exec(cleanupCtx, `UPDATE cloud_agent_connection SET status='error',last_error=$3 WHERE provider=$2 AND workspace_id=$1`, ws, spec.Descriptor.Provider, claudemanagedruntime.ErrCleanupUnauthorized.Error())
		}
		cancel()
	}
}

func (h *Handler) cleanupCloudAgentJournal(ctx context.Context, ws string, spec cloudAgentSpec) error {
	cfg, _, _, _, err := h.readCloudAgentConfig(ctx, ws, spec)
	if err != nil {
		return err
	}
	var encrypted string
	if err = h.DB.QueryRow(ctx, `SELECT journal_encrypted FROM cloud_agent_connection WHERE provider=$2 AND workspace_id=$1`, ws, spec.Descriptor.Provider).Scan(&encrypted); err != nil {
		return err
	}
	if encrypted == "" {
		return nil
	}
	raw, err := h.openVCSSecret(encrypted)
	if err != nil {
		return err
	}
	var state struct {
		Runs []struct {
			TaskID          string    `json:"task_id"`
			SessionID       string    `json:"session_id"`
			AgentID         string    `json:"agent_id"`
			CreateAttemptAt time.Time `json:"create_attempt_at"`
		} `json:"runs"`
		Cleanup    []string `json:"cleanup"`
		Unresolved []struct {
			TaskID          string    `json:"task_id"`
			AgentID         string    `json:"agent_id"`
			CreateAttemptAt time.Time `json:"create_attempt_at"`
		} `json:"unresolved"`
	}
	if err = json.Unmarshal([]byte(raw), &state); err != nil {
		return err
	}
	sessions := state.Cleanup
	type unconfirmed struct {
		taskID, agentID string
		attemptAt       time.Time
	}
	var unconfirmedCreates []unconfirmed
	for _, u := range state.Unresolved {
		unconfirmedCreates = append(unconfirmedCreates, unconfirmed{u.TaskID, u.AgentID, u.CreateAttemptAt})
	}
	for _, run := range state.Runs {
		if run.SessionID != "" {
			sessions = append(sessions, run.SessionID)
		} else if !run.CreateAttemptAt.IsZero() {
			unconfirmedCreates = append(unconfirmedCreates, unconfirmed{run.TaskID, run.AgentID, run.CreateAttemptAt})
		}
		id, err := util.ParseUUID(run.TaskID)
		if err != nil {
			return err
		}
		if _, err = h.TaskService.FailTask(ctx, id, spec.Descriptor.ServiceName+" connection stopped", run.SessionID, "", "", spec.failureReason(), false, "", ""); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	for _, u := range unconfirmedCreates {
		if err = spec.CancelUnconfirmed(ctx, spec.baseURL(cfg), cfg.Token, ws, u.agentID, u.taskID, u.attemptAt); err != nil {
			return fmt.Errorf("remote cleanup pending: %w", err)
		}
	}
	for _, id := range sessions {
		if err = spec.CancelRemoteSession(ctx, spec.baseURL(cfg), cfg.Token, id); err != nil {
			return fmt.Errorf("remote cleanup pending: %w", err)
		}
	}
	_, err = h.DB.Exec(ctx, `UPDATE cloud_agent_connection SET journal_encrypted='',status='stopped',last_error='' WHERE provider=$2 AND workspace_id=$1`, ws, spec.Descriptor.Provider)
	return err
}

func (h *Handler) prepareCloudAgentWorkspaceDeletion(ctx context.Context, ws string, spec cloudAgentSpec) error {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,$2))`, ws, spec.Descriptor.LockSeed); err != nil {
		return err
	}
	var busy bool
	err = tx.QueryRow(ctx, `UPDATE cloud_agent_connection SET enabled=false,status='stopped' WHERE provider=$2 AND workspace_id=$1 RETURNING COALESCE(busy_until>now(),false)`, ws, spec.Descriptor.Provider).Scan(&busy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if busy {
		return fmt.Errorf("remote step still active")
	}
	return h.cleanupCloudAgentJournal(ctx, ws, spec)
}
