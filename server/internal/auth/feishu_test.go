package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type feishuRoundTripper func(*http.Request) (*http.Response, error)

func (f feishuRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFeishuLoginState(t *testing.T) {
	state, cookie, err := NewFeishuLoginState("cli_test", "/login?next=%2Fjoin", "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseFeishuLoginState(cookie)
	if err != nil || parsed.Nonce != state.Nonce || parsed.Verifier != state.Verifier {
		t.Fatalf("state did not round-trip: %v", err)
	}
	authorize, _ := url.Parse((FeishuOAuth{AppID: "cli_test", RedirectURI: "http://demo/auth/feishu/callback"}).AuthorizeURL(state))
	q := authorize.Query()
	challenge := sha256.Sum256([]byte(state.Verifier))
	if q.Get("state") != state.Nonce || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(challenge[:]) {
		t.Fatal("authorization request is not bound to the state and PKCE verifier")
	}
	if q.Has("client_secret") || q.Has("code_verifier") || q.Has("scope") {
		t.Fatal("sign-in must not expose credentials or request business API scopes")
	}
	state.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
	expired, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, state).SignedString(JWTSecret())
	state.ExpiresAt = jwt.NewNumericDate(time.Now().Add(time.Minute))
	state.Audience = jwt.ClaimStrings{"some_other_flow"}
	wrongAudience, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, state).SignedString(JWTSecret())
	for _, invalid := range []string{cookie + "x", expired, wrongAudience, ""} {
		if _, err := ParseFeishuLoginState(invalid); err == nil {
			t.Fatal("accepted invalid OAuth state")
		}
	}
}

func TestFeishuIdentityExchange(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		profile string
		status  int
		wantOK  bool
	}{
		{"success", `{"code":0,"access_token":"user-token"}`, `{"code":0,"data":{"open_id":"ou_member","tenant_key":"tenant","name":"Member","email":"unverified@example.test"}}`, 200, true},
		{"missing token", `{"code":0}`, "", 200, false},
		{"missing result code", `{"access_token":"user-token"}`, "", 200, false},
		{"provider rejection", `{"code":20004}`, "", 200, false},
		{"malformed token", `null`, "", 200, false},
		{"wrong token type", `{"code":0,"access_token":42}`, "", 200, false},
		{"unavailable", `{}`, "", 503, false},
		{"missing identity", `{"code":0,"access_token":"user-token"}`, `{"code":0,"data":{"tenant_key":"tenant"}}`, 200, false},
		{"missing tenant", `{"code":0,"access_token":"user-token"}`, `{"code":0,"data":{"open_id":"ou_member"}}`, 200, false},
		{"malformed profile", `{"code":0,"access_token":"user-token"}`, `{"code":0,"data":42}`, 200, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := FeishuOAuth{AppID: "cli_test", AppSecret: "private-secret", RedirectURI: "http://demo/auth/feishu/callback"}
			cfg.HTTPClient = &http.Client{Transport: feishuRoundTripper(func(req *http.Request) (*http.Response, error) {
				body := tt.profile
				if req.URL.String() == "https://open.feishu.cn/open-apis/authen/v2/oauth/token" {
					var payload map[string]string
					if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					if req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/json; charset=utf-8" || payload["code_verifier"] != "verifier" || payload["client_secret"] != cfg.AppSecret || payload["redirect_uri"] != cfg.RedirectURI {
						t.Fatal("incorrect token exchange")
					}
					body = tt.token
				} else if req.URL.String() != "https://open.feishu.cn/open-apis/authen/v1/user_info" || req.Header.Get("Authorization") != "Bearer user-token" {
					t.Fatalf("unexpected identity request: %s", req.URL)
				}
				return &http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			identity, err := cfg.Identity(context.Background(), "code", "verifier")
			if (err == nil) != tt.wantOK {
				t.Fatalf("identity=%+v error=%v, want success=%v", identity, err, tt.wantOK)
			}
			if tt.wantOK && (identity.OpenID != "ou_member" || identity.Name != "Member") {
				t.Fatalf("wrong identity: %+v", identity)
			}
		})
	}
}
