package accounts

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrAPIKeyAuth          = errors.New("API-key-only auth is not supported")
	ErrUnsupportedAuth     = errors.New("unsupported auth mode")
	ErrNativeIdentity      = errors.New("native ChatGPT identity is incomplete")
	ErrConflictingIdentity = errors.New("native ChatGPT account IDs conflict")
)

// NativeAuth is a parsed native Codex ChatGPT credential document. Identity
// fields are metadata decoded from the ID token without signature validation;
// callers must not treat them as cryptographic proof.
//
// The original credential document is deliberately kept private. It can only
// leave the package through Store.Credentials after enrollment.
type NativeAuth struct {
	Email     string
	UserID    string
	AccountID string
	raw       json.RawMessage
}

func (a NativeAuth) String() string {
	return fmt.Sprintf("NativeAuth{Email:%q UserID:%q AccountID:%q credentials:[redacted]}", a.Email, a.UserID, a.AccountID)
}

func (a NativeAuth) GoString() string { return a.String() }

type authDocument struct {
	AuthMode  string          `json:"auth_mode"`
	OpenAIKey json.RawMessage `json:"OPENAI_API_KEY"`
	Tokens    json.RawMessage `json:"tokens"`
}

type tokenDocument struct {
	IDToken     string `json:"id_token"`
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
}

type idClaims struct {
	Email   string `json:"email"`
	Profile struct {
		Email string `json:"email"`
	} `json:"https://api.openai.com/profile"`
	Auth struct {
		ChatGPTUserID    string `json:"chatgpt_user_id"`
		UserID           string `json:"user_id"`
		ChatGPTAccountID string `json:"chatgpt_account_id"`
	} `json:"https://api.openai.com/auth"`
}

// ParseNativeAuth validates the shape of a file-backed native ChatGPT auth
// document and extracts account metadata. JWT claims are decoded, not verified.
func ParseNativeAuth(raw []byte) (NativeAuth, error) {
	if !json.Valid(raw) {
		return NativeAuth{}, errors.New("invalid auth JSON")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return NativeAuth{}, errors.New("auth document must be a JSON object")
	}

	var doc authDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return NativeAuth{}, fmt.Errorf("parse auth document: %w", err)
	}
	mode := strings.TrimSpace(doc.AuthMode)
	if mode == "apikey" || (len(doc.Tokens) == 0 && hasNonEmptyString(doc.OpenAIKey)) {
		return NativeAuth{}, ErrAPIKeyAuth
	}
	if mode != "" && mode != "chatgpt" {
		return NativeAuth{}, fmt.Errorf("%w: %s", ErrUnsupportedAuth, mode)
	}
	if len(doc.Tokens) == 0 || bytes.Equal(bytes.TrimSpace(doc.Tokens), []byte("null")) {
		return NativeAuth{}, errors.New("native ChatGPT token data is missing")
	}

	var tokens tokenDocument
	if err := json.Unmarshal(doc.Tokens, &tokens); err != nil {
		return NativeAuth{}, fmt.Errorf("parse token data: %w", err)
	}
	if strings.TrimSpace(tokens.AccessToken) == "" || strings.TrimSpace(tokens.IDToken) == "" {
		if hasNonEmptyString(doc.OpenAIKey) {
			return NativeAuth{}, ErrAPIKeyAuth
		}
		return NativeAuth{}, errors.New("native ChatGPT token data is incomplete")
	}
	claims, err := decodeClaims(tokens.IDToken)
	if err != nil {
		return NativeAuth{}, fmt.Errorf("decode ID token metadata: %w", err)
	}

	userID := strings.TrimSpace(claims.Auth.ChatGPTUserID)
	if userID == "" {
		userID = strings.TrimSpace(claims.Auth.UserID)
	}
	tokenAccountID := strings.TrimSpace(tokens.AccountID)
	claimAccountID := strings.TrimSpace(claims.Auth.ChatGPTAccountID)
	if tokenAccountID != "" && claimAccountID != "" && tokenAccountID != claimAccountID {
		return NativeAuth{}, ErrConflictingIdentity
	}
	accountID := tokenAccountID
	if accountID == "" {
		accountID = claimAccountID
	}
	if userID == "" || accountID == "" {
		return NativeAuth{}, ErrNativeIdentity
	}
	email := strings.TrimSpace(claims.Email)
	if email == "" {
		email = strings.TrimSpace(claims.Profile.Email)
	}

	credentialCopy := append(json.RawMessage(nil), raw...)
	return NativeAuth{Email: email, UserID: userID, AccountID: accountID, raw: credentialCopy}, nil
}

func decodeClaims(token string) (idClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return idClaims{}, errors.New("invalid JWT format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return idClaims{}, err
	}
	var claims idClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return idClaims{}, err
	}
	return claims, nil
}

func hasNonEmptyString(raw json.RawMessage) bool {
	var value string
	return len(raw) != 0 && json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != ""
}
