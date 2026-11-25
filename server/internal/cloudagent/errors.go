package cloudagent

import "errors"

// GitHub App credential resolution shared by cloud runtime bridges. A bridge
// falls back to the workspace's GitHub App installation when no manual
// repository token is configured.
var (
	// ErrGitHubAppNotCovered reports that no bound installation authorizes the
	// repository. Callers treat it as "no credential configured".
	ErrGitHubAppNotCovered = errors.New("GitHub App installation does not cover the repository")
	// ErrGitHubAppPermissions reports that the installation exists but has not
	// granted the repository write permissions remote work needs.
	ErrGitHubAppPermissions = errors.New("workspace GitHub App has not granted repository write permissions; approve Contents and Pull requests write access on GitHub, or add a manual repository token")
)
