package handler

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/auth"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const feishuLoginCookie = "multica_feishu_login"

func (h *Handler) feishuLoginConfig() (auth.FeishuOAuth, string, error) {
	appID := strings.TrimSpace(os.Getenv("MULTICA_FEISHU_LOGIN_APP_ID"))
	secret := strings.TrimSpace(os.Getenv("MULTICA_FEISHU_LOGIN_APP_SECRET"))
	tenantKey := strings.TrimSpace(os.Getenv("MULTICA_FEISHU_LOGIN_TENANT_KEY"))
	if appID == "" || secret == "" || tenantKey == "" {
		return auth.FeishuOAuth{}, "", errors.New("Feishu login is not configured")
	}
	appURL := strings.TrimRight(strings.TrimSpace(h.cfg.AppURL), "/")
	base, err := url.Parse(appURL)
	if err != nil || base.Host == "" || base.User != nil || (base.Scheme != "http" && base.Scheme != "https") {
		return auth.FeishuOAuth{}, "", errors.New("application URL is not configured")
	}
	return auth.FeishuOAuth{
		AppID: appID, AppSecret: secret, RedirectURI: appURL + "/auth/feishu/callback",
		HTTPClient: h.feishuOAuthHTTPClient,
	}, tenantKey, nil
}

func feishuLoginReturnPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || u.Path != "/login" {
		return "/login"
	}
	u.Fragment = ""
	q := u.Query()
	q.Del("feishu_error")
	u.RawQuery = q.Encode()
	return u.String()
}

func feishuLoginRedirect(w http.ResponseWriter, r *http.Request, returnPath, code string) {
	u, _ := url.Parse(feishuLoginReturnPath(returnPath))
	if code != "" {
		q := u.Query()
		q.Set("feishu_error", code)
		u.RawQuery = q.Encode()
	}
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

func (h *Handler) BeginFeishuLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	returnPath := feishuLoginReturnPath(r.URL.Query().Get("return_to"))
	cfg, _, err := h.feishuLoginConfig()
	if err != nil {
		feishuLoginRedirect(w, r, returnPath, "not_configured")
		return
	}
	linkUserID := ""
	if r.URL.Query().Get("link") == "1" {
		cookie, err := r.Cookie(auth.AuthCookieName)
		if err != nil {
			feishuLoginRedirect(w, r, returnPath, "link_signin")
			return
		}
		claims, err := auth.ParseSessionToken(cookie.Value)
		if err != nil {
			feishuLoginRedirect(w, r, returnPath, "link_signin")
			return
		}
		linkUserID, _ = claims.GetSubject()
		if linkUserID == "" {
			feishuLoginRedirect(w, r, returnPath, "link_signin")
			return
		}
	}
	state, cookie, err := auth.NewFeishuLoginState(cfg.AppID, returnPath, linkUserID)
	if err != nil {
		feishuLoginRedirect(w, r, returnPath, "failed")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: feishuLoginCookie, Value: cookie, Path: "/auth/feishu",
		MaxAge: 600, HttpOnly: true, Secure: strings.HasPrefix(cfg.RedirectURI, "https://"),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, cfg.AuthorizeURL(state), http.StatusSeeOther)
}

func (h *Handler) CompleteFeishuLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	cookie, err := r.Cookie(feishuLoginCookie)
	if err != nil {
		feishuLoginRedirect(w, r, "/login", "expired")
		return
	}
	state, err := auth.ParseFeishuLoginState(cookie.Value)
	if err != nil || subtle.ConstantTimeCompare([]byte(state.Nonce), []byte(r.URL.Query().Get("state"))) != 1 {
		feishuLoginRedirect(w, r, "/login", "expired")
		return
	}
	cfg, tenantKey, err := h.feishuLoginConfig()
	if err != nil || cfg.AppID != state.AppID {
		feishuLoginRedirect(w, r, state.ReturnPath, "not_configured")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: feishuLoginCookie, Value: "", Path: "/auth/feishu", MaxAge: -1,
		HttpOnly: true, Secure: strings.HasPrefix(cfg.RedirectURI, "https://"), SameSite: http.SameSiteLaxMode,
	})
	code := r.URL.Query().Get("code")
	if r.URL.Query().Get("error") != "" || code == "" {
		feishuLoginRedirect(w, r, state.ReturnPath, "denied")
		return
	}
	identity, err := cfg.Identity(r.Context(), code, state.Verifier)
	if err != nil {
		slog.Warn("Feishu sign-in identity exchange failed", "error", err)
		feishuLoginRedirect(w, r, state.ReturnPath, "failed")
		return
	}
	if tenantKey != identity.TenantKey {
		slog.Warn("Feishu sign-in tenant does not match application")
		feishuLoginRedirect(w, r, state.ReturnPath, "failed")
		return
	}
	user, isNew, err := h.feishuLoginUser(r.Context(), cfg.AppID, identity, state.LinkUserID)
	if err != nil {
		slog.Warn("Feishu sign-in account resolution failed", "error", err)
		errorCode := "failed"
		if errors.Is(err, ErrSignupProhibited) || errors.Is(err, ErrEmailNotAllowed) {
			errorCode = "signup_disabled"
		}
		feishuLoginRedirect(w, r, state.ReturnPath, errorCode)
		return
	}
	if isNew {
		obsmetrics.RecordEvent(h.Analytics, h.Metrics, analytics.Signup(uuidToString(user.ID), user.Email, signupSourceFromRequest(r)))
	}
	token, err := h.issueJWT(user)
	if err != nil {
		feishuLoginRedirect(w, r, state.ReturnPath, "failed")
		return
	}
	if err := auth.SetAuthCookies(w, token); err != nil {
		feishuLoginRedirect(w, r, state.ReturnPath, "failed")
		return
	}
	if h.CFSigner != nil {
		for _, cookie := range h.CFSigner.SignedCookies(time.Now().Add(auth.AuthTokenTTL())) {
			http.SetCookie(w, cookie)
		}
	}
	slog.Info("user logged in with Feishu", "user_id", uuidToString(user.ID))
	feishuLoginRedirect(w, r, state.ReturnPath, "")
}

func (h *Handler) feishuLoginUser(ctx context.Context, appID string, identity auth.FeishuIdentity, linkUserID string) (db.User, bool, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return db.User{}, false, err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	linked, err := q.GetFeishuLoginIdentity(ctx, db.GetFeishuLoginIdentityParams{AppID: appID, OpenID: identity.OpenID})
	if err == nil {
		if linked.TenantKey != identity.TenantKey || (linkUserID != "" && uuidToString(linked.UserID) != linkUserID) {
			return db.User{}, false, errors.New("Feishu identity belongs to a different account")
		}
		user, err := q.GetUser(ctx, linked.UserID)
		return user, false, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.User{}, false, err
	}
	// open_id changes between apps. A matching tenant and union_id preserve
	// the account when switching the login application of the same developer.
	users, err := q.ListFeishuLoginUsersByUnionID(ctx, db.ListFeishuLoginUsersByUnionIDParams{
		TenantKey: identity.TenantKey, UnionID: identity.UnionID,
	})
	if err != nil {
		return db.User{}, false, err
	}
	if len(users) > 1 || (len(users) == 1 && linkUserID != "" && uuidToString(users[0]) != linkUserID) {
		return db.User{}, false, errors.New("Feishu identity belongs to a different account")
	}
	var user db.User
	isNew := false
	if linkUserID != "" {
		user, err = q.GetUser(ctx, parseUUID(linkUserID))
	} else if len(users) == 1 {
		user, err = q.GetUser(ctx, users[0])
	} else {
		if err := h.checkSignupAllowed(ctx, "", true); err != nil {
			return db.User{}, false, err
		}
		name := strings.TrimSpace(identity.Name)
		if name == "" {
			name = "Feishu"
		}
		user, err = q.CreateUser(ctx, db.CreateUserParams{Name: name, Email: ""})
		isNew = true
	}
	if err != nil {
		return db.User{}, false, err
	}
	if auth.IsTemporarilyDisabledUser(uuidToString(user.ID), user.Email) {
		return db.User{}, false, auth.ErrTemporarilyDisabledUser
	}
	if _, err := q.CreateFeishuLoginIdentity(ctx, db.CreateFeishuLoginIdentityParams{
		AppID: appID, OpenID: identity.OpenID, TenantKey: identity.TenantKey,
		UnionID: identity.UnionID, UserID: user.ID,
	}); err != nil {
		return db.User{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.User{}, false, err
	}
	return user, isNew, nil
}
