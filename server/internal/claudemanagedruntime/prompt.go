package claudemanagedruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/multica-ai/multica/server/internal/cloudagent"
)

func (b *Bridge) prepare(ctx context.Context, t *claimedTask) (*run, error) {
	r := &run{TaskID: t.ID, Phase: "prepared", Resources: []map[string]any{}}
	if t.RuntimeID != b.state.RuntimeID || t.WorkspaceID != b.cfg.WorkspaceID {
		return nil, fmt.Errorf("claim identity does not match bridge")
	}
	chat := t.ChatSessionID != ""
	quickCreate := !chat && t.IssueID == "" && t.QuickCreatePrompt != ""
	if (t.IssueID != "") == (chat || quickCreate) || (t.IsLeaderTask && t.IssueID == "") {
		return nil, fmt.Errorf("%s bridge supports issue, quick-create and chat runs only", b.profile.DisplayName)
	}
	if chat && !b.profile.Chat {
		return nil, fmt.Errorf("chat runs are not supported by the %s bridge", b.profile.DisplayName)
	}
	if quickCreate && !b.profile.QuickCreate {
		return nil, fmt.Errorf("%s supports existing issue runs only; create the issue manually and assign it to this agent", b.profile.DisplayName)
	}
	if t.IsLeaderTask && !b.profile.Leader {
		return nil, fmt.Errorf("%s does not support squad leader runs", b.profile.DisplayName)
	}
	if !strings.HasPrefix(t.AuthToken, "mat_") {
		return nil, fmt.Errorf("claim is missing a task-scoped credential")
	}
	if len(t.RemoteMCPConnections) > 0 || len(t.ConnectedApps) > 0 || len(t.PluginHookTools) > 0 {
		return nil, fmt.Errorf("Multica connected apps and runtime MCP tools are not supported by the %s bridge", b.profile.DisplayName)
	}
	if t.Agent != nil && (hasCustomSkills(t.Agent.Skills) || len(t.Agent.SkillRefs) > 0 || len(t.Agent.CustomEnv) > 0 || len(t.Agent.CustomArgs) > 0 || len(t.Agent.McpConfig) > 0 && string(t.Agent.McpConfig) != "null" && string(t.Agent.McpConfig) != "{}" || t.Agent.Model != "" || t.Agent.ThinkingLevel != "" || t.Agent.ServiceTier != "") {
		return nil, fmt.Errorf("configure skills, tools, credentials and model on the %s; Multica execution overrides are not supported by this bridge", b.profile.AgentNoun)
	}
	if t.Agent == nil {
		return nil, fmt.Errorf("%s", b.profile.AgentBindingError)
	}
	binding, err := ParseAgentConfigForProfile(t.Agent.RuntimeConfig, b.profile)
	if err != nil {
		return nil, err
	}
	r.AgentID = binding.AgentID
	taskAPI := *b.multica
	taskAPI.token = t.AuthToken
	if quickCreate {
		if len(t.QuickCreateAttachmentIDs) > 0 {
			return nil, fmt.Errorf("attachments are not supported by %s quick-create; create the issue without them", b.profile.DisplayName)
		}
		return prepareQuickCreate(t, r), nil
	}
	var p strings.Builder
	var issueTitle string
	if chat {
		r.Reply = &chatReply{token: t.AuthToken, Locale: t.ChatNoticeLocale}
		p.WriteString("You are chatting with a user of a Multica workspace. Reply directly to their message; your final message is shown to them as your reply. This bridge does not expose a local Multica daemon.\n")
		p.WriteString(chatAudience(t.ChatChannelType, t.ChatType))
		fmt.Fprintf(&p, "Files you write under %s are attached to your reply. A file over %d MB is not attached; tell the user instead of saying it was sent.\n", outputDirPrefix, maxReplyFileBytes>>20)
		if t.PriorSessionID != "" {
			r.PriorSessionID = t.PriorSessionID
			p.WriteString("This continues an earlier conversation. If it is not in your context, answer from the message below.\n")
		}
		fmt.Fprintf(&p, "\nRun: %s\n\nUser message:\n%s\n", t.ID, t.ChatMessage)
		// Chat files may join a continued session; each turn mounts under its
		// own run so same-named files from earlier turns stay intact.
		r.MountFiles = len(t.ChatAttachments) > 0
		if err := b.attachChatFiles(ctx, &taskAPI, t, r, &p); err != nil {
			return nil, err
		}
	} else {
		var issue struct {
			Title       string               `json:"title"`
			Description string               `json:"description"`
			Attachments []chatAttachmentMeta `json:"attachments"`
		}
		if err := taskAPI.call(ctx, http.MethodGet, "/api/issues/"+url.PathEscape(t.IssueID), nil, &issue); err != nil {
			return nil, fmt.Errorf("read issue: %w", err)
		}
		if len(issue.Attachments) > 0 && !b.profile.IssueAttachments {
			return nil, fmt.Errorf("issue attachments are not supported by the %s bridge", b.profile.DisplayName)
		}
		issueTitle = issue.Title
		if t.IsLeaderTask {
			r.Leader = &squadLeader{IssueID: t.IssueID, TriggerCommentID: t.TriggerCommentID, token: t.AuthToken}
			fmt.Fprintf(&p, "You lead a Multica squad on the following issue. Follow the Squad Operating Protocol in your agent instructions: coordinate, do not do the work yourself. This bridge does not expose a local Multica daemon.\n")
		} else {
			fmt.Fprintf(&p, "Complete the following Multica issue in your %s environment. Return a final summary including code changes, tests and branch/PR/MR links when available. This bridge does not expose a local Multica daemon.\n", b.profile.DisplayName)
		}
		if t.PriorSessionID != "" {
			r.PriorSessionID = t.PriorSessionID
			p.WriteString("This is a follow-up on the same issue. If the earlier conversation is not in your context, rely on the issue and comments below.\n")
		}
		fmt.Fprintf(&p, "\nRun: %s\nIssue: %s\n\n%s\n\n%s\n", t.ID, t.IssueIdentifier, issue.Title, issue.Description)
		if err := b.attachIssueFiles(ctx, &taskAPI, t, issue.Attachments, r, &p); err != nil {
			return nil, err
		}
	}
	if t.Agent != nil {
		fmt.Fprintf(&p, "\nAgent instructions:\n%s\n", t.Agent.Instructions)
	}
	if r.Leader != nil {
		p.WriteString(leaderBridgeNote(b.profile.DisplayName))
	}
	fmt.Fprintf(&p, "\nWorkspace context:\n%s\n\nProject:\n%s\n%s\n", t.WorkspaceContext, t.ProjectTitle, t.ProjectDescription)
	if t.HandoffNote != "" {
		fmt.Fprintf(&p, "\nHandoff:\n%s\n", t.HandoffNote)
	}
	for _, c := range t.CoalescedComments {
		fmt.Fprintf(&p, "\nComment by %s:\n%s\n", c.AuthorName, c.Content)
	}
	if t.TriggerCommentContent != "" {
		fmt.Fprintf(&p, "\nTriggering comment by %s:\n%s\n", t.TriggerAuthorName, t.TriggerCommentContent)
	}
	for _, res := range t.ProjectResources {
		if res.ResourceType == "local_directory" {
			return nil, fmt.Errorf("local directory resources cannot be executed by %s", b.profile.DisplayName)
		}
		if res.ResourceType != "github_repo" {
			continue
		}
		var ref Repository
		if err := json.Unmarshal(res.ResourceRef, &ref); err != nil {
			return nil, fmt.Errorf("invalid repository resource")
		}
		resource, err := repositoryResourceFor(ref, b.profile)
		if err != nil {
			return nil, err
		}
		r.Resources = append(r.Resources, resource)
		if len(r.Resources) > 1 {
			return nil, fmt.Errorf("%s supports one project repository per run", b.profile.DisplayName)
		}
	}
	// Project repositories win; otherwise use the agent's default repository,
	// and finally the workspace's only repository when it has a credential.
	if len(r.Resources) == 0 && binding.Repository != nil && binding.Repository.URL != "" {
		resource, err := repositoryResourceFor(*binding.Repository, b.profile)
		if err != nil {
			return nil, err
		}
		r.Resources = append(r.Resources, resource)
	}
	if len(r.Resources) == 0 && len(t.Repos) == 1 {
		if resource, err := repositoryResourceFor(Repository{URL: t.Repos[0].URL}, b.profile); err == nil {
			if _, _, _, err := b.sessionResources(ctx, []map[string]any{resource}); err == nil {
				r.Resources = append(r.Resources, resource)
			}
		}
	}
	for _, repo := range t.Repos {
		fmt.Fprintf(&p, "\nAvailable repository: %s (%s)\n", repo.URL, repo.Description)
	}
	// Vaults can give the session tools that reach other repositories (a
	// GitHub MCP server), so an issue run is told exactly which one it may
	// change. Chat is a conversation, so it never gets issue branch/PR rules.
	if !chat {
		if len(r.Resources) == 0 {
			p.WriteString("\nNo repository is mounted for this run. Do not create branches, commits or pull requests in any repository, including repositories your tools can reach; describe the change you would make in your final summary instead.\n")
		} else if t.IssueIdentifier != "" && !t.IsLeaderTask {
			fmt.Fprintf(&p, "\nChange only the mounted repository %s. When you change code, push a new branch named %s (never the default branch), open a pull request in that repository whose title starts with \"%s: \", and put the pull request link in your final summary. Multica links the pull request to this issue through that identifier.\n", r.Resources[0]["url"], BranchName(t.IssueIdentifier, issueTitle), t.IssueIdentifier)
		}
	}
	r.Prompt = p.String()
	return r, nil
}

// chatAudience mirrors the local daemon's chat audience line: a group reply is
// not private to its sender, and an IM chat of unknown type never defaults to
// private. Kept local so the bridge stays independent of the daemon packages.
func chatAudience(channelType, chatType string) string {
	switch {
	case chatType == "group":
		return "Audience: group room; not private; unseen members may read replies.\n"
	case chatType == "p2p" || channelType == "":
		return "Audience: direct room.\n"
	default:
		return "Audience: direct room with the user.\n"
	}
}

// Repository is a GitHub repository and optional branch mounted into a session.
type Repository struct {
	URL string `json:"url"`
	Ref string `json:"ref,omitempty"`
}

type AgentConfig struct {
	AgentID    string      `json:"-"`
	Repository *Repository `json:"repository,omitempty"`
}

func ParseAgentConfig(raw json.RawMessage) (AgentConfig, error) {
	return ParseAgentConfigForProfile(raw, ClaudeProfile())
}

func ParseAgentConfigForProfile(raw json.RawMessage, profile Profile) (AgentConfig, error) {
	profile = normalizeProfile(profile)
	return parseAgentConfig(raw, profile)
}

func parseAgentConfig(raw json.RawMessage, profile Profile) (AgentConfig, error) {
	var payload map[string]json.RawMessage
	var cfg AgentConfig
	if err := json.Unmarshal(raw, &payload); err != nil {
		return cfg, fmt.Errorf("%s", profile.AgentBindingError)
	}
	if err := json.Unmarshal(payload[profile.AgentBindingKey], &cfg.AgentID); err != nil || !ValidAgentID(cfg.AgentID) {
		return cfg, fmt.Errorf("%s", profile.AgentBindingError)
	}
	if repository := payload["repository"]; len(repository) > 0 && string(repository) != "null" {
		if err := json.Unmarshal(repository, &cfg.Repository); err != nil {
			return cfg, fmt.Errorf("%s", profile.AgentBindingError)
		}
	}
	if cfg.Repository != nil && cfg.Repository.URL != "" {
		if _, err := repositoryResourceFor(*cfg.Repository, profile); err != nil {
			return cfg, fmt.Errorf("default repository: %w", err)
		}
	}
	return cfg, nil
}

var commitRef = regexp.MustCompile(`^[a-fA-F0-9]{40}([a-fA-F0-9]{24})?$`)

func repositoryResource(ref Repository) (map[string]any, error) {
	return repositoryResourceFor(ref, ClaudeProfile())
}

func repositoryResourceFor(ref Repository, profile Profile) (map[string]any, error) {
	u, err := url.Parse(ref.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("repositories must use an HTTPS github.com URL without credentials")
	}
	if _, err := repositoryKey(ref.URL); err != nil {
		return nil, err
	}
	// Claude accepts https://github.com/<owner>/<repo> without a .git suffix;
	// OMA's repository contract requires the clone URL to end in .git.
	path := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git")
	if profile.Provider == cloudagent.OMAProvider {
		path += ".git"
	}
	resource := map[string]any{"type": "github_repository", "url": "https://github.com" + path}
	if strings.HasPrefix(ref.Ref, "refs/tags/") || commitRef.MatchString(ref.Ref) {
		return nil, fmt.Errorf("%s supports branch refs only", profile.DisplayName)
	}
	if name := strings.TrimPrefix(ref.Ref, "refs/heads/"); name != "" {
		resource["checkout"] = map[string]string{"type": "branch", "name": name}
	}
	return resource, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// BranchName gives remote work a branch Multica's GitHub integration links back
// to the issue: the identifier must appear in the branch name.
func BranchName(identifier, title string) string {
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(slug) > 40 {
		slug = slug[:41]
		if i := strings.LastIndex(slug, "-"); i > 0 {
			slug = slug[:i]
		} else {
			slug = slug[:40]
		}
	}
	branch := "multica/" + strings.ToLower(identifier)
	if slug != "" {
		branch += "-" + slug
	}
	return branch
}

// Built-in platform skills have no ID and assume local CLI tools. Do not inject
// those tools into a Claude Managed Agents session; custom skills must be configured there.
func hasCustomSkills(skills []claimedSkill) bool {
	for _, s := range skills {
		if s.ID != "" {
			return true
		}
	}
	return false
}

// ValidAgentID validates the public Claude agent identifier without accepting URL fragments.
func ValidAgentID(id string) bool {
	return regexp.MustCompile(`^agent_[a-zA-Z0-9_-]{1,128}$`).MatchString(id)
}
