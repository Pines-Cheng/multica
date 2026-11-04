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

// SyncChatURLTabs preserves manual tabs, updating only persisted tab IDs.
// Created IDs are returned in the caller's slice, including on partial failure.
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
	byID := make(map[string]chatTab, len(resp.Data.Tabs))
	names := make(map[string]int, len(resp.Data.Tabs))
	// System tabs do not consume the 20 custom-tab slots. Updates still run
	// when the chat is full; excess links remain available on the card.
	remaining := 20
	for _, tab := range resp.Data.Tabs {
		byID[tab.ID] = tab
		names[tab.Name]++
		if tab.Type == "url" || tab.Type == "doc" {
			remaining--
		}
	}
	var creates, updates []chatTab
	var createdIndexes []int
	for i, want := range tabs {
		existing, found := byID[want.ID]
		owned := want.ID != "" && found && existing.Type == "url"
		if !owned && remaining <= 0 {
			continue
		}
		// Titles are data, not ownership markers. Resolve collisions without
		// changing a manual tab, including one with the same URL and title.
		base := want.Name
		for n := 2; names[want.Name] > 0 && !(owned && existing.Name == want.Name && names[want.Name] == 1); n++ {
			suffix := fmt.Sprintf(" (%d)", n)
			want.Name = chatTabName(base, 30-len(suffix)) + suffix
		}
		if !owned || existing.Name != want.Name {
			names[want.Name]++
		}
		if owned && existing.Name == want.Name && existing.Content.URL == want.URL && existing.Config.IsBuiltIn {
			continue
		}
		tab := chatTab{Name: want.Name, Type: "url"}
		tab.Content.URL = want.URL
		tab.Config.IsBuiltIn = true
		if owned {
			tab.ID = existing.ID
			updates = append(updates, tab)
		} else if remaining > 0 {
			creates = append(creates, tab)
			createdIndexes = append(createdIndexes, i)
			remaining--
		}
	}
	if len(updates) > 0 {
		if _, err := c.writeChatTabs(ctx, creds, path+"/update_tabs", updates); err != nil {
			return err
		}
	}
	if len(creates) == 0 {
		return nil
	}
	created, err := c.writeChatTabs(ctx, creds, path, creates)
	if err != nil {
		return err
	}
	complete := true
	for i, want := range creates {
		id := ""
		for _, got := range created {
			if got.ID != "" && got.Name == want.Name && got.Type == "url" && got.Content.URL == want.Content.URL {
				id = got.ID
				break
			}
		}
		if id == "" {
			complete = false
		} else {
			tabs[createdIndexes[i]].ID = id
		}
	}
	if !complete {
		return fmt.Errorf("lark http client: created chat tab response missing tab IDs")
	}
	return nil
}

func (c *httpAPIClient) writeChatTabs(ctx context.Context, creds InstallationCredentials, path string, tabs []chatTab) ([]chatTab, error) {
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Tabs []chatTab `json:"chat_tabs"`
		} `json:"data"`
	}
	if err := c.doAuthedJSON(ctx, creds, http.MethodPost, path, map[string]any{"chat_tabs": tabs}, &resp); err != nil {
		return nil, fmt.Errorf("lark http client: sync chat tabs: %w", err)
	}
	if resp.Code != 0 {
		if isTokenError(resp.Code) {
			c.invalidateToken(creds.AppID)
		}
		return nil, &APIError{Op: "sync chat tabs", Code: resp.Code, Msg: resp.Msg}
	}
	return resp.Data.Tabs, nil
}
