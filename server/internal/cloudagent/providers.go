package cloudagent

import "strings"

const (
	QoderProvider  = "qoder_cloud"
	ClaudeProvider = "claude_managed"
	OMAProvider    = "oma_cloud"
)

type Capabilities struct {
	Chat        bool
	QuickCreate bool
}

type Descriptor struct {
	Provider          string
	DaemonPrefix      string
	ServiceName       string
	ShortName         string
	ConnectionName    string
	AgentNoun         string
	LockSeed          int64
	SupervisorLockKey int64
	ManagedDaemonOnly bool
	Capabilities      Capabilities
}

var descriptors = []Descriptor{
	{
		Provider:          QoderProvider,
		DaemonPrefix:      "qoder-managed-",
		ServiceName:       "Qoder Cloud Agents",
		ShortName:         "Qoder",
		ConnectionName:    "Qoder",
		AgentNoun:         "Qoder Agent",
		LockSeed:          19,
		SupervisorLockKey: 8191,
		Capabilities:      Capabilities{Chat: true, QuickCreate: true},
	},
	{
		Provider:          ClaudeProvider,
		DaemonPrefix:      "claude-managed-",
		ServiceName:       "Claude Managed Agents",
		ShortName:         "Claude",
		ConnectionName:    "Claude Managed Agents",
		AgentNoun:         "Claude agent",
		LockSeed:          23,
		SupervisorLockKey: 8192,
		ManagedDaemonOnly: true,
	},
	{
		Provider:          OMAProvider,
		DaemonPrefix:      "oma-managed-",
		ServiceName:       "OMA Managed Agents",
		ShortName:         "OMA",
		ConnectionName:    "OMA Managed Agents",
		AgentNoun:         "OMA agent",
		LockSeed:          24,
		SupervisorLockKey: 8193,
		ManagedDaemonOnly: true,
		Capabilities:      Capabilities{Chat: true},
	},
}

func Lookup(provider string) (Descriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.Provider == provider {
			return descriptor, true
		}
	}
	return Descriptor{}, false
}

func IsCloud(provider string) bool {
	_, ok := Lookup(provider)
	return ok
}

func ServiceName(provider string) string {
	if descriptor, ok := Lookup(provider); ok {
		return descriptor.ServiceName
	}
	return ""
}

func ConnectionName(provider string) string {
	if descriptor, ok := Lookup(provider); ok {
		return descriptor.ConnectionName
	}
	return ""
}

func ByDaemonID(daemonID string) (Descriptor, bool) {
	for _, descriptor := range descriptors {
		if daemonID != "" && strings.HasPrefix(daemonID, descriptor.DaemonPrefix) {
			return descriptor, true
		}
	}
	return Descriptor{}, false
}

func Providers() []string {
	providers := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		providers = append(providers, descriptor.Provider)
	}
	return providers
}

func SupportsChat(provider string) bool {
	descriptor, ok := Lookup(provider)
	return ok && descriptor.Capabilities.Chat
}

func SupportsQuickCreate(provider string) bool {
	descriptor, ok := Lookup(provider)
	return ok && descriptor.Capabilities.QuickCreate
}
