package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const feishuLoginAudience = "feishu_login"

var ErrFeishuOAuth = errors.New("Feishu authorization failed")

type FeishuLoginState struct {
	jwt.RegisteredClaims
	Nonce      string `json:"nonce"`
	Verifier   string `json:"verifier"`
	AppID      string `json:"app_id"`
	ReturnPath string `json:"return_path"`
	LinkUserID string `json:"link_user_id,omitempty"`
}

func NewFeishuLoginState(appID, returnPath, linkUserID string) (FeishuLoginState, string, error) {
	randomValue := func() (string, error) {
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(raw[:]), nil
	}
	nonce, err := randomValue()
	if err != nil {
		return FeishuLoginState{}, "", err
	}
	verifier, err := randomValue()
	if err != nil {
		return FeishuLoginState{}, "", err
	}
	state := FeishuLoginState{
		RegisteredClaims: jwt.RegisteredClaims{
			Audience:  jwt.ClaimStrings{feishuLoginAudience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(10 * time.Minute)),
		},
		Nonce: nonce, Verifier: verifier, AppID: appID, ReturnPath: returnPath, LinkUserID: linkUserID,
	}
	cookie, err := jwt.NewWithClaims(jwt.SigningMethodHS256, state).SignedString(JWTSecret())
	return state, cookie, err
}

func ParseFeishuLoginState(cookie string) (FeishuLoginState, error) {
	var state FeishuLoginState
	_, err := jwt.ParseWithClaims(cookie, &state, func(*jwt.Token) (any, error) {
		return JWTSecret(), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience(feishuLoginAudience), jwt.WithExpirationRequired())
	if err != nil || state.Nonce == "" || state.Verifier == "" || state.AppID == "" {
		return FeishuLoginState{}, ErrFeishuOAuth
	}
	return state, nil
}

type FeishuOAuth struct {
	AppID       string
	AppSecret   string
	RedirectURI string
	HTTPClient  *http.Client
}

type FeishuIdentity struct {
	OpenID    string `json:"open_id"`
	TenantKey string `json:"tenant_key"`
	UnionID   string `json:"union_id"`
	Name      string `json:"name"`
}

func (f FeishuOAuth) AuthorizeURL(state FeishuLoginState) string {
	challenge := sha256.Sum256([]byte(state.Verifier))
	params := url.Values{
		"client_id": {f.AppID}, "redirect_uri": {f.RedirectURI},
		"response_type": {"code"}, "state": {state.Nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	return "https://accounts.feishu.cn/open-apis/authen/v1/authorize?" + params.Encode()
}

func (f FeishuOAuth) Identity(ctx context.Context, code, verifier string) (FeishuIdentity, error) {
	client := f.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	body, err := json.Marshal(map[string]string{
		"grant_type": "authorization_code", "client_id": f.AppID,
		"client_secret": f.AppSecret, "redirect_uri": f.RedirectURI,
		"code": code, "code_verifier": verifier,
	})
	if err != nil {
		return FeishuIdentity{}, ErrFeishuOAuth
	}
	// Feishu's v1 authorization endpoint currently requires v2 token exchange
	// for PKCE; its authorize API documentation explicitly calls out this limit.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://open.feishu.cn/open-apis/authen/v2/oauth/token", bytes.NewReader(body))
	if err != nil {
		return FeishuIdentity{}, ErrFeishuOAuth
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	var token struct {
		Code        *int   `json:"code"`
		AccessToken string `json:"access_token"`
	}
	if err := feishuOAuthRequest(client, req, &token); err != nil {
		return FeishuIdentity{}, fmt.Errorf("token exchange: %w", err)
	}
	if token.Code == nil || *token.Code != 0 || token.AccessToken == "" {
		return FeishuIdentity{}, feishuOAuthResultError("token exchange", token.Code)
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, "https://open.feishu.cn/open-apis/authen/v1/user_info", nil)
	if err != nil {
		return FeishuIdentity{}, ErrFeishuOAuth
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	var profile struct {
		Code *int           `json:"code"`
		Data FeishuIdentity `json:"data"`
	}
	if err := feishuOAuthRequest(client, req, &profile); err != nil {
		return FeishuIdentity{}, fmt.Errorf("user info: %w", err)
	}
	if profile.Code == nil || *profile.Code != 0 || profile.Data.OpenID == "" || profile.Data.TenantKey == "" {
		return FeishuIdentity{}, feishuOAuthResultError("user info", profile.Code)
	}
	return profile.Data, nil
}

func feishuOAuthResultError(stage string, code *int) error {
	if code == nil {
		return fmt.Errorf("%s: missing result code: %w", stage, ErrFeishuOAuth)
	}
	return fmt.Errorf("%s: provider code %d: %w", stage, *code, ErrFeishuOAuth)
}

func feishuOAuthRequest(client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return ErrFeishuOAuth
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var result struct {
			Code *int `json:"code"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result)
		return fmt.Errorf("HTTP %d: %w", resp.StatusCode, feishuOAuthResultError("response", result.Code))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(out); err != nil {
		return ErrFeishuOAuth
	}
	return nil
}
