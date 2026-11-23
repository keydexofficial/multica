package middleware

import "testing"

func TestManagedDaemonProviderRecognizesOMA(t *testing.T) {
	for daemonID, want := range map[string]string{
		"qoder-managed-ws":  "qoder_cloud",
		"claude-managed-ws": "claude_managed",
		"oma-managed-ws":    "oma_cloud",
		"oma-bridge-abc":    "",
		"daemon-1":          "",
	} {
		if got := ManagedDaemonProvider(daemonID); got != want {
			t.Fatalf("ManagedDaemonProvider(%q) = %q, want %q", daemonID, got, want)
		}
	}
}
