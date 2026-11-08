package kiro

import (
	"encoding/json"
	"errors"
	"github.com/AkkunYo/kiro4cpa-plugin/internal/config"
	"github.com/AkkunYo/kiro4cpa-plugin/internal/hostapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegressionNonstreamRejectsMalformedEvent(t *testing.T) {
	stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		return &hostapi.HTTPResponse{StatusCode: 200, Body: eventFrame("assistantResponseEvent", `{"content":`)}, nil
	})
	raw, _ := json.Marshal(execRequest(pingRequest))
	out, _ := HandleMethod(pluginabi.MethodExecutorExecute, raw)
	if env := decodeEnvelope(t, out); env.OK {
		t.Fatalf("malformed known event returned success: %s", env.Result)
	}
}
func TestRegressionStreamRejectsTruncatedFrame(t *testing.T) {
	frame := eventFrame("assistantResponseEvent", `{"content":"hello"}`)
	s := stubStream(t, &streamStub{status: 200, reads: []hostapi.StreamChunk{{Payload: frame[:len(frame)-3], Done: true}}})
	streamCall(t, pingRequest)
	if msg := waitClosed(t, s); msg == "" {
		t.Fatalf("truncated frame ended successfully; message_stop=%v", strings.Contains(strings.Join(s.emitted, ""), "message_stop"))
	}
}
func TestRegressionRetryCredentialPersistence(t *testing.T) {
	stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		return ok(`{"accessToken":"fresh","refreshToken":"rotated","expiresIn":3600}`)
	})
	saved := 0
	orig := hostAuthSave
	hostAuthSave = func(string, []byte) error {
		saved++
		if saved == 1 {
			return errors.New("temporary disk failure")
		}
		return nil
	}
	t.Cleanup(func() { hostAuthSave = orig })
	req := execRequest(pingRequest)
	cred, _, err := decodeCredential(req.StorageJSON)
	if err != nil {
		t.Fatal(err)
	}
	send := func(c Credential) (string, error) {
		if c.AccessToken != "fresh" {
			return "", classifyUpstream(401, []byte("expired"))
		}
		return "ok", nil
	}
	for range 2 {
		if _, err := withRefresh(req.refreshTarget(), cred, send); err != nil {
			t.Fatal(err)
		}
	}
	if saved < 2 {
		t.Fatalf("persist failed once, but later cached refresh never retries it: saveCalls=%d", saved)
	}
}
func TestRegressionOldGenerationDoesNotSpendRefreshTokenAgain(t *testing.T) {
	resetRefreshes(t)
	calls := 0
	refresh := func(c Credential) (Credential, error) {
		calls++
		c.AccessToken += "+"
		c.RefreshToken += "+"
		return c, nil
	}
	b, _ := sharedRefresh("audit.json", staleCred, refresh)
	_, _ = sharedRefresh("audit.json", b, refresh)
	_, _ = sharedRefresh("audit.json", staleCred, refresh)
	if calls != 2 {
		t.Fatalf("old generation spent refresh token again after two rotations: refreshCalls=%d", calls)
	}
}
func TestRegressionQuotaFallbackPreservesTokenFailure(t *testing.T) {
	refreshCalls := 0
	stubHTTP(t, func(r hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		if strings.Contains(r.URL, "refreshToken") {
			refreshCalls++
			return ok(`{"accessToken":"fresh","refreshToken":"rotated"}`)
		}
		if r.Headers["Authorization"][0] == "Bearer fresh" {
			return ok(`{"usageBreakdownList":[]}`)
		}
		if strings.Contains(r.URL, "us-east-1") {
			return &hostapi.HTTPResponse{StatusCode: 403, Body: []byte("The bearer token included in the request is invalid")}, nil
		}
		return &hostapi.HTTPResponse{StatusCode: 403, Body: []byte("User is not authorized to make this call")}, nil
	})
	orig := hostAuthSave
	hostAuthSave = func(string, []byte) error { return nil }
	t.Cleanup(func() { hostAuthSave = orig })
	cred := staleCred
	cred.AuthRegion = "us-east-1"
	_, err := withRefresh(refreshTarget{authID: "audit.json"}, cred, func(c Credential) ([]byte, error) { return fetchUsageLimits(c, config.Get(), "") })
	if refreshCalls == 0 {
		t.Fatalf("fallback masked expired token, refreshCalls=%d err=%v", refreshCalls, err)
	}
}

func TestRegressionStreamIntegrity(t *testing.T) {
	valid := eventFrame("assistantResponseEvent", `{"content":"hello"}`)
	badCRC := append([]byte(nil), valid...)
	badCRC[len(badCRC)-1] ^= 1
	cases := map[string][]byte{
		"empty":                nil,
		"truncated":            valid[:len(valid)-1],
		"bad CRC":              badCRC,
		"bad JSON":             eventFrame("assistantResponseEvent", `{"content":`),
		"valid then truncated": kiroBody(valid, valid[:len(valid)-1]),
	}
	for name, body := range cases {
		t.Run(name+"/nonstream", func(t *testing.T) {
			stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
				return &hostapi.HTTPResponse{StatusCode: 200, Body: body}, nil
			})
			raw, _ := json.Marshal(execRequest(pingRequest))
			out, _ := handleExecute(raw)
			if env := decodeEnvelope(t, out); env.OK || env.Error.HTTPStatus != 502 {
				t.Fatalf("damaged stream must fail: %s", out)
			}
		})
		t.Run(name+"/stream", func(t *testing.T) {
			s := stubStream(t, &streamStub{status: 200, reads: []hostapi.StreamChunk{{Payload: body, Done: true}}})
			streamCall(t, pingRequest)
			if waitClosed(t, s) == "" || strings.Contains(strings.Join(s.emitted, ""), "message_stop") {
				t.Fatal("damaged stream must close with an error and no message_stop")
			}
		})
	}
}

func TestRegressionRefreshUsesLatestWhenReplacementExpired(t *testing.T) {
	resetRefreshes(t)
	b := staleCred
	b.AccessToken, b.RefreshToken, b.ExpiresAt = "at2", "rt2", time.Now().Add(-time.Minute).Format(time.RFC3339)
	_, _ = sharedRefresh("expiry.json", staleCred, func(Credential) (Credential, error) { return b, nil })
	_, err := sharedRefresh("expiry.json", staleCred, func(c Credential) (Credential, error) {
		if c.RefreshToken != b.RefreshToken {
			t.Fatal("attempted to reuse retired token")
		}
		c.AccessToken, c.RefreshToken = "at3", "rt3"
		return c, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRegressionRefreshHistoryIsBoundedAndRejectsRetiredSnapshot(t *testing.T) {
	resetRefreshes(t)
	first := staleCred
	first.ExpiresAt = time.Now().Add(time.Hour).Format(time.RFC3339)
	c := first
	for i := 0; i < refreshHistoryLimit+5; i++ {
		var err error
		c, err = sharedRefresh("history.json", c, func(c Credential) (Credential, error) {
			c.AccessToken += "+"
			c.RefreshToken += "+"
			expiry, _ := time.Parse(time.RFC3339, c.ExpiresAt)
			c.ExpiresAt = expiry.Add(time.Hour).Format(time.RFC3339)
			return c, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(refreshes.latest["history.json"].history) != refreshHistoryLimit {
		t.Fatal("unbounded history")
	}
	_, err := sharedRefresh("history.json", first, func(Credential) (Credential, error) {
		t.Fatal("retired snapshot reached the refresh endpoint")
		return Credential{}, nil
	})
	if !errors.Is(err, errStaleCredential) {
		t.Fatalf("err=%v", err)
	}
}

func TestRegressionConcurrentRefreshAndSaveAreCoalesced(t *testing.T) {
	resetRefreshes(t)
	var refreshCalls, saveCalls atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := coordinatedRefresh("concurrent.json", staleCred, func(c Credential) (Credential, error) {
				refreshCalls.Add(1)
				c.AccessToken, c.RefreshToken = "at2", "rt2"
				return c, nil
			}, func(Credential) error { saveCalls.Add(1); return nil })
			if err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if refreshCalls.Load() != 1 || saveCalls.Load() != 1 {
		t.Fatalf("refresh=%d save=%d", refreshCalls.Load(), saveCalls.Load())
	}
	fresh, _ := sharedRefresh("concurrent.json", Credential{AccessToken: "at2", RefreshToken: "rt2"}, func(c Credential) (Credential, error) { c.AccessToken, c.RefreshToken = "at3", "rt3"; return c, nil })
	_, err := coordinatedRefresh("concurrent.json", staleCred, func(Credential) (Credential, error) { t.Fatal("old snapshot refreshed"); return Credential{}, nil }, func(c Credential) error {
		if c.AccessToken != fresh.AccessToken {
			t.Fatal("old snapshot overwrote latest tokens")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRegressionFailedSaveRetriedWithWorkingAccessToken(t *testing.T) {
	resetRefreshes(t)
	fresh := staleCred
	fresh.AccessToken = "fresh"
	fresh.RefreshToken = "rotated"
	saves := 0
	orig := hostAuthSave
	hostAuthSave = func(string, []byte) error {
		saves++
		if saves == 1 {
			return errors.New("temporary failure")
		}
		return nil
	}
	t.Cleanup(func() { hostAuthSave = orig })
	target := execRequest(pingRequest).refreshTarget()
	_, _ = coordinatedRefresh(target.authID, staleCred, func(Credential) (Credential, error) { return fresh, nil }, func(c Credential) error { return persistCredential(target, c) })
	calls := 0
	_, err := withRefresh(target, fresh, func(c Credential) (string, error) { calls++; return "ok", nil })
	if err != nil || saves != 2 || calls != 1 {
		t.Fatalf("err=%v saves=%d sends=%d", err, saves, calls)
	}
}

func TestRegressionScheduledRefreshDoesNotQueuePluginSave(t *testing.T) {
	resetRefreshes(t)
	fresh, err := sharedRefresh("scheduled.json", staleCred, func(c Credential) (Credential, error) {
		c.AccessToken, c.RefreshToken = "new-access", "new-refresh"
		return c, nil
	})
	if err != nil || hasPendingRefreshSave("scheduled.json", fresh) || hasPendingRefreshSave("scheduled.json", staleCred) {
		t.Fatalf("host-owned persistence was treated as a failed plugin save: %v", err)
	}
}

func TestRegressionCachedRefreshPreservesCurrentSettings(t *testing.T) {
	resetRefreshes(t)
	fresh, _ := sharedRefresh("settings.json", staleCred, func(c Credential) (Credential, error) {
		c.AccessToken, c.RefreshToken, c.ProfileArn = "new-access", "new-refresh", "new-profile"
		return c, nil
	})
	current := staleCred
	current.DisableCooling, current.APIRegion, current.Email = true, "eu-central-1", "updated@example.com"
	got, err := coordinatedRefresh("settings.json", current, func(Credential) (Credential, error) {
		t.Fatal("spent an old refresh token")
		return Credential{}, nil
	}, func(c Credential) error {
		if !c.DisableCooling || c.APIRegion != current.APIRegion || c.Email != current.Email {
			t.Error("save reverted current account settings")
		}
		return nil
	})
	if err != nil || !got.DisableCooling || got.APIRegion != current.APIRegion || got.Email != current.Email || got.RefreshToken != fresh.RefreshToken || got.ProfileArn != fresh.ProfileArn {
		t.Fatalf("cached tokens must preserve current settings: %+v err=%v", got, err)
	}
}

func TestRegressionRejectedTokenRefreshesDespiteFailedSave(t *testing.T) {
	refreshCalls := 0
	stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		refreshCalls++
		return ok(`{"accessToken":"accepted","refreshToken":"latest","expiresIn":3600}`)
	})
	orig := hostAuthSave
	hostAuthSave = func(string, []byte) error { return errors.New("disk unavailable") }
	t.Cleanup(func() { hostAuthSave = orig })
	target := execRequest(pingRequest).refreshTarget()
	fresh, _ := coordinatedRefresh(target.authID, staleCred, func(c Credential) (Credential, error) {
		c.AccessToken, c.RefreshToken = "rejected", "rotated"
		c.AuthRegion = defaultRegion
		c.ExpiresAt = time.Now().Add(time.Hour).Format(time.RFC3339)
		return c, nil
	}, func(c Credential) error { return persistCredential(target, c) })
	got, err := withRefresh(target, fresh, func(c Credential) (string, error) {
		if c.AccessToken != "accepted" {
			return "", classifyUpstream(401, []byte("invalid token"))
		}
		return "ok", nil
	})
	if err != nil || got != "ok" || refreshCalls != 1 {
		t.Fatalf("rejected cached token was not refreshed: result=%q err=%v refreshes=%d", got, err, refreshCalls)
	}
}
