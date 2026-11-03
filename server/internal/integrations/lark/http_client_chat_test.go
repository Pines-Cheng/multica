package lark

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
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

func TestHTTPClient_SyncChatURLTabs(t *testing.T) {
	fake := newLarkFake(t)
	fake.stubToken("tok_chat_tab", 7200)
	const path = "/open-apis/im/v1/chats/oc_issue/chat_tabs"
	existing := []map[string]any{
		{"tab_id": "task", "tab_name": "任务详情", "tab_type": "url", "tab_content": map[string]string{"url": "https://app.example/task"}},
		{"tab_id": "mr", "tab_name": "MR", "tab_type": "url", "tab_content": map[string]string{"url": "https://code.example/old"}, "tab_config": map[string]bool{"is_built_in": true}},
		{"tab_id": "ppe", "tab_name": "PPE", "tab_type": "url", "tab_content": map[string]string{"url": "https://ppe.example"}},
		{"tab_id": "notes", "tab_name": "团队笔记", "tab_type": "doc"},
		{"tab_id": "manual-prd", "tab_name": "PRD", "tab_type": "doc"},
	}
	reads, writes := 0, 0
	fake.mux.HandleFunc(path+"/list_tabs", func(w http.ResponseWriter, r *http.Request) {
		reads++
		if r.Method != http.MethodGet {
			t.Errorf("list method = %s, want GET", r.Method)
		}
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"chat_tabs": existing}})
	})
	for _, endpoint := range []string{path, path + "/update_tabs"} {
		fake.mux.HandleFunc(endpoint, func(w http.ResponseWriter, r *http.Request) {
			writes++
			if r.Method != http.MethodPost {
				t.Errorf("write method = %s, want POST", r.Method)
			}
			var body struct {
				Tabs []map[string]any `json:"chat_tabs"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode: %v", err)
			}
			var names []string
			for _, tab := range body.Tabs {
				names = append(names, tab["tab_name"].(string))
				if tab["tab_type"] != "url" || tab["tab_config"].(map[string]any)["is_built_in"] != true {
					t.Errorf("tab must be an in-app URL: %+v", tab)
				}
				id, _ := tab["tab_id"].(string)
				if r.URL.Path == path {
					if id != "" {
						t.Errorf("new tab has id: %+v", tab)
					}
					tab["tab_id"] = "new-" + tab["tab_name"].(string)
					existing = append(existing, tab)
				} else {
					found := false
					for i, old := range existing {
						if old["tab_id"] == id {
							existing[i] = tab
							found = true
						}
					}
					if !found {
						t.Errorf("update must address an existing tab: %+v", tab)
					}
				}
			}
			want := []string{"Bits", "Meego"}
			if r.URL.Path == path+"/update_tabs" {
				want = []string{"MR", "PPE"}
			}
			if !reflect.DeepEqual(names, want) {
				t.Errorf("write %s names = %v, want %v", r.URL.Path, names, want)
			}
			writeJSON(w, map[string]any{"code": 0})
		})
	}
	client := newTestClient(fake, time.Now)
	tabs := []ChatURLTab{
		{Name: "Bits", URL: "https://bits.example/task/1"},
		{Name: "MR", URL: "https://code.example/new"},
		{Name: "Meego", URL: "https://meego.example/1"},
		{Name: "PPE", URL: "https://ppe.example"},
		{Name: "PRD", URL: "https://docs.example/spec"},
	}
	for i := 0; i < 2; i++ {
		if err := client.SyncChatURLTabs(context.Background(), testCreds(), "oc_issue", tabs); err != nil {
			t.Fatalf("sync: %v", err)
		}
	}
	if reads != 2 || writes != 2 || len(existing) != 7 {
		t.Fatalf("repeat sync must not write or add duplicates: reads=%d writes=%d tabs=%+v", reads, writes, existing)
	}
	if existing[0]["tab_id"] != "task" || existing[3]["tab_id"] != "notes" || existing[4]["tab_type"] != "doc" {
		t.Fatalf("unrelated and manual document tabs changed: %+v", existing)
	}
	if got := existing[1]["tab_content"].(map[string]any)["url"]; got != "https://code.example/new" {
		t.Fatalf("MR URL = %v", got)
	}
}

func TestHTTPClient_SyncChatURLTabs_APIErrors(t *testing.T) {
	for _, stage := range []string{"list", "update", "create"} {
		t.Run(stage, func(t *testing.T) {
			fake := newLarkFake(t)
			fake.stubToken("tok_chat_tab", 7200)
			const path = "/open-apis/im/v1/chats/oc_issue/chat_tabs"
			writes := 0
			fake.mux.HandleFunc(path+"/list_tabs", func(w http.ResponseWriter, r *http.Request) {
				if stage == "list" {
					writeJSON(w, map[string]any{"code": 99991672, "msg": "missing scope"})
					return
				}
				tabs := []map[string]any{}
				if stage == "update" {
					tabs = append(tabs, map[string]any{"tab_id": "prd", "tab_name": "PRD", "tab_type": "url"})
				}
				writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"chat_tabs": tabs}})
			})
			for _, endpoint := range []string{path, path + "/update_tabs"} {
				fake.mux.HandleFunc(endpoint, func(w http.ResponseWriter, r *http.Request) {
					writes++
					writeJSON(w, map[string]any{"code": 232001, "msg": "tab update rejected"})
				})
			}
			client := newTestClient(fake, time.Now)
			err := client.SyncChatURLTabs(context.Background(), testCreds(), "oc_issue", []ChatURLTab{{Name: "PRD", URL: "https://docs.example/spec"}})
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error = %v, want APIError", err)
			}
			if stage == "list" && writes != 0 {
				t.Fatal("must not create tabs when listing is denied")
			}
		})
	}
}
