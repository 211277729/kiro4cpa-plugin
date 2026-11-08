package kiro

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/hostapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The PKCE challenge for the RFC 7636 appendix B verifier.
func TestPKCEChallengeMatchesRFC7636(t *testing.T) {
	if got := pkceChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("challenge %s", got)
	}
}

func TestSocialPortalURL(t *testing.T) {
	u, err := url.Parse(socialPortalURL("st", "ch"))
	if err != nil || u.Host != "app.kiro.dev" || u.Path != "/signin" {
		t.Fatalf("url %v", u)
	}
	q := u.Query()
	if q.Get("state") != "st" || q.Get("code_challenge") != "ch" || q.Get("code_challenge_method") != "S256" ||
		q.Get("redirect_uri") != "http://127.0.0.1:3128" || q.Get("redirect_from") != "KiroIDE" {
		t.Fatalf("query %v", q)
	}
}

func TestParseSocialCallback(t *testing.T) {
	cb, err := parseSocialCallback(" http://127.0.0.1:3128/oauth/callback?code=c1&state=s1&login_option=github ")
	if err != nil || cb.Code != "c1" || cb.State != "s1" || cb.provider() != ProviderGithub {
		t.Fatalf("%+v %v", cb, err)
	}
	if got := cb.fullRedirectURI(); got != "http://127.0.0.1:3128/oauth/callback?login_option=github" {
		t.Fatalf("redirect %s", got)
	}
	plain, _ := parseSocialCallback("http://localhost:3128/signin/callback?code=c&state=s")
	if plain.fullRedirectURI() != "http://127.0.0.1:3128/signin/callback" || plain.provider() != ProviderGoogle {
		t.Fatalf("plain %+v", plain)
	}
	for _, bad := range []string{
		"",
		"not a url",
		"http://127.0.0.1:3128/oauth/callback",
		"http://127.0.0.1:3128/other?code=c&state=s",
		"http://127.0.0.1:3128/oauth/callback?state=s",
		"http://127.0.0.1:3128/oauth/callback?error=access_denied&error_description=denied",
	} {
		if _, err := parseSocialCallback(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestSocialLoginExchangesCodeAndCompletes(t *testing.T) {
	var got map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.Header.Get("User-Agent"), "KiroIDE-") {
			t.Errorf("request %s %s", r.Method, r.Header.Get("User-Agent"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"accessToken":"at","refreshToken":"rt","profileArn":"arn:aws:codewhisperer:us-east-1:1:profile/X","expiresIn":3600}`))
	}))
	defer server.Close()
	orig := socialTokenURL
	socialTokenURL = server.URL
	defer func() { socialTokenURL = orig }()

	state := newSession(t)
	status, start := callRoute(t, http.MethodPost, routeSocialStart, map[string]string{"state": state}, nil)
	if status != http.StatusOK {
		t.Fatalf("start %d %v", status, start)
	}
	portal, _ := url.Parse(start["url"].(string))
	socialState := portal.Query().Get("state")
	session, _ := getLoginSession(state)

	wrong := "http://127.0.0.1:3128/oauth/callback?code=c&state=other&login_option=google"
	if status, _ := callRoute(t, http.MethodPost, routeSocialComplete, map[string]string{"state": state, "callbackUrl": wrong}, nil); status != http.StatusBadRequest {
		t.Fatalf("state mismatch accepted: %d", status)
	}

	callback := "http://127.0.0.1:3128/oauth/callback?code=the-code&state=" + url.QueryEscape(socialState) + "&login_option=google"
	if status, body := callRoute(t, http.MethodPost, routeSocialComplete, map[string]string{"state": state, "callbackUrl": callback}, nil); status != http.StatusOK {
		t.Fatalf("complete %d %v", status, body)
	}
	if got["code"] != "the-code" || got["code_verifier"] != session.CodeVerifier ||
		got["redirect_uri"] != "http://127.0.0.1:3128/oauth/callback?login_option=google" ||
		portal.Query().Get("code_challenge") != pkceChallenge(session.CodeVerifier) {
		t.Fatalf("exchange body %v", got)
	}
	done, _ := getLoginSession(state)
	c := done.Completed
	if c == nil || c.AuthMethod != AuthMethodSocial || c.Provider != ProviderGoogle || c.RefreshToken != "rt" || c.ProfileArn == "" || done.CodeVerifier != "" {
		t.Fatalf("credential %+v verifier %q", c, done.CodeVerifier)
	}
}

func TestSocialCompleteRequiresStart(t *testing.T) {
	state := newSession(t)
	cb := "http://127.0.0.1:3128/oauth/callback?code=c&state=s"
	if status, _ := callRoute(t, http.MethodPost, routeSocialComplete, map[string]string{"state": state, "callbackUrl": cb}, nil); status != http.StatusConflict {
		t.Fatalf("status %d", status)
	}
}

func TestSocialExchangeFailureIsReported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer server.Close()
	orig := socialTokenURL
	socialTokenURL = server.URL
	defer func() { socialTokenURL = orig }()

	state := newSession(t)
	_, start := callRoute(t, http.MethodPost, routeSocialStart, map[string]string{"state": state}, nil)
	portal, _ := url.Parse(start["url"].(string))
	cb := "http://127.0.0.1:3128/oauth/callback?code=c&state=" + url.QueryEscape(portal.Query().Get("state"))
	status, body := callRoute(t, http.MethodPost, routeSocialComplete, map[string]string{"state": state, "callbackUrl": cb}, nil)
	if status != http.StatusBadGateway || !strings.Contains(body["error"].(string), "invalid_grant") {
		t.Fatalf("%d %v", status, body)
	}
	if s, _ := getLoginSession(state); s.Completed != nil {
		t.Fatal("failed exchange completed the session")
	}
}

// The official host polls the outer session; Kiro's full callback is submitted
// on the plugin page so its path and login_option survive the handoff.
func TestSocialLoginOfficialPanelHandoff(t *testing.T) {
	for _, path := range []string{"/oauth/callback", "/signin/callback"} {
		for _, provider := range []string{"google", "github"} {
			t.Run(path+"/"+provider, func(t *testing.T) {
				stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
					return ok(`{"userInfo":{"email":"fixture@example.test","userId":"fixture-user"}}`)
				})
				exchanges := 0
				var exchange map[string]string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					exchanges++
					_ = json.NewDecoder(r.Body).Decode(&exchange)
					_, _ = w.Write([]byte(`{"accessToken":"fixture-at","refreshToken":"fixture-rt","expiresIn":3600}`))
				}))
				defer server.Close()
				original := socialTokenURL
				socialTokenURL = server.URL
				defer func() { socialTokenURL = original }()
				start := callHandler[pluginapi.AuthLoginStartResponse](t, pluginabi.MethodAuthLoginStart, pluginapi.AuthLoginStartRequest{Provider: "kiro4cpa"})
				t.Cleanup(func() { loginSessionsMu.Lock(); delete(loginSessions, start.State); loginSessionsMu.Unlock() })
				if start.URL != connectPageURL(start.State) {
					t.Fatalf("wrong connect URL %q", start.URL)
				}
				_, link := callRoute(t, http.MethodPost, routeSocialStart, map[string]string{"state": start.State}, nil)
				portal, _ := url.Parse(link["url"].(string))
				poll := pluginapi.AuthLoginPollRequest{Provider: "kiro4cpa", State: start.State}
				before := callHandler[pluginapi.AuthLoginPollResponse](t, pluginabi.MethodAuthLoginPoll, poll)
				if before.Status != pluginapi.AuthLoginStatusPending {
					t.Fatalf("premature result: %s", before.Status)
				}
				callback := socialRedirectURI + path + "?code=fixture-code&state=" + url.QueryEscape(portal.Query().Get("state")) + "&login_option=" + provider
				status, _ := callRoute(t, http.MethodPost, routeSocialComplete, map[string]string{"state": start.State, "callbackUrl": callback}, nil)
				if status != http.StatusOK || exchange["redirect_uri"] != socialRedirectURI+path+"?login_option="+provider {
					t.Fatalf("callback status %d, wrong redirect URI", status)
				}
				if pkceChallenge(exchange["code_verifier"]) != portal.Query().Get("code_challenge") {
					t.Fatal("PKCE verifier mismatch")
				}
				first := callHandler[pluginapi.AuthLoginPollResponse](t, pluginabi.MethodAuthLoginPoll, poll)
				second := callHandler[pluginapi.AuthLoginPollResponse](t, pluginabi.MethodAuthLoginPoll, poll)
				if first.Status != pluginapi.AuthLoginStatusSuccess || first.Auth.Provider != "kiro4cpa" || first.Auth.FileName != "kiro4cpa-"+provider+"-fixture@example.test.json" || first.Auth.ID != second.Auth.ID {
					t.Fatal("unstable host credential handoff")
				}
				if string(first.Auth.StorageJSON) != string(second.Auth.StorageJSON) {
					t.Fatal("repeated poll changed credentials")
				}
				_, public := callRoute(t, http.MethodGet, routeStatus, nil, url.Values{"state": {start.State}})
				data, _ := json.Marshal(public)
				if public["delivered"] != true || strings.Contains(string(data), "fixture-at") || strings.Contains(string(data), "fixture-rt") {
					t.Fatal("public status leaked tokens or lost delivery state")
				}
				status, _ = callRoute(t, http.MethodPost, routeSocialComplete, map[string]string{"state": start.State, "callbackUrl": callback}, nil)
				if status != http.StatusConflict || exchanges != 1 {
					t.Fatalf("authorization code was reused: status=%d exchanges=%d", status, exchanges)
				}
			})
		}
	}
}
