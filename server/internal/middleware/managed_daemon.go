package middleware

import "github.com/multica-ai/multica/server/internal/cloudagent"

func ManagedDaemonProvider(daemonID string) string {
	descriptor, ok := cloudagent.ByDaemonID(daemonID)
	if !ok {
		return ""
	}
	return descriptor.Provider
}
