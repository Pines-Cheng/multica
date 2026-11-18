package lark

import (
	"context"
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
	// AddChatURLTab pins a URL as an in-app tab at the top of the chat.
	AddChatURLTab(ctx context.Context, creds InstallationCredentials, chatID ChatID, name, url string) error
	// SyncChatURLTabs updates only owned tabs and suppresses identical existing
	// links. It fills created IDs and clears deleted duplicate IDs in tabs.
	SyncChatURLTabs(ctx context.Context, creds InstallationCredentials, chatID ChatID, tabs []ChatURLTab) error
	// PinTopNotice pins a message to the top of the chat.
	PinTopNotice(ctx context.Context, creds InstallationCredentials, chatID ChatID, messageID string) error
	// GetChatInfo reads what binding an existing chat needs to know about it,
	// as seen by the calling bot.
	GetChatInfo(ctx context.Context, creds InstallationCredentials, chatID ChatID) (ChatInfo, error)
}

// ChatInfo is an existing chat as seen by one bot.
type ChatInfo struct {
	BotInChat bool
	External  bool
}

// ErrChatNotFound: Lark does not know the chat id.
var ErrChatNotFound = errors.New("lark: chat not found")

const (
	codeChatInvalidID     = 232006
	codeChatOperatorNotIn = 232011
)

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

type ChatURLTab struct {
	Key  string
	ID   string
	Name string
	URL  string
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
		"tab_config":  map[string]bool{"is_built_in": true},
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

// GetChatInfo calls GET /open-apis/im/v1/chats/{chat_id}. A bot outside the
// chat is either refused with 232011 or given only the public fields (name,
// avatar, member count, status), so a reply without chat_mode also means the
// bot is not in the chat.
func (c *httpAPIClient) GetChatInfo(ctx context.Context, creds InstallationCredentials, chatID ChatID) (ChatInfo, error) {
	if chatID == "" {
		return ChatInfo{}, errors.New("lark http client: get chat: missing chat_id")
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ChatMode string `json:"chat_mode"`
			External bool   `json:"external"`
		} `json:"data"`
	}
	path := "/open-apis/im/v1/chats/" + url.PathEscape(string(chatID))
	err := c.doAuthedJSON(ctx, creds, http.MethodGet, path, nil, &resp)
	// Lark answers these refusals with HTTP 400, so the code may come from
	// either the error or the envelope.
	code, msg, hasCode := larkErrorCodeMsg(err)
	if err == nil && resp.Code != 0 {
		code, msg, hasCode = resp.Code, resp.Msg, true
		if isTokenError(code) {
			c.invalidateToken(creds.AppID)
		}
	}
	switch {
	case code == codeChatOperatorNotIn:
		return ChatInfo{}, nil
	case code == codeChatInvalidID:
		return ChatInfo{}, ErrChatNotFound
	case hasCode:
		return ChatInfo{}, &APIError{Op: "get chat", Code: code, Msg: msg}
	case err != nil:
		return ChatInfo{}, fmt.Errorf("lark http client: get chat: %w", err)
	}
	return ChatInfo{BotInChat: resp.Data.ChatMode != "", External: resp.Data.External}, nil
}

var _ ChatAdmin = (*httpAPIClient)(nil)
