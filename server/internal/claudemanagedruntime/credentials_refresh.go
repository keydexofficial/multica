package claudemanagedruntime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// errRefreshDeferred and errRefreshRefused classify a repository credential
// refresh: the session's provider asked us to retry later, or refused the
// update (including a repository that is no longer mounted), respectively.
var (
	errRefreshDeferred = errors.New("session resource update rate limited")
	errRefreshRefused  = errors.New("session refused the repository credential update")
)

// credentialRefreshInterval is how long a run waits before re-issuing a
// rotating GitHub App credential into its live session; GitHub installation
// tokens expire after one hour, so the refresh lands with margin to spare.
const credentialRefreshInterval = 45 * time.Minute

// sessionRepositoryResourceID finds the mounted-resource ID of one GitHub
// repository in a session, following the resource list's pagination.
func (b *Bridge) sessionRepositoryResourceID(ctx context.Context, sessionID, key string) (string, error) {
	page := ""
	for range 100 {
		q := url.Values{"limit": {"100"}}
		if b.profile.BetaQuery {
			q.Set("beta", "true")
		}
		if page != "" {
			q.Set("page", page)
		}
		var response struct {
			Data []struct {
				ID   string `json:"id"`
				Type string `json:"type"`
				URL  string `json:"url"`
			} `json:"data"`
			NextPage *string `json:"next_page"`
		}
		if err := b.anthropic.call(ctx, http.MethodGet, "/sessions/"+url.PathEscape(sessionID)+"/resources?"+q.Encode(), nil, &response); err != nil {
			return "", err
		}
		for _, resource := range response.Data {
			if resource.Type != "github_repository" {
				continue
			}
			if candidate, err := repositoryKey(resource.URL); err == nil && candidate == key {
				return resource.ID, nil
			}
		}
		if response.NextPage == nil || *response.NextPage == "" || *response.NextPage == page {
			return "", nil
		}
		page = *response.NextPage
	}
	return "", fmt.Errorf("session resource list did not settle")
}

// refreshRepositoryCredentials pushes fresh tokens into a session's mounted
// repositories. The add-resource endpoint only accepts files, so an existing
// repository is updated through its per-resource credential endpoint, matched
// by canonical repository URL so a token never lands on another resource.
func (b *Bridge) refreshRepositoryCredentials(ctx context.Context, sessionID string, resources []map[string]any) error {
	for i := range resources {
		raw, _ := resources[i]["url"].(string)
		key, err := repositoryKey(raw)
		if err != nil {
			return fmt.Errorf("refresh session repository credential: %w", err)
		}
		token, _ := resources[i]["authorization_token"].(string)
		resourceID, err := b.sessionRepositoryResourceID(ctx, sessionID, key)
		if err != nil {
			if rateLimited(err) {
				return errRefreshDeferred
			}
			if definitelyRejected(err) {
				return errRefreshRefused
			}
			return err
		}
		if resourceID == "" {
			return errRefreshRefused
		}
		err = b.anthropic.call(ctx, http.MethodPost, b.betaPath("/sessions/"+url.PathEscape(sessionID)+"/resources/"+url.PathEscape(resourceID)), map[string]string{"authorization_token": token}, nil)
		if err != nil {
			if rateLimited(err) {
				return errRefreshDeferred
			}
			if definitelyRejected(err) {
				return errRefreshRefused
			}
			return err
		}
	}
	return nil
}

// refreshRunningCredentials keeps a live session's short-lived GitHub token
// usable for runs longer than the token's lifetime: about 45 minutes after the
// last issuance it re-resolves the credentials and pushes them into the
// session. Only the issuance timestamp is journaled, never the token; its zero
// value disables the refresh (static credentials, or a session that refused the
// update for good). Transient failures back off and retry.
func (b *Bridge) refreshRunningCredentials(ctx context.Context, r *run) error {
	if r.CredentialIssuedAt.IsZero() || time.Now().Before(r.CredentialIssuedAt.Add(credentialRefreshInterval)) {
		return nil
	}
	resources, _, rotating, err := b.sessionResources(ctx, r.Resources)
	if err != nil {
		// The run is already executing; a failed refresh (for example a
		// just-revoked installation) must not kill it. Retry next tick.
		b.log.WarnContext(ctx, b.profile.DisplayName+" credential refresh failed", "task_id", r.TaskID, "error", err)
		r.CredentialIssuedAt = time.Now().Add(-credentialRefreshInterval + time.Minute)
		return b.persist()
	}
	if !rotating {
		// Credentials became manual mid-flight (for example an operator bound
		// a PAT); nothing expires anymore.
		r.CredentialIssuedAt = time.Time{}
		return b.persist()
	}
	if err := b.refreshRepositoryCredentials(ctx, r.SessionID, resources); err != nil {
		b.log.WarnContext(ctx, b.profile.DisplayName+" credential refresh push failed", "task_id", r.TaskID, "error", err)
		if errors.Is(err, errRefreshRefused) {
			r.CredentialIssuedAt = time.Time{}
		} else {
			r.CredentialIssuedAt = time.Now().Add(-credentialRefreshInterval + time.Minute)
		}
		return b.persist()
	}
	r.CredentialIssuedAt = time.Now()
	return b.persist()
}
