package cloudagent

import (
	"slices"
	"testing"
)

func TestProvidersAreUniqueAndOrdered(t *testing.T) {
	providers := Providers()
	if !slices.Equal(providers, []string{QoderProvider, ClaudeProvider, OMAProvider}) {
		t.Fatalf("providers = %v", providers)
	}
	for _, provider := range providers {
		if _, ok := Lookup(provider); !ok {
			t.Fatalf("lookup missed %s", provider)
		}
	}
}

func TestDescriptorIdentity(t *testing.T) {
	qoder, ok := Lookup(QoderProvider)
	if !ok || qoder.DaemonPrefix != "qoder-managed-" || qoder.SupervisorLockKey != 8191 {
		t.Fatalf("qoder = %+v", qoder)
	}
	claude, ok := Lookup(ClaudeProvider)
	if !ok || claude.DaemonPrefix != "claude-managed-" || claude.SupervisorLockKey != 8192 {
		t.Fatalf("claude = %+v", claude)
	}
	oma, ok := Lookup(OMAProvider)
	if !ok || oma.DaemonPrefix != "oma-managed-" || oma.SupervisorLockKey != 8193 {
		t.Fatalf("oma = %+v", oma)
	}
	if qoder.LockSeed == claude.LockSeed || qoder.LockSeed == oma.LockSeed || claude.LockSeed == oma.LockSeed {
		t.Fatal("provider lock seeds collide")
	}
}

func TestCapabilities(t *testing.T) {
	if !SupportsChat(QoderProvider) || !SupportsQuickCreate(QoderProvider) {
		t.Fatal("qoder capabilities missing")
	}
	if SupportsChat(ClaudeProvider) || SupportsQuickCreate(ClaudeProvider) {
		t.Fatal("claude capabilities overstated")
	}
	if !SupportsChat(OMAProvider) || !SupportsQuickCreate(OMAProvider) {
		t.Fatal("oma capabilities incorrect")
	}
	if SupportsChat("local") || SupportsQuickCreate("local") {
		t.Fatal("non-cloud provider reported cloud capabilities")
	}
}

func TestByDaemonID(t *testing.T) {
	for daemonID, want := range map[string]string{
		"qoder-managed-ws":  QoderProvider,
		"claude-managed-ws": ClaudeProvider,
		"oma-managed-ws":    OMAProvider,
		"claude-bridge-abc": "",
		"oma-bridge-abc":    "",
		"":                  "",
	} {
		descriptor, ok := ByDaemonID(daemonID)
		if got := descriptor.Provider; got != want || ok != (want != "") {
			t.Fatalf("ByDaemonID(%q) = %q/%v, want %q", daemonID, got, ok, want)
		}
	}
}

func TestUnknownProviderHasNoCloudIdentity(t *testing.T) {
	if IsCloud("other_cloud") {
		t.Fatal("unregistered provider treated as cloud")
	}
	descriptor, ok := Lookup("other_cloud")
	if ok || descriptor.Provider != "" {
		t.Fatalf("lookup = %+v/%v", descriptor, ok)
	}
	if ServiceName("other_cloud") != "" || ConnectionName("other_cloud") != "" {
		t.Fatal("unknown provider returned identity")
	}
}
