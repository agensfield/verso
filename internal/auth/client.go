// Package auth implements the network-only parts of Codex account enrollment,
// refresh, and quota inspection. It never reads, writes, or activates a Codex
// home.
package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultIssuerURL  = "https://auth.openai.com"
	DefaultBackendURL = "https://chatgpt.com/backend-api"
	DefaultClientID   = "app_EMoamEEZ73f0CkXaXp7hrann"

	defaultTimeout      = 30 * time.Second
	defaultDeviceExpiry = 15 * time.Minute
	defaultPollInterval = 5 * time.Second
	defaultMaxBody      = 1 << 20
)

var (
	ErrDeviceExpired    = errors.New("device authorization expired")
	ErrLoginRequired    = errors.New("login required")
	ErrRateLimited      = errors.New("quota request rate limited")
	ErrIdentityMismatch = errors.New("refreshed credential identity changed")
	ErrInvalidAuth      = errors.New("invalid native auth document")
	ErrNetwork          = errors.New("network request failed")
)

// DevicePrompt is the only value through which a device code and its URL leave
// this package. Callers should display it directly to the human who initiated
// enrollment.
type DevicePrompt struct {
	VerificationURL string
	UserCode        string
	ExpiresAt       time.Time
}

// Window retains absent fields instead of manufacturing zero usage or reset
// values.
type Window struct {
	UsedPercent *float64
	Window      *time.Duration
	ResetAfter  *time.Duration
	ResetsAt    *time.Time
}

// Quota is the ordinary Codex quota observation. Exhausted is nil when the
// backend did not provide an authoritative allowed/limit-reached decision.
type Quota struct {
	Primary    *Window
	Secondary  *Window
	ObservedAt time.Time
	Plan       *string
	Exhausted  *bool
}

// Config supplies test seams without making production URLs mutable globals.
type Config struct {
	HTTPClient *http.Client
	IssuerURL  string
	BackendURL string
	ClientID   string
	Timeout    time.Duration
	MaxBody    int64
	Now        func() time.Time
	Sleep      func(context.Context, time.Duration) error
}

type Client struct {
	http     *http.Client
	issuer   string
	backend  string
	clientID string
	timeout  time.Duration
	maxBody  int64
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
}

func NewClient(cfg Config) *Client {
	httpClient := http.DefaultClient
	if cfg.HTTPClient != nil {
		httpClient = cfg.HTTPClient
	}
	clientCopy := *httpClient
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	issuer := strings.TrimRight(cfg.IssuerURL, "/")
	if issuer == "" {
		issuer = DefaultIssuerURL
	}
	backend := strings.TrimRight(cfg.BackendURL, "/")
	if backend == "" {
		backend = DefaultBackendURL
	}
	clientID := cfg.ClientID
	if clientID == "" {
		clientID = DefaultClientID
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	maxBody := cfg.MaxBody
	if maxBody <= 0 {
		maxBody = defaultMaxBody
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	sleep := cfg.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	return &Client{
		http: &clientCopy, issuer: issuer, backend: backend, clientID: clientID,
		timeout: timeout, maxBody: maxBody, now: now, sleep: sleep,
	}
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// DeviceLogin enrolls a new account and returns a native auth document to the
// caller. It does not install or store the credentials.
func (c *Client) DeviceLogin(ctx context.Context, onCode func(DevicePrompt) error) ([]byte, error) {
	if onCode == nil {
		return nil, errors.New("device prompt callback is required")
	}
	var issued struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		UserCodeAlt  string          `json:"usercode"`
		Interval     json.RawMessage `json:"interval"`
		ExpiresIn    *int64          `json:"expires_in"`
	}
	if err := c.jsonRequest(ctx, http.MethodPost, c.issuer+"/api/accounts/deviceauth/usercode",
		map[string]string{"client_id": c.clientID}, nil, &issued); err != nil {
		return nil, fmt.Errorf("request device authorization: %w", err)
	}
	if issued.UserCode == "" {
		issued.UserCode = issued.UserCodeAlt
	}
	if issued.DeviceAuthID == "" || issued.UserCode == "" {
		return nil, errors.New("device authorization response is incomplete")
	}
	interval, err := parseInterval(issued.Interval)
	if err != nil {
		return nil, errors.New("device authorization response has invalid interval")
	}
	if interval <= 0 {
		interval = defaultPollInterval
	}
	expiresIn := defaultDeviceExpiry
	if issued.ExpiresIn != nil && *issued.ExpiresIn > 0 {
		expiresIn = time.Duration(*issued.ExpiresIn) * time.Second
	}
	expiresAt := c.now().Add(expiresIn)
	if err := onCode(DevicePrompt{
		VerificationURL: c.issuer + "/codex/device",
		UserCode:        issued.UserCode,
		ExpiresAt:       expiresAt,
	}); err != nil {
		return nil, err
	}

	var authorizationCode, verifier string
	for {
		if !c.now().Before(expiresAt) {
			return nil, ErrDeviceExpired
		}
		var polled struct {
			AuthorizationCode string `json:"authorization_code"`
			CodeVerifier      string `json:"code_verifier"`
			Status            string `json:"status"`
			Error             any    `json:"error"`
			ErrorCode         string `json:"error_code"`
			Code              string `json:"code"`
		}
		status, err := c.jsonRequestStatus(ctx, http.MethodPost,
			c.issuer+"/api/accounts/deviceauth/token",
			map[string]string{"device_auth_id": issued.DeviceAuthID, "user_code": issued.UserCode}, nil, &polled)
		if err != nil {
			return nil, fmt.Errorf("poll device authorization: %w", err)
		}
		code := responseErrorCode(polled.Error, polled.ErrorCode, polled.Code)
		pending := status == http.StatusForbidden || status == http.StatusNotFound ||
			code == "authorization_pending" || code == "slow_down" ||
			strings.EqualFold(polled.Status, "pending") || strings.EqualFold(polled.Status, "authorization_pending")
		if status >= 200 && status < 300 && polled.AuthorizationCode != "" {
			if polled.CodeVerifier == "" {
				return nil, errors.New("device authorization response is incomplete")
			}
			authorizationCode, verifier = polled.AuthorizationCode, polled.CodeVerifier
			break
		}
		if !pending {
			return nil, statusError("device authorization", status)
		}
		delay := interval
		remaining := expiresAt.Sub(c.now())
		if delay > remaining {
			delay = remaining
		}
		if err := c.sleep(ctx, delay); err != nil {
			return nil, err
		}
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authorizationCode},
		"redirect_uri":  {c.issuer + "/deviceauth/callback"},
		"client_id":     {c.clientID},
		"code_verifier": {verifier},
	}
	var tokens tokenResponse
	if err := c.formRequest(ctx, c.issuer+"/oauth/token", form, &tokens); err != nil {
		return nil, fmt.Errorf("exchange device authorization: %w", err)
	}
	if tokens.IDToken == "" || tokens.AccessToken == "" || tokens.RefreshToken == "" {
		return nil, errors.New("token response is incomplete")
	}
	identity, err := identityFromToken(tokens.IDToken)
	if err != nil || identity.userID == "" || identity.accountID == "" {
		return nil, errors.New("token response contains an invalid ID token")
	}
	doc := map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"id_token": tokens.IDToken, "access_token": tokens.AccessToken,
			"refresh_token": tokens.RefreshToken, "account_id": identity.accountID,
		},
		"last_refresh": c.now().UTC().Format(time.RFC3339Nano),
	}
	return json.Marshal(doc)
}

// Refresh returns an updated copy of rawAuth. Unknown top-level and token
// fields are retained, as are token fields omitted by the refresh response.
func (c *Client) Refresh(ctx context.Context, rawAuth []byte) ([]byte, error) {
	doc, tokenMap, oldIdentity, err := parseAuth(rawAuth)
	if err != nil {
		return nil, err
	}
	refreshToken, ok := stringField(tokenMap, "refresh_token")
	if !ok || refreshToken == "" {
		return nil, ErrLoginRequired
	}
	var refreshed tokenResponse
	status, code, err := c.refreshRequest(ctx, refreshToken, &refreshed)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || code == "invalid_grant" || strings.HasPrefix(code, "refresh_token_") {
		return nil, ErrLoginRequired
	}
	if status < 200 || status >= 300 {
		return nil, statusError("refresh", status)
	}
	if refreshed.IDToken != "" {
		newIdentity, err := identityFromToken(refreshed.IDToken)
		if newIdentity.accountID == "" {
			newIdentity.accountID = oldIdentity.accountID
		}
		if err != nil || oldIdentity != newIdentity {
			return nil, ErrIdentityMismatch
		}
		tokenMap["id_token"] = mustJSON(refreshed.IDToken)
	}
	if refreshed.AccessToken != "" {
		tokenMap["access_token"] = mustJSON(refreshed.AccessToken)
	}
	if refreshed.RefreshToken != "" {
		tokenMap["refresh_token"] = mustJSON(refreshed.RefreshToken)
	}
	doc["tokens"] = mustJSON(tokenMap)
	doc["last_refresh"] = mustJSON(c.now().UTC().Format(time.RFC3339Nano))
	return json.Marshal(doc)
}

// Usage fetches quota with the supplied access token. It never refreshes.
func (c *Client) Usage(ctx context.Context, rawAuth []byte) (Quota, error) {
	_, tokens, _, err := parseAuth(rawAuth)
	if err != nil {
		return Quota{}, err
	}
	accessToken, ok := stringField(tokens, "access_token")
	if !ok || accessToken == "" {
		return Quota{}, ErrLoginRequired
	}
	headers := http.Header{"Authorization": {"Bearer " + accessToken}, "User-Agent": {"codex-cli"}}
	if accountID, ok := stringField(tokens, "account_id"); ok && accountID != "" {
		headers.Set("ChatGPT-Account-Id", accountID)
	}
	var payload quotaPayload
	status, err := c.jsonRequestStatus(ctx, http.MethodGet, c.backend+"/wham/usage", nil, headers, &payload)
	if err != nil {
		return Quota{}, fmt.Errorf("fetch quota: %w", err)
	}
	switch status {
	case http.StatusUnauthorized:
		return Quota{}, ErrLoginRequired
	case http.StatusTooManyRequests:
		return Quota{}, ErrRateLimited
	}
	if status < 200 || status >= 300 {
		return Quota{}, statusError("quota", status)
	}
	quota := Quota{ObservedAt: c.now().UTC(), Plan: payload.PlanType}
	if payload.RateLimit != nil {
		quota.Primary = payload.RateLimit.Primary.toWindow()
		quota.Secondary = payload.RateLimit.Secondary.toWindow()
		switch {
		case payload.RateLimit.Allowed != nil:
			value := !*payload.RateLimit.Allowed
			quota.Exhausted = &value
		case payload.RateLimit.LimitReached != nil:
			value := *payload.RateLimit.LimitReached
			quota.Exhausted = &value
		}
	}
	return quota, nil
}

// AccessTokenExpiry reads JWT metadata without validating its signature.
func AccessTokenExpiry(rawAuth []byte) (time.Time, bool, error) {
	_, tokens, _, err := parseAuth(rawAuth)
	if err != nil {
		return time.Time{}, false, err
	}
	token, ok := stringField(tokens, "access_token")
	if !ok || token == "" {
		return time.Time{}, false, ErrLoginRequired
	}
	claims, err := jwtClaims(token)
	if err != nil {
		return time.Time{}, false, errors.New("access token has invalid JWT metadata")
	}
	value, ok := claims["exp"]
	if !ok {
		return time.Time{}, false, nil
	}
	var seconds json.Number
	if err := json.Unmarshal(value, &seconds); err != nil {
		return time.Time{}, false, errors.New("access token has invalid expiry metadata")
	}
	unix, err := seconds.Int64()
	if err != nil {
		return time.Time{}, false, errors.New("access token has invalid expiry metadata")
	}
	return time.Unix(unix, 0).UTC(), true, nil
}

func AccessTokenExpired(rawAuth []byte, now time.Time) (bool, error) {
	expiresAt, known, err := AccessTokenExpiry(rawAuth)
	return known && !now.Before(expiresAt), err
}

type tokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

type quotaPayload struct {
	PlanType  *string           `json:"plan_type"`
	RateLimit *rateLimitPayload `json:"rate_limit"`
}

type rateLimitPayload struct {
	Allowed      *bool         `json:"allowed"`
	LimitReached *bool         `json:"limit_reached"`
	Primary      windowPayload `json:"primary_window"`
	Secondary    windowPayload `json:"secondary_window"`
}

type windowPayload struct {
	UsedPercent       *float64 `json:"used_percent"`
	WindowSeconds     *int64   `json:"limit_window_seconds"`
	ResetAfterSeconds *int64   `json:"reset_after_seconds"`
	ResetAt           *int64   `json:"reset_at"`
}

func (w windowPayload) toWindow() *Window {
	if w.UsedPercent == nil && w.WindowSeconds == nil && w.ResetAfterSeconds == nil && w.ResetAt == nil {
		return nil
	}
	result := &Window{UsedPercent: w.UsedPercent}
	if w.WindowSeconds != nil {
		value := time.Duration(*w.WindowSeconds) * time.Second
		result.Window = &value
	}
	if w.ResetAfterSeconds != nil {
		value := time.Duration(*w.ResetAfterSeconds) * time.Second
		result.ResetAfter = &value
	}
	if w.ResetAt != nil {
		value := time.Unix(*w.ResetAt, 0).UTC()
		result.ResetsAt = &value
	}
	return result
}

type identity struct{ userID, accountID string }

func parseAuth(raw []byte) (map[string]json.RawMessage, map[string]json.RawMessage, identity, error) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil || doc == nil {
		return nil, nil, identity{}, ErrInvalidAuth
	}
	var tokens map[string]json.RawMessage
	if json.Unmarshal(doc["tokens"], &tokens) != nil || tokens == nil {
		return nil, nil, identity{}, ErrInvalidAuth
	}
	idToken, ok := stringField(tokens, "id_token")
	if !ok || idToken == "" {
		return nil, nil, identity{}, ErrInvalidAuth
	}
	id, err := identityFromToken(idToken)
	if err != nil {
		return nil, nil, identity{}, ErrInvalidAuth
	}
	if tokenAccount, ok := stringField(tokens, "account_id"); ok && tokenAccount != "" {
		if id.accountID != "" && id.accountID != tokenAccount {
			return nil, nil, identity{}, ErrInvalidAuth
		}
		id.accountID = tokenAccount
	}
	if id.userID == "" || id.accountID == "" {
		return nil, nil, identity{}, ErrInvalidAuth
	}
	return doc, tokens, id, nil
}

func identityFromToken(token string) (identity, error) {
	claims, err := jwtClaims(token)
	if err != nil {
		return identity{}, err
	}
	var authClaims map[string]json.RawMessage
	if json.Unmarshal(claims["https://api.openai.com/auth"], &authClaims) != nil {
		return identity{}, errors.New("identity claims missing")
	}
	userID, _ := stringField(authClaims, "chatgpt_user_id")
	if userID == "" {
		userID, _ = stringField(authClaims, "user_id")
	}
	accountID, _ := stringField(authClaims, "chatgpt_account_id")
	return identity{userID: userID, accountID: accountID}, nil
}

func jwtClaims(token string) (map[string]json.RawMessage, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, errors.New("invalid JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]json.RawMessage
	if json.Unmarshal(payload, &claims) != nil || claims == nil {
		return nil, errors.New("invalid JWT claims")
	}
	return claims, nil
}

func stringField(values map[string]json.RawMessage, key string) (string, bool) {
	var value string
	err := json.Unmarshal(values[key], &value)
	return value, err == nil
}

func mustJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

func parseInterval(raw json.RawMessage) (time.Duration, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		seconds, err := time.ParseDuration(strings.TrimSpace(text) + "s")
		return seconds, err
	}
	var seconds int64
	if err := json.Unmarshal(raw, &seconds); err != nil {
		return 0, err
	}
	return time.Duration(seconds) * time.Second, nil
}

func responseErrorCode(value any, alternatives ...string) string {
	if text, ok := value.(string); ok && text != "" {
		return strings.ToLower(text)
	}
	if object, ok := value.(map[string]any); ok {
		for _, key := range []string{"code", "error"} {
			if text, ok := object[key].(string); ok && text != "" {
				return strings.ToLower(text)
			}
		}
	}
	for _, value := range alternatives {
		if value != "" {
			return strings.ToLower(value)
		}
	}
	return ""
}

func (c *Client) refreshRequest(ctx context.Context, refreshToken string, out *tokenResponse) (int, string, error) {
	body, err := json.Marshal(map[string]string{
		"client_id": c.clientID, "grant_type": "refresh_token", "refresh_token": refreshToken,
	})
	if err != nil {
		return 0, "", err
	}
	status, raw, err := c.do(ctx, http.MethodPost, c.issuer+"/oauth/token", "application/json", body, nil)
	if err != nil {
		return 0, "", fmt.Errorf("refresh request failed: %w", err)
	}
	var envelope struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Error        any    `json:"error"`
		Code         string `json:"code"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		if status >= 200 && status < 300 {
			return status, "", errors.New("refresh response is not valid JSON")
		}
		return status, "", nil
	}
	*out = tokenResponse{envelope.IDToken, envelope.AccessToken, envelope.RefreshToken}
	return status, responseErrorCode(envelope.Error, envelope.Code), nil
}

func (c *Client) jsonRequest(ctx context.Context, method, endpoint string, body any, headers http.Header, out any) error {
	status, err := c.jsonRequestStatus(ctx, method, endpoint, body, headers, out)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return statusError("request", status)
	}
	return nil
}

func (c *Client) jsonRequestStatus(ctx context.Context, method, endpoint string, body any, headers http.Header, out any) (int, error) {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return 0, err
		}
	}
	contentType := ""
	if body != nil {
		contentType = "application/json"
	}
	status, raw, err := c.do(ctx, method, endpoint, contentType, encoded, headers)
	if err != nil {
		return 0, err
	}
	if len(bytes.TrimSpace(raw)) != 0 && json.Unmarshal(raw, out) != nil {
		if status >= 200 && status < 300 {
			return status, errors.New("server response is not valid JSON")
		}
	}
	return status, nil
}

func (c *Client) formRequest(ctx context.Context, endpoint string, form url.Values, out any) error {
	status, raw, err := c.do(ctx, http.MethodPost, endpoint, "application/x-www-form-urlencoded", []byte(form.Encode()), nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return statusError("token exchange", status)
	}
	if json.Unmarshal(raw, out) != nil {
		return errors.New("token response is not valid JSON")
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, endpoint, contentType string, body []byte, headers http.Header) (int, []byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, errors.New("build HTTP request")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctxErr := requestCtx.Err(); ctxErr != nil {
			return 0, nil, ctxErr
		}
		return 0, nil, ErrNetwork
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return resp.StatusCode, nil, statusError("redirect refused", resp.StatusCode)
	}
	limited := io.LimitReader(resp.Body, c.maxBody+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return 0, nil, ErrNetwork
	}
	if int64(len(raw)) > c.maxBody {
		return 0, nil, errors.New("HTTP response exceeds size limit")
	}
	return resp.StatusCode, raw, nil
}

func statusError(operation string, status int) error {
	return fmt.Errorf("%s failed with HTTP status %d", operation, status)
}
