package lark

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

type chatTab struct {
	ID      string `json:"tab_id,omitempty"`
	Name    string `json:"tab_name"`
	Type    string `json:"tab_type"`
	Content struct {
		URL string `json:"url"`
	} `json:"tab_content"`
	Config struct {
		IsBuiltIn bool `json:"is_built_in"`
	} `json:"tab_config"`
}

// SyncChatURLTabs preserves unrelated tabs and performs no writes for an
// unchanged URL tab. Non-URL tabs with a matching name are left untouched.
func (c *httpAPIClient) SyncChatURLTabs(ctx context.Context, creds InstallationCredentials, chatID ChatID, tabs []ChatURLTab) error {
	if len(tabs) == 0 {
		return nil
	}
	path := "/open-apis/im/v1/chats/" + url.PathEscape(string(chatID)) + "/chat_tabs"
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Tabs []chatTab `json:"chat_tabs"`
		} `json:"data"`
	}
	if err := c.doAuthedJSON(ctx, creds, http.MethodGet, path+"/list_tabs", nil, &resp); err != nil {
		return fmt.Errorf("lark http client: list chat tabs: %w", err)
	}
	if resp.Code != 0 {
		if isTokenError(resp.Code) {
			c.invalidateToken(creds.AppID)
		}
		return &APIError{Op: "list chat tabs", Code: resp.Code, Msg: resp.Msg}
	}
	byName := make(map[string]chatTab, len(resp.Data.Tabs))
	for _, tab := range resp.Data.Tabs {
		if _, exists := byName[tab.Name]; !exists {
			byName[tab.Name] = tab
		}
	}
	var creates, updates []chatTab
	for _, want := range tabs {
		existing, found := byName[want.Name]
		if found && (existing.Type != "url" || (existing.Content.URL == want.URL && existing.Config.IsBuiltIn)) {
			continue
		}
		tab := chatTab{Name: want.Name, Type: "url"}
		tab.Content.URL = want.URL
		tab.Config.IsBuiltIn = true
		if found {
			tab.ID = existing.ID
			updates = append(updates, tab)
		} else {
			creates = append(creates, tab)
		}
	}
	if len(updates) > 0 {
		if err := c.writeChatTabs(ctx, creds, path+"/update_tabs", updates); err != nil {
			return err
		}
	}
	if len(creates) > 0 {
		return c.writeChatTabs(ctx, creds, path, creates)
	}
	return nil
}

func (c *httpAPIClient) writeChatTabs(ctx context.Context, creds InstallationCredentials, path string, tabs []chatTab) error {
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := c.doAuthedJSON(ctx, creds, http.MethodPost, path, map[string]any{"chat_tabs": tabs}, &resp); err != nil {
		return fmt.Errorf("lark http client: sync chat tabs: %w", err)
	}
	if resp.Code != 0 {
		if isTokenError(resp.Code) {
			c.invalidateToken(creds.AppID)
		}
		return &APIError{Op: "sync chat tabs", Code: resp.Code, Msg: resp.Msg}
	}
	return nil
}
