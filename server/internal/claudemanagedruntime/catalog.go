package claudemanagedruntime

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// CatalogItem is a Claude agent or environment as Multica shows it: only its
// identity, never its configuration, which may contain secrets.
type CatalogItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

const maxCatalogPages = 10

func ListCatalog(ctx context.Context, baseURL, token, kind string) ([]CatalogItem, error) {
	return ListCatalogWithProfile(ctx, baseURL, token, kind, ClaudeProfile())
}

func ListCatalogWithProfile(ctx context.Context, baseURL, token, kind string, profile Profile) ([]CatalogItem, error) {
	profile = normalizeProfile(profile)
	prefix := map[string]string{"agents": "agent_", "environments": "env_"}[kind]
	if prefix == "" {
		return nil, fmt.Errorf("unknown %s catalog %q", profile.DisplayName, kind)
	}
	a, err := newAnthropicAPI(baseURL, token, profile)
	if err != nil {
		return nil, err
	}
	items := []CatalogItem{}
	seen := map[string]bool{}
	page := ""
	for range maxCatalogPages {
		q := url.Values{"limit": {"100"}}
		if page != "" {
			q.Set("page", page)
		}
		if kind == "environments" && profile.BetaQuery {
			q.Set("beta", "true")
		}
		var res struct {
			Data []struct {
				ID         string  `json:"id"`
				Name       string  `json:"name"`
				ArchivedAt *string `json:"archived_at"`
			} `json:"data"`
			NextPage string `json:"next_page"`
		}
		if err := a.call(ctx, http.MethodGet, "/"+kind+"?"+q.Encode(), nil, &res); err != nil {
			return nil, err
		}
		if res.Data == nil {
			return nil, fmt.Errorf("invalid %s catalog response", profile.DisplayName)
		}
		for _, item := range res.Data {
			if !strings.HasPrefix(item.ID, prefix) {
				return nil, fmt.Errorf("invalid %s catalog identity", profile.DisplayName)
			}
			if item.ArchivedAt != nil || seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			name := strings.TrimSpace(item.Name)
			if name == "" {
				name = item.ID
			}
			items = append(items, CatalogItem{ID: item.ID, Name: name})
		}
		if res.NextPage == "" {
			return items, nil
		}
		if res.NextPage == page {
			return nil, fmt.Errorf("%s pagination cursor did not advance", profile.DisplayName)
		}
		page = res.NextPage
	}
	return nil, fmt.Errorf("%s catalog exceeds %d pages", profile.DisplayName, maxCatalogPages)
}

func CheckAccess(ctx context.Context, baseURL, token, environmentID string, vaultIDs []string) error {
	return CheckAccessWithProfile(ctx, baseURL, token, environmentID, vaultIDs, ClaudeProfile())
}

func CheckAccessWithProfile(ctx context.Context, baseURL, token, environmentID string, vaultIDs []string, profile Profile) error {
	profile = normalizeProfile(profile)
	a, err := newAnthropicAPI(baseURL, token, profile)
	if err != nil {
		return err
	}
	resources := map[string]string{"/environments/" + url.PathEscape(environmentID): environmentID}
	for _, id := range vaultIDs {
		if !validVaultID.MatchString(id) {
			return fmt.Errorf("vault IDs must look like vlt_...")
		}
		resources["/vaults/"+url.PathEscape(id)] = id
	}
	for path, want := range resources {
		if profile.BetaQuery {
			path += "?beta=true"
		}
		var res struct {
			ID string `json:"id"`
		}
		if err := a.call(ctx, http.MethodGet, path, nil, &res); err != nil {
			return fmt.Errorf("%s access check for %s: %w", profile.DisplayName, want, err)
		}
		if res.ID != want {
			return fmt.Errorf("%s access check for %s: unexpected resource identity", profile.DisplayName, want)
		}
	}
	return nil
}
