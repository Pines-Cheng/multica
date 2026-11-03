package lark

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestHTTPClient_AddChatURLTab_OpensInApp(t *testing.T) {
	fake := newLarkFake(t)
	fake.stubToken("tok_chat_tab", 7200)
	const tabURL = "https://app.example.com/workspace/issues/DEV-22"
	called := false
	fake.mux.HandleFunc("/open-apis/im/v1/chats/oc_issue/chat_tabs", func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		var body struct {
			ChatTabs []struct {
				Name    string `json:"tab_name"`
				Type    string `json:"tab_type"`
				Content struct {
					URL string `json:"url"`
				} `json:"tab_content"`
				Config struct {
					IsBuiltIn bool `json:"is_built_in"`
				} `json:"tab_config"`
			} `json:"chat_tabs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		} else if len(body.ChatTabs) != 1 {
			t.Errorf("tabs = %d, want 1", len(body.ChatTabs))
		} else {
			tab := body.ChatTabs[0]
			if tab.Name != "任务详情" || tab.Type != "url" || tab.Content.URL != tabURL {
				t.Errorf("unexpected tab: %+v", tab)
			}
			if !tab.Config.IsBuiltIn {
				t.Error("URL tab must open inside the app")
			}
		}
		writeJSON(w, map[string]any{"code": 0, "msg": "ok"})
	})
	client := newTestClient(fake, time.Now)
	if err := client.AddChatURLTab(context.Background(), testCreds(), "oc_issue", "任务详情", tabURL); err != nil {
		t.Fatalf("AddChatURLTab: %v", err)
	}
	if !called {
		t.Fatal("chat tab endpoint was not called")
	}
}
