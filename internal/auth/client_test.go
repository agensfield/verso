package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeviceLoginPollsAndReturnsNativeCredentials(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	idToken := testJWT("user-1", "account-1", now.Add(time.Hour))
	var polls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			requireJSONField(t, r, "client_id", DefaultClientID)
			writeJSON(t, w, http.StatusOK, map[string]any{
				"device_auth_id": "device-secret", "usercode": "ABCD-EFGH",
				"interval": "2", "expires_in": 30,
			})
		case "/api/accounts/deviceauth/token":
			polls++
			requireJSONField(t, r, "device_auth_id", "device-secret")
			if polls == 1 {
				writeJSON(t, w, http.StatusForbidden, map[string]any{"error": "authorization_pending"})
				return
			}
			writeJSON(t, w, http.StatusOK, map[string]any{
				"authorization_code": "authorization-secret", "code_verifier": "verifier-secret",
				"code_challenge": "ignored",
			})
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse form: %v", err)
				return
			}
			for key, want := range map[string]string{
				"grant_type": "authorization_code", "code": "authorization-secret",
				"code_verifier": "verifier-secret", "client_id": DefaultClientID,
				"redirect_uri": serverURL(r) + "/deviceauth/callback",
			} {
				if got := r.Form.Get(key); got != want {
					t.Errorf("form %s = %q, want %q", key, got, want)
				}
			}
			writeJSON(t, w, http.StatusOK, tokenResponse{
				IDToken: idToken, AccessToken: "access-secret", RefreshToken: "refresh-secret",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient(Config{
		IssuerURL: server.URL, Now: func() time.Time { return now },
		Sleep: func(ctx context.Context, delay time.Duration) error {
			if delay != 2*time.Second {
				t.Fatalf("poll delay = %s, want 2s", delay)
			}
			now = now.Add(delay)
			return nil
		},
	})
	var prompt DevicePrompt
	raw, err := client.DeviceLogin(context.Background(), func(value DevicePrompt) error {
		prompt = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if prompt.VerificationURL != server.URL+"/codex/device" || prompt.UserCode != "ABCD-EFGH" {
		t.Fatalf("unexpected prompt: %#v", prompt)
	}
	if !prompt.ExpiresAt.Equal(time.Date(2026, 9, 11, 12, 0, 30, 0, time.UTC)) {
		t.Fatalf("expiry = %s", prompt.ExpiresAt)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var tokens map[string]string
	if err := json.Unmarshal(doc["tokens"], &tokens); err != nil {
		t.Fatal(err)
	}
	if tokens["id_token"] != idToken || tokens["access_token"] != "access-secret" ||
		tokens["refresh_token"] != "refresh-secret" || tokens["account_id"] != "account-1" {
		t.Fatalf("unexpected returned tokens")
	}
}

func TestDeviceLoginExpiryAndCancellation(t *testing.T) {
	t.Run("expiry", func(t *testing.T) {
		now := time.Unix(1_800_000_000, 0)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/usercode") {
				writeJSON(t, w, http.StatusOK, map[string]any{
					"device_auth_id": "device", "user_code": "code", "interval": "4", "expires_in": 5,
				})
				return
			}
			w.WriteHeader(http.StatusForbidden)
		}))
		defer server.Close()
		client := NewClient(Config{
			IssuerURL: server.URL, Now: func() time.Time { return now },
			Sleep: func(context.Context, time.Duration) error { now = now.Add(4 * time.Second); return nil },
		})
		_, err := client.DeviceLogin(context.Background(), func(DevicePrompt) error { return nil })
		if !errors.Is(err, ErrDeviceExpired) {
			t.Fatalf("error = %v, want ErrDeviceExpired", err)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/usercode") {
				writeJSON(t, w, http.StatusOK, map[string]any{
					"device_auth_id": "device", "user_code": "code", "interval": "1",
				})
				return
			}
			writeJSON(t, w, http.StatusForbidden, map[string]any{"status": "pending"})
		}))
		defer server.Close()
		ctx, cancel := context.WithCancel(context.Background())
		client := NewClient(Config{IssuerURL: server.URL})
		_, err := client.DeviceLogin(ctx, func(DevicePrompt) error { cancel(); return nil })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context cancellation", err)
		}
	})
}

func TestRefreshPreservesOpaqueAndOmittedFields(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 30, 0, 123, time.UTC)
	raw := testAuth(t, "user-1", "account-1", map[string]any{
		"access_token": "old-access", "refresh_token": "old-refresh", "opaque_token": map[string]any{"x": 1},
	}, map[string]any{"opaque_top": []string{"kept"}, "last_refresh": "old"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireJSONField(t, r, "refresh_token", "old-refresh")
		writeJSON(t, w, http.StatusOK, map[string]any{"access_token": "new-access"})
	}))
	defer server.Close()
	client := NewClient(Config{IssuerURL: server.URL, Now: func() time.Time { return now }})
	updated, err := client.Refresh(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(updated, &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc["opaque_top"]) != `["kept"]` {
		t.Fatalf("opaque top-level field changed: %s", doc["opaque_top"])
	}
	var tokens map[string]json.RawMessage
	if err := json.Unmarshal(doc["tokens"], &tokens); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"access_token": "new-access", "refresh_token": "old-refresh"} {
		if got, _ := stringField(tokens, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if string(tokens["opaque_token"]) != `{"x":1}` {
		t.Fatalf("opaque token field changed: %s", tokens["opaque_token"])
	}
	if got, _ := stringField(doc, "last_refresh"); got != now.Format(time.RFC3339Nano) {
		t.Fatalf("last_refresh = %q", got)
	}
}

func TestRefreshAcceptsNativeAccountIDOutsideIDToken(t *testing.T) {
	raw := testAuth(t, "user-1", "account-1", map[string]any{"refresh_token": "old-refresh"}, nil)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	tokens := doc["tokens"].(map[string]any)
	tokens["id_token"] = testJWT("user-1", "", time.Unix(1_900_000_000, 0))
	raw, _ = json.Marshal(doc)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id_token": testJWT("user-1", "", time.Unix(1_900_000_100, 0)),
		})
	}))
	defer server.Close()
	if _, err := NewClient(Config{IssuerURL: server.URL}).Refresh(context.Background(), raw); err != nil {
		t.Fatalf("refresh rejected native token-level account ID: %v", err)
	}
}

func TestRefreshRejectsLoginRequiredAndIdentityMismatchWithoutLeaking(t *testing.T) {
	raw := testAuth(t, "user-1", "account-1", map[string]any{"refresh_token": "refresh-secret"}, nil)
	tests := []struct {
		name     string
		response any
		status   int
		want     error
	}{
		{"invalid grant", map[string]any{"error": "invalid_grant", "error_description": "refresh-secret leaked"}, http.StatusBadRequest, ErrLoginRequired},
		{"unauthorized", "refresh-secret body", http.StatusUnauthorized, ErrLoginRequired},
		{"identity mismatch", tokenResponse{IDToken: testJWT("user-2", "account-1", time.Now())}, http.StatusOK, ErrIdentityMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if text, ok := test.response.(string); ok {
					w.WriteHeader(test.status)
					_, _ = io.WriteString(w, text)
					return
				}
				writeJSON(t, w, test.status, test.response)
			}))
			defer server.Close()
			_, err := NewClient(Config{IssuerURL: server.URL}).Refresh(context.Background(), raw)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if strings.Contains(fmt.Sprint(err), "refresh-secret") {
				t.Fatalf("error leaked credential: %v", err)
			}
		})
	}
}

func TestUsageParsesQuotaAndSendsNativeHeadersWithoutRefresh(t *testing.T) {
	now := time.Date(2026, 9, 11, 13, 0, 0, 0, time.UTC)
	raw := testAuth(t, "user-1", "account-1", map[string]any{"access_token": "access-secret"}, nil)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/wham/usage" || r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer access-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-Id"); got != "account-1" {
			t.Errorf("ChatGPT-Account-Id = %q", got)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"plan_type": "plus",
			"rate_limit": map[string]any{
				"allowed": false, "limit_reached": true,
				"primary_window": map[string]any{
					"used_percent": 100, "limit_window_seconds": 18000,
					"reset_after_seconds": 60, "reset_at": 1_800_000_000,
				},
				"secondary_window": map[string]any{"used_percent": 42.5},
			},
		})
	}))
	defer server.Close()
	quota, err := NewClient(Config{BackendURL: server.URL, Now: func() time.Time { return now }}).Usage(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || quota.Plan == nil || *quota.Plan != "plus" || quota.Exhausted == nil || !*quota.Exhausted {
		t.Fatalf("unexpected quota: %#v", quota)
	}
	if quota.Primary == nil || quota.Primary.UsedPercent == nil || *quota.Primary.UsedPercent != 100 ||
		quota.Primary.Window == nil || *quota.Primary.Window != 5*time.Hour || quota.Primary.ResetsAt == nil {
		t.Fatalf("unexpected primary window: %#v", quota.Primary)
	}
	if quota.Secondary == nil || quota.Secondary.UsedPercent == nil || quota.Secondary.Window != nil {
		t.Fatalf("absence was not retained: %#v", quota.Secondary)
	}
	if !quota.ObservedAt.Equal(now) {
		t.Fatalf("observed at = %s", quota.ObservedAt)
	}
}

func TestUsageStatusAndInvalidJSONAreSanitized(t *testing.T) {
	raw := testAuth(t, "user-1", "account-1", map[string]any{"access_token": "access-secret"}, nil)
	for _, test := range []struct {
		name   string
		status int
		want   error
	}{
		{"unauthorized", http.StatusUnauthorized, ErrLoginRequired},
		{"rate limited", http.StatusTooManyRequests, ErrRateLimited},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, "access-secret server body")
			}))
			defer server.Close()
			_, err := NewClient(Config{BackendURL: server.URL}).Usage(context.Background(), raw)
			if !errors.Is(err, test.want) || strings.Contains(fmt.Sprint(err), "server body") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	t.Run("successful non-JSON", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "not JSON access-secret")
		}))
		defer server.Close()
		_, err := NewClient(Config{BackendURL: server.URL}).Usage(context.Background(), raw)
		if err == nil || strings.Contains(fmt.Sprint(err), "access-secret") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestUsageRetainsUnavailableQuota(t *testing.T) {
	raw := testAuth(t, "user-1", "account-1", map[string]any{"access_token": "access-secret"}, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"rate_limit": nil})
	}))
	defer server.Close()
	quota, err := NewClient(Config{BackendURL: server.URL}).Usage(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if quota.Plan != nil || quota.Primary != nil || quota.Secondary != nil || quota.Exhausted != nil {
		t.Fatalf("unavailable quota gained invented values: %#v", quota)
	}
}

func TestRedirectsAreNotFollowedAndResponseSizeIsBounded(t *testing.T) {
	raw := testAuth(t, "user-1", "account-1", map[string]any{"access_token": "secret"}, nil)
	redirectTargetHit := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirectTargetHit = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	_, err := NewClient(Config{BackendURL: redirect.URL}).Usage(context.Background(), raw)
	if err == nil || redirectTargetHit {
		t.Fatalf("redirect result: err=%v targetHit=%v", err, redirectTargetHit)
	}

	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 17))
	}))
	defer large.Close()
	_, err = NewClient(Config{BackendURL: large.URL, MaxBody: 16}).Usage(context.Background(), raw)
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversize error = %v", err)
	}
}

func TestNetworkErrorIsClassifiableAndSanitized(t *testing.T) {
	raw := testAuth(t, "user-1", "account-1", map[string]any{"access_token": "secret"}, nil)
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("transport accidentally included secret")
	})}
	_, err := NewClient(Config{HTTPClient: httpClient}).Usage(context.Background(), raw)
	if !errors.Is(err, ErrNetwork) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("network error = %v", err)
	}
}

func TestAccessTokenExpiry(t *testing.T) {
	expires := time.Unix(1_800_000_000, 0).UTC()
	raw := testAuth(t, "user-1", "account-1", map[string]any{
		"access_token": testJWT("user-1", "account-1", expires),
	}, nil)
	got, known, err := AccessTokenExpiry(raw)
	if err != nil || !known || !got.Equal(expires) {
		t.Fatalf("expiry = %s, %v, %v", got, known, err)
	}
	expired, err := AccessTokenExpired(raw, expires)
	if err != nil || !expired {
		t.Fatalf("expired = %v, %v", expired, err)
	}
}

func testAuth(t *testing.T, userID, accountID string, tokenFields, topFields map[string]any) []byte {
	t.Helper()
	tokens := map[string]any{
		"id_token": testJWT(userID, accountID, time.Unix(1_900_000_000, 0)), "account_id": accountID,
	}
	for key, value := range tokenFields {
		tokens[key] = value
	}
	doc := map[string]any{"auth_mode": "chatgpt", "tokens": tokens}
	for key, value := range topFields {
		doc[key] = value
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func testJWT(userID, accountID string, expires time.Time) string {
	claims := map[string]any{
		"exp": expires.Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id": userID, "chatgpt_account_id": accountID,
		},
	}
	header, _ := json.Marshal(map[string]string{"alg": "none"})
	payload, _ := json.Marshal(claims)
	encode := base64.RawURLEncoding.EncodeToString
	return encode(header) + "." + encode(payload) + ".signature"
}

func requireJSONField(t *testing.T, r *http.Request, key, want string) {
	t.Helper()
	defer r.Body.Close()
	var values map[string]string
	if err := json.NewDecoder(r.Body).Decode(&values); err != nil {
		t.Errorf("decode request: %v", err)
		return
	}
	if got := values[key]; got != want {
		t.Errorf("%s = %q, want %q", key, got, want)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
