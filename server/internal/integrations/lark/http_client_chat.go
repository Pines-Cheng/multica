package lark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// ChatAdmin creates group chats and adds members on behalf of an
// installation's bot. It is separate from APIClient, like ReactionLister:
// only the real HTTP client manages chats, so fakes that only send messages
// are not forced to grow chat plumbing.
type ChatAdmin interface {
	CreateChat(ctx context.Context, creds InstallationCredentials, p CreateChatParams) (ChatID, error)
	AddChatMembers(ctx context.Context, creds InstallationCredentials, p AddChatMembersParams) (invalid []string, err error)
	// AddChatURLTab pins a URL as a tab at the top of the chat.
	AddChatURLTab(ctx context.Context, creds InstallationCredentials, chatID ChatID, name, url string) error
	// PinTopNotice pins a message to the top of the chat.
	PinTopNotice(ctx context.Context, creds InstallationCredentials, chatID ChatID, messageID string) error
	// GetChatName and UpdateChatName read and set the chat's name.
	GetChatName(ctx context.Context, creds InstallationCredentials, chatID ChatID) (string, error)
	UpdateChatName(ctx context.Context, creds InstallationCredentials, chatID ChatID, name string) error
	// SetChatTimeSensitive pins the chat to the top of the given users'
	// message lists, or releases it.
	SetChatTimeSensitive(ctx context.Context, creds InstallationCredentials, chatID ChatID, on bool, openIDs []string) error
}

// Lark id types accepted by the chat endpoints.
const (
	MemberIDTypeOpenID  = "open_id"
	MemberIDTypeUnionID = "union_id"
	MemberIDTypeAppID   = "app_id"
)

// maxBotsPerChatRequest is Lark's cap on bot_id_list / one members call for
// bots. Callers batch above it.
const maxBotsPerChatRequest = 5

// CreateChatParams describes a private group chat. UserIDs and OwnerID share
// UserIDType. UUID makes creation idempotent on Lark's side for ten hours, so
// a retried request returns the chat created by the first one.
type CreateChatParams struct {
	Name        string
	Description string
	UserIDType  string
	UserIDs     []string
	OwnerID     string
	BotAppIDs   []string
	UUID        string
}

// AddChatMembersParams adds users (open_id / union_id) or bots (app_id).
type AddChatMembersParams struct {
	ChatID       ChatID
	MemberIDType string
	IDs          []string
}

// CreateChat calls POST /open-apis/im/v1/chats. The calling bot joins the
// chat implicitly; when OwnerID is set it also becomes a chat manager.
func (c *httpAPIClient) CreateChat(ctx context.Context, creds InstallationCredentials, p CreateChatParams) (ChatID, error) {
	if len(p.BotAppIDs) > maxBotsPerChatRequest {
		return "", fmt.Errorf("lark http client: create chat: at most %d bots per request", maxBotsPerChatRequest)
	}
	q := url.Values{}
	idType := p.UserIDType
	if idType == "" {
		idType = MemberIDTypeOpenID
	}
	q.Set("user_id_type", idType)
	if p.OwnerID != "" {
		q.Set("set_bot_manager", "true")
	}
	if p.UUID != "" {
		q.Set("uuid", p.UUID)
	}
	body := map[string]any{
		"name":               p.Name,
		"chat_mode":          "group",
		"chat_type":          "private",
		"group_message_type": "chat",
	}
	if p.Description != "" {
		body["description"] = p.Description
	}
	if p.OwnerID != "" {
		body["owner_id"] = p.OwnerID
	}
	if len(p.UserIDs) > 0 {
		body["user_id_list"] = p.UserIDs
	}
	if len(p.BotAppIDs) > 0 {
		body["bot_id_list"] = p.BotAppIDs
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ChatID string `json:"chat_id"`
		} `json:"data"`
	}
	if err := c.doAuthedJSON(ctx, creds, http.MethodPost, "/open-apis/im/v1/chats?"+q.Encode(), body, &resp); err != nil {
		return "", fmt.Errorf("lark http client: create chat: %w", err)
	}
	if resp.Code != 0 || resp.Data.ChatID == "" {
		if isTokenError(resp.Code) {
			c.invalidateToken(creds.AppID)
		}
		return "", &APIError{Op: "create chat", Code: resp.Code, Msg: resp.Msg}
	}
	return ChatID(resp.Data.ChatID), nil
}

// AddChatMembers calls POST /open-apis/im/v1/chats/{chat_id}/members. Valid
// ids are added even when others are not; the unusable ones are returned.
func (c *httpAPIClient) AddChatMembers(ctx context.Context, creds InstallationCredentials, p AddChatMembersParams) ([]string, error) {
	if p.ChatID == "" {
		return nil, errors.New("lark http client: add chat members: missing chat_id")
	}
	if len(p.IDs) == 0 {
		return nil, nil
	}
	q := url.Values{}
	q.Set("member_id_type", p.MemberIDType)
	q.Set("succeed_type", "1")
	path := "/open-apis/im/v1/chats/" + url.PathEscape(string(p.ChatID)) + "/members?" + q.Encode()
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			InvalidIDList  []string `json:"invalid_id_list"`
			NotExistedList []string `json:"not_existed_id_list"`
		} `json:"data"`
	}
	if err := c.doAuthedJSON(ctx, creds, http.MethodPost, path, map[string]any{"id_list": p.IDs}, &resp); err != nil {
		return nil, fmt.Errorf("lark http client: add chat members: %w", err)
	}
	if resp.Code != 0 {
		if isTokenError(resp.Code) {
			c.invalidateToken(creds.AppID)
		}
		return nil, &APIError{Op: "add chat members", Code: resp.Code, Msg: resp.Msg}
	}
	return append(resp.Data.InvalidIDList, resp.Data.NotExistedList...), nil
}

// AddChatURLTab calls POST /open-apis/im/v1/chats/{chat_id}/chat_tabs.
func (c *httpAPIClient) AddChatURLTab(ctx context.Context, creds InstallationCredentials, chatID ChatID, name, tabURL string) error {
	body := map[string]any{"chat_tabs": []map[string]any{{
		"tab_name":    name,
		"tab_type":    "url",
		"tab_content": map[string]string{"url": tabURL},
	}}}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	path := "/open-apis/im/v1/chats/" + url.PathEscape(string(chatID)) + "/chat_tabs"
	if err := c.doAuthedJSON(ctx, creds, http.MethodPost, path, body, &resp); err != nil {
		return fmt.Errorf("lark http client: add chat tab: %w", err)
	}
	if resp.Code != 0 {
		if isTokenError(resp.Code) {
			c.invalidateToken(creds.AppID)
		}
		return &APIError{Op: "add chat tab", Code: resp.Code, Msg: resp.Msg}
	}
	return nil
}

// PinTopNotice calls POST /open-apis/im/v1/chats/{chat_id}/top_notice/put_top_notice
// with action_type 1 (a message).
func (c *httpAPIClient) PinTopNotice(ctx context.Context, creds InstallationCredentials, chatID ChatID, messageID string) error {
	body := map[string]any{"chat_top_notice": []map[string]string{{"action_type": "1", "message_id": messageID}}}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	path := "/open-apis/im/v1/chats/" + url.PathEscape(string(chatID)) + "/top_notice/put_top_notice"
	if err := c.doAuthedJSON(ctx, creds, http.MethodPost, path, body, &resp); err != nil {
		return fmt.Errorf("lark http client: pin top notice: %w", err)
	}
	if resp.Code != 0 {
		if isTokenError(resp.Code) {
			c.invalidateToken(creds.AppID)
		}
		return &APIError{Op: "pin top notice", Code: resp.Code, Msg: resp.Msg}
	}
	return nil
}

// chatCall sends one chat-management request and maps a Lark business error.
func (c *httpAPIClient) chatCall(ctx context.Context, creds InstallationCredentials, method, path, op string, body any, data any) error {
	var resp struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := c.doAuthedJSON(ctx, creds, method, path, body, &resp); err != nil {
		return fmt.Errorf("lark http client: %s: %w", op, err)
	}
	if resp.Code != 0 {
		if isTokenError(resp.Code) {
			c.invalidateToken(creds.AppID)
		}
		return &APIError{Op: op, Code: resp.Code, Msg: resp.Msg}
	}
	if data != nil && len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, data); err != nil {
			return fmt.Errorf("lark http client: %s: decode: %w", op, err)
		}
	}
	return nil
}

// GetChatName calls GET /open-apis/im/v1/chats/{chat_id}.
func (c *httpAPIClient) GetChatName(ctx context.Context, creds InstallationCredentials, chatID ChatID) (string, error) {
	var data struct {
		Name string `json:"name"`
	}
	err := c.chatCall(ctx, creds, http.MethodGet, "/open-apis/im/v1/chats/"+url.PathEscape(string(chatID)), "get chat", nil, &data)
	return data.Name, err
}

// UpdateChatName calls PUT /open-apis/im/v1/chats/{chat_id}.
func (c *httpAPIClient) UpdateChatName(ctx context.Context, creds InstallationCredentials, chatID ChatID, name string) error {
	return c.chatCall(ctx, creds, http.MethodPut, "/open-apis/im/v1/chats/"+url.PathEscape(string(chatID)), "update chat name", map[string]string{"name": name}, nil)
}

// SetChatTimeSensitive calls PATCH /open-apis/im/v2/feed_cards/{chat_id}.
func (c *httpAPIClient) SetChatTimeSensitive(ctx context.Context, creds InstallationCredentials, chatID ChatID, on bool, openIDs []string) error {
	path := "/open-apis/im/v2/feed_cards/" + url.PathEscape(string(chatID)) + "?user_id_type=open_id"
	return c.chatCall(ctx, creds, http.MethodPatch, path, "set chat time sensitive", map[string]any{"time_sensitive": on, "user_ids": openIDs}, nil)
}

var _ ChatAdmin = (*httpAPIClient)(nil)
