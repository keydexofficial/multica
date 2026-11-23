package handler

import (
	"fmt"

	"github.com/multica-ai/multica/server/internal/claudemanagedruntime"
	"github.com/multica-ai/multica/server/internal/cloudagent"
)

var cloudAgentConfigValidators = map[string]func([]byte) error{
	cloudagent.ClaudeProvider: func(raw []byte) error {
		_, err := claudemanagedruntime.ParseAgentConfig(raw)
		return err
	},
	cloudagent.QoderProvider: validateQoderAgentConfig,
	cloudagent.OMAProvider: func(raw []byte) error {
		_, err := claudemanagedruntime.ParseAgentConfigForProfile(raw, claudemanagedruntime.OMAProfile())
		return err
	},
}

func isCloudAgentProvider(provider string) bool {
	return cloudagent.IsCloud(provider)
}

func cloudAgentServiceName(provider string) string {
	return cloudagent.ServiceName(provider)
}

func cloudAgentConnectionName(provider string) string {
	return cloudagent.ConnectionName(provider)
}

func cloudAgentSupportsChat(provider string) bool {
	return cloudagent.SupportsChat(provider)
}

func cloudAgentSupportsQuickCreate(provider string) bool {
	return cloudagent.SupportsQuickCreate(provider)
}

func validateCloudAgentConfig(provider string, raw []byte) error {
	validate, ok := cloudAgentConfigValidators[provider]
	if !ok {
		return fmt.Errorf("unsupported cloud agent provider %q", provider)
	}
	return validate(raw)
}
