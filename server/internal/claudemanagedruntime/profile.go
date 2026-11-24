package claudemanagedruntime

import "github.com/multica-ai/multica/server/internal/cloudagent"

type Profile struct {
	Provider                  string
	DisplayName               string
	AgentNoun                 string
	DefaultBaseURL            string
	AllowPlainHTTP            bool
	BetaQuery                 bool
	FailureReason             string
	StandalonePrefix          string
	AgentBindingKey           string
	AgentBindingError         string
	SkipWorkspaceKeyDetection bool
	Usage                     UsageMode
	// Chat lets the bridge accept conversation turns. Cloud providers that
	// expose no chat capability are never sent one, and this keeps a standalone
	// bridge from silently running a task its documented contract excludes.
	Chat bool
}

func ClaudeProfile() Profile {
	descriptor, _ := cloudagent.Lookup(cloudagent.ClaudeProvider)
	return Profile{
		Provider:          descriptor.Provider,
		DisplayName:       descriptor.ServiceName,
		AgentNoun:         descriptor.AgentNoun,
		DefaultBaseURL:    DefaultAnthropicURL,
		FailureReason:     descriptor.Provider + "_execution",
		StandalonePrefix:  StandaloneDaemonPrefix,
		AgentBindingKey:   "claude_agent_id",
		AgentBindingError: "associate a Claude agent in the Multica agent runtime configuration",
	}
}

func OMAProfile() Profile {
	descriptor, _ := cloudagent.Lookup(cloudagent.OMAProvider)
	return Profile{
		Provider:                  descriptor.Provider,
		DisplayName:               descriptor.ServiceName,
		AgentNoun:                 descriptor.AgentNoun,
		AllowPlainHTTP:            true,
		BetaQuery:                 true,
		FailureReason:             descriptor.Provider + "_execution",
		StandalonePrefix:          "oma-bridge-",
		AgentBindingKey:           "oma_agent_id",
		AgentBindingError:         "associate an OMA agent in the Multica agent runtime configuration",
		SkipWorkspaceKeyDetection: true,
		Usage:                     UsageSession,
		Chat:                      true,
	}
}

func normalizeProfile(profile Profile) Profile {
	if profile.Provider == "" {
		return ClaudeProfile()
	}
	defaults := ClaudeProfile()
	if profile.Provider == cloudagent.OMAProvider {
		defaults = OMAProfile()
		profile.AllowPlainHTTP = true
		profile.BetaQuery = true
		profile.SkipWorkspaceKeyDetection = true
		profile.Chat = true
	}
	if profile.DisplayName == "" {
		profile.DisplayName = defaults.DisplayName
	}
	if profile.AgentNoun == "" {
		profile.AgentNoun = defaults.AgentNoun
	}
	if profile.DefaultBaseURL == "" && profile.Provider != cloudagent.OMAProvider {
		profile.DefaultBaseURL = defaults.DefaultBaseURL
	}
	if profile.FailureReason == "" {
		profile.FailureReason = defaults.FailureReason
	}
	if profile.StandalonePrefix == "" {
		profile.StandalonePrefix = defaults.StandalonePrefix
	}
	if profile.AgentBindingKey == "" {
		profile.AgentBindingKey = defaults.AgentBindingKey
	}
	if profile.AgentBindingError == "" {
		profile.AgentBindingError = defaults.AgentBindingError
	}
	if profile.Usage == "" {
		profile.Usage = defaults.Usage
	}
	return profile
}
