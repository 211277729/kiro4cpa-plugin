package kiro

import (
	"encoding/json"
	"strings"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/models"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// CPA filters source IDs before applying aliases, but the official panel saves
// selections from the already aliased account catalogue. Match both namespaces
// before publication so an excluded alias cannot route back to this account.
// Global rules remain additive to this account's own exclusions.
func withoutModelExclusions(infos []pluginapi.ModelInfo, req pluginapi.AuthModelRequest) []pluginapi.ModelInfo {
	patterns := strings.Split(req.Attributes["excluded_models"], ",")
	for provider, rules := range req.Host.ExcludedModels {
		if !strings.EqualFold(strings.TrimSpace(provider), models.ProviderName) {
			continue
		}
		patterns = append(patterns, rules...)
	}
	active := patterns[:0]
	for _, rule := range patterns {
		if p := strings.ToLower(strings.TrimSpace(rule)); p != "" {
			active = append(active, p)
		}
	}
	if len(active) == 0 {
		return infos
	}
	aliases := effectiveModelAliases(req.Host, req.Attributes)
	out := make([]pluginapi.ModelInfo, 0, len(infos))
	for _, info := range infos {
		id := strings.ToLower(strings.TrimSpace(info.ID))
		blocked := false
		for _, p := range active {
			if matchWildcard(p, id) {
				blocked = true
				break
			}
			for _, alias := range aliases[id] {
				if matchWildcard(p, alias) {
					blocked = true
					break
				}
			}
			if blocked {
				break
			}
		}
		if !blocked {
			out = append(out, info)
		}
	}
	return out
}

// Account mappings override provider mappings by alias, as in CPA's
// oauthModelAliasesForAuth. Use host routing attributes rather than persisted
// snapshots, so clearing an exclusion or changing a mapping takes effect.
func effectiveModelAliases(host pluginapi.HostConfigSummary, attributes map[string]string) map[string][]string {
	var own []pluginapi.ModelAlias
	if err := json.Unmarshal([]byte(attributes["model_aliases"]), &own); err != nil {
		own = nil
	}
	var configured []pluginapi.ModelAlias
	for provider, entries := range host.OAuthModelAlias {
		if strings.EqualFold(strings.TrimSpace(provider), models.ProviderName) {
			configured = append(configured, entries...)
		}
	}
	bySource := make(map[string][]string)
	seen := make(map[string]bool)
	for _, entry := range append(own, configured...) {
		name := strings.ToLower(strings.TrimSpace(entry.Name))
		alias := strings.ToLower(strings.TrimSpace(entry.Alias))
		if name == "" || alias == "" || name == alias || seen[alias] {
			continue
		}
		seen[alias] = true
		bySource[name] = append(bySource[name], alias)
	}
	return bySource
}

// matchWildcard mirrors CPA's sdk/cliproxy matchWildcard so a rule hides the
// same models here as it does in CPA.
func matchWildcard(pattern, value string) bool {
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	parts := strings.Split(pattern, "*")
	if prefix := parts[0]; prefix != "" {
		if !strings.HasPrefix(value, prefix) {
			return false
		}
		value = value[len(prefix):]
	}
	if suffix := parts[len(parts)-1]; suffix != "" {
		if !strings.HasSuffix(value, suffix) {
			return false
		}
		value = value[:len(value)-len(suffix)]
	}
	for i := 1; i < len(parts)-1; i++ {
		segment := parts[i]
		if segment == "" {
			continue
		}
		idx := strings.Index(value, segment)
		if idx < 0 {
			return false
		}
		value = value[idx+len(segment):]
	}
	return true
}
