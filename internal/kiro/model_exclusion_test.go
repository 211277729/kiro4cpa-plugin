package kiro

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/models"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func modelsForAuth(t *testing.T, host pluginapi.HostConfigSummary) []string {
	t.Helper()
	catalog := []models.UpstreamModel{{ModelID: "auto"}, {ModelID: "claude-sonnet-4.5"}, {ModelID: "claude-opus-4.8"}, {ModelID: "glm-5"}, {ModelID: "deepseek-3.2"}}
	storage, _ := mergeCredential(withCatalog(map[string]any{}, catalog, time.Now()), Credential{AuthMethod: AuthMethodSocial, RefreshToken: "rt"})
	resp := callHandler[pluginapi.ModelResponse](t, pluginabi.MethodModelForAuth,
		authModelRPC{AuthModelRequest: pluginapi.AuthModelRequest{StorageJSON: storage, Host: host}})
	ids := make([]string, 0, len(resp.Models))
	for _, m := range resp.Models {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids
}

func TestAccountExclusionMatchesPanelAlias(t *testing.T) {
	host := pluginapi.HostConfigSummary{OAuthModelAlias: map[string][]pluginapi.ModelAlias{
		models.ProviderName: {{Name: "claude-sonnet-4.5", Alias: "claude-sonnet-4-5"}, {Name: "claude-sonnet-5.5", Alias: "claude-sonnet-5-5"}},
	}}
	catalog := []models.UpstreamModel{{ModelID: "claude-sonnet-4.5"}, {ModelID: "claude-sonnet-5.5"}}
	storage, _ := mergeCredential(withCatalog(map[string]any{}, catalog, time.Now()), socialCred)
	for _, tc := range []struct {
		name, rule string
		want       []string
	}{
		{"pro excludes selected alias", "claude-sonnet-4-5", []string{"claude-sonnet-5.5"}},
		{"pro excludes source", "claude-sonnet-4.5", []string{"claude-sonnet-5.5"}},
		{"pro excludes alias wildcard", "claude-sonnet-4-*", []string{"claude-sonnet-5.5"}},
		{"cleared rule restores models", "", []string{"claude-sonnet-4.5", "claude-sonnet-5.5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := pluginapi.AuthModelRequest{AuthID: "pro", Host: host, StorageJSON: storage,
				Attributes: map[string]string{"excluded_models": tc.rule}}
			assertAccountModels(t, req, tc.want)
			// The same provider aliases must not block another account.
			freeStorage, _ := mergeCredential(withCatalog(map[string]any{}, catalog[:1], time.Now()), socialCred)
			assertAccountModels(t, pluginapi.AuthModelRequest{AuthID: "free", Host: host, StorageJSON: freeStorage}, []string{"claude-sonnet-4.5"})
		})
	}
}

func assertAccountModels(t *testing.T, req pluginapi.AuthModelRequest, want []string) {
	t.Helper()
	before, _ := json.Marshal(req)
	resp := callHandler[pluginapi.ModelResponse](t, pluginabi.MethodModelForAuth, authModelRPC{AuthModelRequest: req})
	ids := make([]string, 0, len(resp.Models))
	for _, info := range resp.Models {
		ids = append(ids, info.ID)
	}
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("account %s: models=%v, want %v", req.AuthID, ids, want)
	}
	after, _ := json.Marshal(req)
	if string(before) != string(after) {
		t.Fatal("model registration mutated the host configuration or credential")
	}
}

func TestAccountExclusionsRespectAliasScopeAndOverrides(t *testing.T) {
	catalog := []models.UpstreamModel{{ModelID: "claude-sonnet-4.5"}, {ModelID: "claude-sonnet-5.5"}, {ModelID: "auto"}}
	storage, _ := mergeCredential(withCatalog(map[string]any{}, catalog, time.Now()), socialCred)
	host := pluginapi.HostConfigSummary{
		ExcludedModels: map[string][]string{models.ProviderName: {"auto"}},
		OAuthModelAlias: map[string][]pluginapi.ModelAlias{
			models.ProviderName: {{Name: "claude-sonnet-4.5", Alias: "client-sonnet"}},
			"claude":            {{Name: "claude-sonnet-5.5", Alias: "other-provider"}},
		},
	}
	for _, tc := range []struct {
		name, rule, ownAliases string
		want                   []string
	}{
		{"global and account rules combine", " CLIENT-SONNET ", "", []string{"claude-sonnet-5.5"}},
		{"other provider does not leak", "other-provider", "", []string{"claude-sonnet-4.5", "claude-sonnet-5.5"}},
		{"account mapping overrides provider mapping", "client-sonnet", `[{"name":"claude-sonnet-5.5","alias":"client-sonnet"}]`, []string{"claude-sonnet-4.5"}},
		{"account mapping keeps other provider aliases", "client-sonnet", `[{"name":"claude-sonnet-5.5","alias":"pro-only"}]`, []string{"claude-sonnet-5.5"}},
		{"account mapping alias", "pro-only", `[{"name":"claude-sonnet-5.5","alias":"pro-only"}]`, []string{"claude-sonnet-4.5"}},
		{"malformed account mapping falls back", "client-sonnet", `{bad`, []string{"claude-sonnet-5.5"}},
		{"invalid mapping entries do not shadow provider mapping", "client-sonnet", `[{"name":"","alias":"client-sonnet"},{"name":"client-sonnet","alias":"client-sonnet"}]`, []string{"claude-sonnet-5.5"}},
		{"duplicate account alias uses first mapping", "pro-only", `[{"name":"claude-sonnet-5.5","alias":"pro-only"},{"name":"claude-sonnet-4.5","alias":"pro-only"}]`, []string{"claude-sonnet-4.5"}},
		{"no alias guessed from spelling", "claude-sonnet-4-5", "", []string{"claude-sonnet-4.5", "claude-sonnet-5.5"}},
		{"all excluded", "*", "", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertAccountModels(t, pluginapi.AuthModelRequest{Host: host, StorageJSON: storage,
				Attributes: map[string]string{"excluded_models": tc.rule, "model_aliases": tc.ownAliases}}, tc.want)
		})
	}
}

func TestAccountExclusionClearsWithoutReusingStoredSnapshot(t *testing.T) {
	host := pluginapi.HostConfigSummary{OAuthModelAlias: map[string][]pluginapi.ModelAlias{
		models.ProviderName: {{Name: "claude-sonnet-4.5", Alias: "sonnet-public"}},
	}}
	storage, _ := mergeCredential(withCatalog(map[string]any{"excluded_models": []string{"sonnet-public"}},
		[]models.UpstreamModel{{ModelID: "claude-sonnet-4.5"}}, time.Now()), socialCred)
	// CPA's latest attributes have cleared the rule while storage still carries
	// an older snapshot. The plugin must not resurrect the removed exclusion.
	assertAccountModels(t, pluginapi.AuthModelRequest{StorageJSON: storage, Host: host,
		Attributes: map[string]string{"excluded_models": ""}}, []string{"claude-sonnet-4.5"})
	// Alias matching is configuration-driven: after a mapping is changed, the
	// old alias rule must no longer disable an unrelated model.
	host.OAuthModelAlias[models.ProviderName][0].Alias = "renamed-sonnet"
	assertAccountModels(t, pluginapi.AuthModelRequest{StorageJSON: storage, Host: host,
		Attributes: map[string]string{"excluded_models": "sonnet-public"}}, []string{"claude-sonnet-4.5"})
}

func TestGlobalExclusionMatchesAlias(t *testing.T) {
	host := pluginapi.HostConfigSummary{
		ExcludedModels:  map[string][]string{models.ProviderName: {"sonnet-public"}},
		OAuthModelAlias: map[string][]pluginapi.ModelAlias{models.ProviderName: {{Name: "claude-sonnet-4.5", Alias: "sonnet-public"}}},
	}
	want := []string{"auto", "claude-opus-4.8", "deepseek-3.2", "glm-5"}
	if got := modelsForAuth(t, host); !reflect.DeepEqual(got, want) {
		t.Fatalf("global alias exclusion: models=%v, want %v", got, want)
	}
}

func TestModelsForAuthAppliesGlobalExclusions(t *testing.T) {
	got := modelsForAuth(t, pluginapi.HostConfigSummary{ExcludedModels: map[string][]string{
		"kiro4cpa": {" Claude-Opus-4.8 ", "auto", "deepseek-*"},
		"claude":   {"glm-5"},
	}})
	want := []string{"claude-sonnet-4.5", "glm-5"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("models %v, want %v", got, want)
	}
}

func TestModelsForAuthWithoutGlobalExclusions(t *testing.T) {
	if got := modelsForAuth(t, pluginapi.HostConfigSummary{}); len(got) != 5 {
		t.Fatalf("models %v", got)
	}
}

func TestMatchWildcardMatchesCPA(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"glm-5", "glm-5", true},
		{"glm-5", "glm-5.1", false},
		{"claude-*", "claude-opus-4.8", true},
		{"*-4.8", "claude-opus-4.8", true},
		{"claude-*-4.8", "claude-opus-4.8", true},
		{"claude-*-4.8", "claude-opus-4.6", false},
		{"*", "anything", true},
		{"", "auto", false},
	}
	for _, tc := range cases {
		if got := matchWildcard(tc.pattern, tc.value); got != tc.want {
			t.Fatalf("matchWildcard(%q, %q) = %v", tc.pattern, tc.value, got)
		}
	}
}
