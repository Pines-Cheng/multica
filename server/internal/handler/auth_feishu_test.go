package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func feishuLoginFixture(t *testing.T) (*Handler, string) {
	t.Helper()
	fixtureID := dbfx.User(t, "Feishu login fixture", "feishu-login-fixture@example.test")
	appID := "cli_login_" + strings.ReplaceAll(fixtureID, "-", "")
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE email = '' AND id IN (SELECT user_id FROM feishu_login_identity WHERE app_id = $1)`, appID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM feishu_login_identity WHERE app_id = $1`, appID)
	})
	t.Setenv("MULTICA_FEISHU_LOGIN_APP_ID", appID)
	t.Setenv("MULTICA_FEISHU_LOGIN_APP_SECRET", "fixture-secret")
	t.Setenv("MULTICA_FEISHU_LOGIN_TENANT_KEY", "fixture-tenant")
	h := &Handler{
		Queries: testHandler.Queries, TxStarter: testPool,
		cfg: Config{AllowSignup: true, AppURL: "https://demo.example"},
	}
	return h, appID
}

func TestFeishuLoginCreatesAccountsWithoutEmail(t *testing.T) {
	h, appID := feishuLoginFixture(t)
	ctx := context.Background()
	first := auth.FeishuIdentity{OpenID: "ou_first", TenantKey: "fixture-tenant", Name: "First member"}
	user, isNew, err := h.feishuLoginUser(ctx, appID, first, "")
	if err != nil || !isNew || user.Email != "" || user.Name != first.Name {
		t.Fatalf("first sign-in: user=%+v new=%v err=%v", user, isNew, err)
	}
	again, isNew, err := h.feishuLoginUser(ctx, appID, first, "")
	if err != nil || isNew || again.ID != user.ID {
		t.Fatalf("repeat sign-in changed account: user=%+v new=%v err=%v", again, isNew, err)
	}
	second, isNew, err := h.feishuLoginUser(ctx, appID, auth.FeishuIdentity{OpenID: "ou_second", TenantKey: "fixture-tenant", Name: "Second member"}, "")
	if err != nil || !isNew || second.Email != "" || second.ID == user.ID {
		t.Fatalf("second member must have an independent account without email: user=%+v new=%v err=%v", second, isNew, err)
	}
	var memberships int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM member WHERE user_id = $1`, second.ID).Scan(&memberships); err != nil || memberships != 0 {
		t.Fatalf("sign-in must not grant workspace membership: memberships=%d err=%v", memberships, err)
	}
}

func TestFeishuLoginPreservesAccountAcrossApplications(t *testing.T) {
	h, appID := feishuLoginFixture(t)
	oldAppID := "cli_old_" + appID
	dbfx.InsertNoID(t, "feishu_login_identity", testutil.Cols{
		"app_id": oldAppID, "open_id": "ou_old", "tenant_key": "fixture-tenant",
		"union_id": "on_member_" + appID, "user_id": testUserID,
	}, "app_id = $1", oldAppID)
	identity := auth.FeishuIdentity{OpenID: "ou_new", TenantKey: "fixture-tenant", UnionID: "on_member_" + appID}
	h.cfg.AllowSignup = false
	user, isNew, err := h.feishuLoginUser(context.Background(), appID, identity, "")
	if err != nil || isNew || uuidToString(user.ID) != testUserID {
		t.Fatalf("new application must retain the original account even with signup closed: user=%+v new=%v err=%v", user, isNew, err)
	}
	other := dbfx.User(t, "Other member", "other-feishu-app@example.test")
	identity.OpenID = "ou_conflicting"
	if _, _, err := h.feishuLoginUser(context.Background(), appID, identity, other); err == nil {
		t.Fatal("cross-app identity must not move to another account through explicit linking")
	}
	identity.TenantKey = "another-tenant"
	if _, _, err := h.feishuLoginUser(context.Background(), appID, identity, ""); err != ErrSignupProhibited {
		t.Fatalf("union identity from another tenant must not merge: %v", err)
	}
	identity.TenantKey = "fixture-tenant"
	identity.UnionID = ""
	if _, _, err := h.feishuLoginUser(context.Background(), appID, identity, ""); err != ErrSignupProhibited {
		t.Fatalf("empty union identity must not merge: %v", err)
	}
}

func TestFeishuLoginRespectsSignupPolicy(t *testing.T) {
	h, appID := feishuLoginFixture(t)
	h.cfg.AllowSignup = false
	_, _, err := h.feishuLoginUser(context.Background(), appID, auth.FeishuIdentity{OpenID: "ou_new", TenantKey: "fixture-tenant"}, "")
	if err != ErrSignupProhibited {
		t.Fatalf("expected signup policy rejection, got %v", err)
	}
	// Existing accounts can explicitly link their identity even when signup is closed.
	identity := auth.FeishuIdentity{OpenID: "ou_linked", TenantKey: "fixture-tenant"}
	user, isNew, err := h.feishuLoginUser(context.Background(), appID, identity, testUserID)
	if err != nil || isNew || uuidToString(user.ID) != testUserID {
		t.Fatalf("explicit account link failed: user=%+v new=%v err=%v", user, isNew, err)
	}
	other := dbfx.User(t, "Other member", "other-feishu-link@example.test")
	if _, _, err := h.feishuLoginUser(context.Background(), appID, identity, other); err == nil {
		t.Fatal("an already linked identity must not move to another account")
	}
}

func TestFeishuLoginCallback(t *testing.T) {
	h, appID := feishuLoginFixture(t)
	dbfx.InsertNoID(t, "feishu_login_identity", testutil.Cols{
		"app_id": appID, "open_id": "ou_existing", "tenant_key": "fixture-tenant", "user_id": testUserID,
	}, "app_id = $1", appID)
	begin := httptest.NewRecorder()
	h.BeginFeishuLogin(begin, httptest.NewRequest(http.MethodGet, "/auth/feishu/start?return_to=%2Flogin%3Fnext%3D%252Fjoin%253Fcode%253Dinvite", nil))
	location, err := url.Parse(begin.Header().Get("Location"))
	if err != nil || location.Host != "accounts.feishu.cn" || len(begin.Result().Cookies()) != 1 {
		t.Fatalf("invalid authorization redirect: %s, err=%v", begin.Header().Get("Location"), err)
	}
	cookie := begin.Result().Cookies()[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("OAuth cookie must survive the cross-site callback without JS access: %+v", cookie)
	}
	profileTenant := "fixture-tenant"
	h.feishuOAuthHTTPClient = &http.Client{Transport: googleRoundTripper(func(req *http.Request) (*http.Response, error) {
		body := `{"code":0,"access_token":"fixture-token"}`
		if req.Method == http.MethodGet {
			body = `{"code":0,"data":{"open_id":"ou_existing","tenant_key":"` + profileTenant + `","email":"not-the-account@example.test"}}`
		}
		return googleResponse(req, http.StatusOK, body), nil
	})}
	callback := httptest.NewRequest(http.MethodGet, "/auth/feishu/callback?code=provider-code&state="+url.QueryEscape(location.Query().Get("state")), nil)
	callback.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.CompleteFeishuLogin(w, callback)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login?next=%2Fjoin%3Fcode%3Dinvite" {
		t.Fatalf("lost invitation destination: status=%d location=%s", w.Code, w.Header().Get("Location"))
	}
	var token string
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == auth.AuthCookieName {
			token = cookie.Value
		}
	}
	claims, err := auth.ParseSessionToken(token)
	if err != nil || claims["sub"] != testUserID {
		t.Fatalf("callback did not authenticate the original account: %v", err)
	}
	callback.URL.RawQuery = "code=provider-code&state=wrong-state"
	w = httptest.NewRecorder()
	h.CompleteFeishuLogin(w, callback)
	if !strings.Contains(w.Header().Get("Location"), "feishu_error=expired") || len(w.Result().Cookies()) != 0 {
		t.Fatal("invalid browser state must not issue a session")
	}
	profileTenant = "another-tenant"
	callback.URL.RawQuery = "code=another-code&state=" + url.QueryEscape(location.Query().Get("state"))
	w = httptest.NewRecorder()
	h.CompleteFeishuLogin(w, callback)
	if !strings.Contains(w.Header().Get("Location"), "feishu_error=failed") {
		t.Fatal("sign-in must reject another enterprise")
	}
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == auth.AuthCookieName {
			t.Fatal("another enterprise must not receive an authenticated session")
		}
	}
}

func TestFeishuLoginConfigIndependentOfBots(t *testing.T) {
	h, appID := feishuLoginFixture(t)
	cfg, tenant, err := h.feishuLoginConfig()
	if err != nil || cfg.AppID != appID || cfg.AppSecret != "fixture-secret" || tenant != "fixture-tenant" || h.LarkInstallations != nil {
		t.Fatalf("dedicated login must work without bot services: %v", err)
	}
	fetch := func() AppConfig {
		t.Helper()
		var cfg AppConfig
		testutil.Call(t, h.GetConfig, httptest.NewRequest(http.MethodGet, "/api/config", nil)).Want(http.StatusOK).JSON(&cfg)
		return cfg
	}
	if !fetch().FeishuLoginEnabled {
		t.Fatal("complete standalone login configuration must enable the login button")
	}
	for _, key := range []string{"MULTICA_FEISHU_LOGIN_APP_ID", "MULTICA_FEISHU_LOGIN_APP_SECRET", "MULTICA_FEISHU_LOGIN_TENANT_KEY"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "")
			if fetch().FeishuLoginEnabled {
				t.Fatal("incomplete credentials must hide the login button")
			}
		})
	}
}

func TestFeishuLoginReturnPath(t *testing.T) {
	for _, path := range []string{"https://evil.test/login", "//evil.test/login", "/some-workspace/issues"} {
		if got := feishuLoginReturnPath(path); got != "/login" {
			t.Fatalf("unsafe return path accepted: %q", got)
		}
	}
}
