package kiro

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/config"
	"github.com/AkkunYo/kiro4cpa-plugin/internal/models"
	"github.com/AkkunYo/kiro4cpa-plugin/internal/wire"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// pluginID is the id CPA derives from the kiro4cpa-v<version>.so file name.
	pluginID            = "kiro4cpa"
	refreshLead         = 10 * time.Minute
	minRefreshDelay     = time.Minute
	longRefreshInterval = 24 * time.Hour
)

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelRegistrar        bool                         `json:"model_registrar"`
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	QuotaProvider         bool                         `json:"quota_provider"`
	ManagementAPI         bool                         `json:"management_api"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

func getRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "New Kiro OAuth2",
			Version:          Version,
			Author:           "xiaoyugan",
			GitHubRepository: "https://github.com/211277729/kiro4cpa-plugin",
			Logo:             logoDataURI(),
			ConfigFields: []pluginapi.ConfigField{
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "启用 Kiro 插件"},
				{Name: "default_region", Type: pluginapi.ConfigFieldTypeString, Description: "默认 AWS 区域。凭据未指定 apiRegion、profileArn 里也没有区域时使用，例如 us-east-1"},
				{Name: "client_version", Type: pluginapi.ConfigFieldTypeString, Description: "User-Agent 中上报的 Kiro IDE 版本，例如 1.4.5"},
				{Name: "os_version", Type: pluginapi.ConfigFieldTypeString, Description: "User-Agent 中上报的操作系统标识，例如 darwin#24.6.0"},
				{Name: "node_version", Type: pluginapi.ConfigFieldTypeString, Description: "User-Agent 中上报的 Node.js 版本，例如 22.22.0"},
				{Name: "min_token_interval", Type: pluginapi.ConfigFieldTypeString, Description: "同一凭据两次请求之间的最小间隔，例如 1s"},
				{Name: "max_token_interval", Type: pluginapi.ConfigFieldTypeString, Description: "同一凭据请求间隔的上限，例如 2s。排队超过这个上限的并发请求会直接返回 429，交给 CPA 换号"},
				{Name: "suspend_cooldown", Type: pluginapi.ConfigFieldTypeString, Description: "被 Kiro 判定为封禁或无权限的凭据退出轮转的时长，例如 1h"},
				{Name: "usage_mode", Type: pluginapi.ConfigFieldTypeEnum, Default: "credit", EnumValues: []string{"credit", "estimate", "off"},
					Description: "用量上报方式。credit：按 credit_rates 把积分换算成 token 并拆出缓存（默认）；estimate：只用字符数估算；off：只报估算，不认缓存"},
				{Name: "credit_rates", Type: pluginapi.ConfigFieldTypeObject,
					Description: "每个模型每 token 消耗的 credit 单价。可写单个数字（输入输出同价），也可写 {input, output}；精确模型名优先，\"*\" 兜底。实测默认值：sonnet {input: 3.53e-5, output: 2.87e-4}、opus {input: 5.13e-5, output: 4.48e-4}、haiku {input: 4.39e-6, output: 1.29e-5}"},
				{Name: "cache_read_ratio", Type: pluginapi.ConfigFieldTypeString, Default: "0.10",
					Description: "缓存命中的 token 按输入价的多少计费，例如 0.10。实测 sonnet 约 25%、opus 10%、haiku 11%；0.10 是保守默认值"},
				{Name: "max_cache_read_share", Type: pluginapi.ConfigFieldTypeString, Default: "0.9",
					Description: "按积分换算时，缓存最多能占本次 prompt 的比例，默认 0.9。上游只给一个总积分，便宜的请求会被解成「整段命中缓存」，客户只付缓存价而你仍要为整段 prompt 付积分；设上限后至少保留 (1-上限) 的新增输入按原价计费。1 表示不封顶，0 表示完全不报缓存"},
			},
		},
		Capabilities: registrationCapability{
			ModelRegistrar: true,
			ModelProvider:  true,
			AuthProvider:   true,
			Executor:       true,
			// Kiro has no credential-free models. Static models would be published
			// under a credential-less executor client that CPA never filters with
			// oauth-excluded-models, so only per-auth models are exposed.
			ExecutorModelScope: pluginapi.ExecutorModelScopeOAuth,
			// CPA translates OpenAI Chat / Responses to and from Claude itself.
			ExecutorInputFormats:  []string{"claude"},
			ExecutorOutputFormats: []string{"claude"},
			QuotaProvider:         true,
			// Serves the login method selection page and its actions.
			ManagementAPI: true,
		},
	}
}

// HandleMethod handles incoming C-ABI RPC calls from CLIProxyAPI.
func HandleMethod(method string, request []byte) (out []byte, err error) {
	defer func() {
		// A panic crossing the C-ABI would take down the whole CPA process.
		if r := recover(); r != nil {
			log.Printf("kiro4cpa: %s panic: %v", method, r)
			out, err = nil, fmt.Errorf("kiro4cpa internal error in %s: %v", method, r)
		}
	}()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, err
			}
			if err := config.UpdateFromYAML(req.ConfigYAML); err != nil {
				return nil, err
			}
		}
		return wire.OK(getRegistration()), nil

	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return wire.OK(map[string]bool{"acknowledged": true}), nil

	case pluginabi.MethodModelRegister:
		return wire.OK(pluginapi.ModelRegistrationResponse{
			Provider: models.ProviderName,
			Models:   models.GetDefaultModelInfos(),
		}), nil

	case pluginabi.MethodModelStatic:
		return wire.OK(pluginapi.ModelResponse{
			Provider: models.ProviderName,
			Models:   models.GetDefaultModelInfos(),
		}), nil

	case pluginabi.MethodModelForAuth:
		return handleModelForAuth(request)

	case pluginabi.MethodAuthIdentifier:
		return wire.OK(identifierResponse{Identifier: models.ProviderName}), nil

	case pluginabi.MethodAuthParse:
		return handleAuthParse(request)

	case pluginabi.MethodAuthRefresh:
		return handleAuthRefresh(request)

	case pluginabi.MethodAuthLoginStart:
		return handleLoginStart(request)

	case pluginabi.MethodAuthLoginPoll:
		return handleLoginPoll(request)

	case pluginabi.MethodExecutorIdentifier:
		return wire.OK(identifierResponse{Identifier: models.ProviderName}), nil

	case pluginabi.MethodExecutorExecute:
		return handleExecute(request)

	case pluginabi.MethodExecutorExecuteStream:
		return handleExecuteStream(request)

	case pluginabi.MethodExecutorCountTokens:
		return handleCountTokens(request)

	case pluginabi.MethodQuotaIdentifier:
		return wire.OK(identifierResponse{Identifier: models.ProviderName}), nil

	case pluginabi.MethodQuotaDescribe:
		return handleQuotaDescribe()

	case pluginabi.MethodQuotaFetch:
		return handleQuotaFetch(request)

	case pluginabi.MethodManagementRegister:
		return wire.OK(managementRoutes()), nil

	case pluginabi.MethodManagementHandle:
		return handleManagement(request)

	default:
		return wire.Error("unsupported_method", "method not supported yet: "+method), nil
	}
}

type authRefreshRPC struct {
	pluginapi.AuthRefreshRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleAuthParse runs synchronously while CPA loads auth files, so it never
// touches the network. A credential without a stored catalogue is scheduled
// for an immediate refresh, which fetches the catalogue off the startup path.
func handleAuthParse(request []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	if req.Provider != models.ProviderName {
		return wire.OK(pluginapi.AuthParseResponse{Handled: false}), nil
	}
	cred, fields, err := decodeCredential(req.RawJSON)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", req.FileName, err)
	}
	storage, err := mergeCredential(fields, cred)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	next := now
	disabled, _ := fields["disabled"].(bool)
	switch _, hasCatalog := catalogFrom(fields); {
	case hasCatalog:
		next = scheduledRefresh(cred, now)
	case disabled:
		// CPA re-parses the file after every refresh write and, for a disabled
		// credential, takes this time instead of keeping its own schedule.
		// "Now" would refresh it again at once, in a loop, for an account
		// whose catalogue Kiro keeps refusing.
		next = now.Add(longRefreshInterval)
	}
	return wire.OK(pluginapi.AuthParseResponse{
		Handled: true,
		Auth: pluginapi.AuthData{
			Provider:         models.ProviderName,
			FileName:         req.FileName,
			Label:            authLabel(cred, req.FileName),
			StorageJSON:      storage,
			Metadata:         authMetadata(cred),
			NextRefreshAfter: next,
		},
	}), nil
}

func handleAuthRefresh(request []byte) ([]byte, error) {
	var req authRefreshRPC
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	cred, fields, err := decodeCredential(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	cfg := config.Get()
	now := time.Now()
	metadata := cloneMap(req.Metadata)
	metadata[refreshIntervalKey] = refreshIntervalSeconds

	// Shared with executor refreshes: CPA persists the result of this one.
	refreshed, err := sharedRefresh(req.AuthID, cred, func(c Credential) (Credential, error) {
		return refreshCredential(c, cfg, req.HostCallbackID, now)
	})
	if errors.Is(err, ErrInvalidGrant) {
		// A revoked refresh token never recovers; disable instead of retrying forever.
		metadata["disabled"] = true
		next := now.Add(longRefreshInterval)
		return wire.OK(pluginapi.AuthRefreshResponse{
			Auth: pluginapi.AuthData{
				Provider: models.ProviderName, ID: req.AuthID, Disabled: true,
				Metadata: metadata, NextRefreshAfter: next,
			},
			NextRefreshAfter: next,
		}), nil
	}
	if err != nil {
		return nil, err
	}

	catalog, errList := fetchModelCatalog(refreshed, cfg, req.HostCallbackID)
	switch {
	case errList == nil:
		fields = withCatalog(fields, catalog, now)
		if blockedNoteSet(fields) {
			// Kiro serves the account again; the user re-enables it in the panel.
			delete(fields, blockedAtKey)
			if note, _ := fields["note"].(string); note == blockedNote {
				// CPA merges metadata over the file, so clear rather than delete.
				fields["note"] = ""
				metadata["note"] = ""
			}
		}
	case isAccountBlocked(errList):
		log.Printf("kiro4cpa: Kiro rejected account %s, disabling it: %v", req.AuthID, errList)
		fields, metadata = markAccountBlocked(fields, metadata, now)
	default:
		log.Printf("kiro4cpa: listing models for %s failed, keeping stored catalogue: %v", req.AuthID, errList)
	}
	storage, err := mergeCredential(fields, refreshed)
	if err != nil {
		return nil, err
	}
	next := scheduledRefresh(refreshed, now)
	if floor := now.Add(minRefreshDelay); next.Before(floor) {
		next = floor
	}
	disabled, _ := metadata["disabled"].(bool)
	return wire.OK(pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider: models.ProviderName, ID: req.AuthID, Disabled: disabled,
			StorageJSON: storage, Metadata: metadata, NextRefreshAfter: next,
		},
		NextRefreshAfter: next,
	}), nil
}

// scheduledRefresh is expiresAt - refreshLead; an unparsable expiry refreshes now.
func scheduledRefresh(c Credential, now time.Time) time.Time {
	if c.AuthMethod == AuthMethodAPIKey {
		return now.Add(longRefreshInterval)
	}
	expiresAt, err := time.Parse(time.RFC3339, c.ExpiresAt)
	if err != nil {
		return now
	}
	return expiresAt.Add(-refreshLead)
}

// CPA schedules auth.refresh for a plugin provider only when its metadata carries
// refresh_interval_seconds; NextRefreshAfter can delay a refresh but never start one.
const (
	refreshIntervalKey     = "refresh_interval_seconds"
	refreshIntervalSeconds = 1800
)

func authMetadata(c Credential) map[string]any {
	m := map[string]any{refreshIntervalKey: refreshIntervalSeconds}
	if c.Email != "" {
		m["email"] = c.Email
	}
	return m
}

// loginSessionTTL matches the window CPA keeps an OAuth session pending.
const loginSessionTTL = 30 * time.Minute

// handleLoginStart only opens a session; the user picks a login method on the
// connect page, which CPA's OAuth card opens in a new tab.
func handleLoginStart(request []byte) ([]byte, error) {
	var req pluginapi.AuthLoginStartRequest
	if len(request) > 0 {
		_ = json.Unmarshal(request, &req)
	}
	session, err := createLoginSession(loginSessionTTL)
	if err != nil {
		return nil, err
	}
	return wire.OK(pluginapi.AuthLoginStartResponse{
		Provider:  models.ProviderName,
		URL:       connectPageURL(session.State),
		State:     session.State,
		ExpiresAt: session.ExpiresAt,
	}), nil
}

func handleLoginPoll(request []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	session, ok := getLoginSession(req.State)
	switch {
	case !ok:
		return wire.OK(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: errSessionNotFound.Error(),
		}), nil
	case session.Completed != nil:
		return wire.OK(buildLoginSuccessResponse(*session.Completed, session.State)), nil
	case session.Error != "":
		return wire.OK(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: session.Error,
		}), nil
	case session.Method == "":
		return wire.OK(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "等待在 Kiro 登录页选择登录方式",
		}), nil
	}
	return wire.OK(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusPending,
		Message: "等待完成 Kiro 授权",
	}), nil
}

func buildLoginSuccessResponse(cred Credential, state string) pluginapi.AuthLoginPollResponse {
	now := time.Now()
	cred = cred.normalized()
	session, _ := getLoginSession(state)
	// A failed import validation must not silently repeat a one-time refresh.
	if session == nil || (session.Validation != "invalid" && session.Validation != "unverified") {
		cred = resolveAccountIdentity(cred, config.Get(), now)
	}
	// A repeated poll must reuse this identity: a rotated refresh token would
	// fail a second lookup and fall back to a different file name.
	updateLoginSession(state, func(s *LoginSession) {
		if s.Completed != nil {
			resolved := cred
			s.Completed = &resolved
			s.Delivered = true
		}
	})
	raw, err := json.Marshal(cred) // #nosec G117 -- AuthLoginPoll returns credentials only to the trusted CPA host for persistence.
	if err != nil {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: err.Error(),
		}
	}
	fields, _ := decodeFields(raw)
	fields["type"] = models.ProviderName
	storage, err := mergeCredential(fields, cred)
	if err != nil {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: err.Error(),
		}
	}

	fileName := authFileName(cred, state, now)
	label := authLabel(cred, cred.ProfileArn)
	auth := pluginapi.AuthData{
		ID:               fileName,
		FileName:         fileName,
		Provider:         models.ProviderName,
		Label:            label,
		StorageJSON:      storage,
		Metadata:         authMetadata(cred),
		NextRefreshAfter: time.Now(),
	}

	return pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "Kiro device login completed",
		Auth:    auth,
	}
}
