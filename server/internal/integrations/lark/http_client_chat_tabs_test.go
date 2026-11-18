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

func TestHTTPClient_SyncChatURLTabs_ReconcilesExistingLinks(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ownedID     string
		legacyURL   string
		legacyType  string
		full        bool
		deleteFails bool
		wantDelete  bool
		wantCreate  bool
	}{
		{name: "legacy link", legacyURL: "https://docs.example/report", legacyType: "url"},
		{name: "stale ownership", ownedID: "missing", legacyURL: "https://docs.example/report", legacyType: "url"},
		{name: "full chat", legacyURL: "https://docs.example/report", legacyType: "url", full: true},
		{name: "owned duplicate", ownedID: "owned", legacyURL: "https://docs.example/report", legacyType: "url", wantDelete: true},
		{name: "delete failure", ownedID: "owned", legacyURL: "https://docs.example/report", legacyType: "url", deleteFails: true, wantDelete: true},
		{name: "different link", legacyURL: "https://docs.example/other", legacyType: "url", wantCreate: true},
		{name: "document tab", legacyURL: "https://docs.example/report", legacyType: "doc", wantCreate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newLarkFake(t)
			fake.stubToken("tok_chat_tab", 7200)
			const path = "/open-apis/im/v1/chats/oc_issue/chat_tabs"
			legacy := chatTab{ID: "legacy", Name: "任务进展", Type: tc.legacyType}
			legacy.Content.URL = tc.legacyURL
			existing := []chatTab{legacy}
			if tc.ownedID == "owned" {
				duplicate := chatTab{ID: "owned", Name: "任务进展 (2)", Type: "url"}
				duplicate.Content.URL = "https://docs.example/report"
				existing = append(existing, duplicate)
			}
			if tc.full {
				for len(existing) < 20 {
					existing = append(existing, chatTab{ID: string(rune('a' + len(existing))), Name: "Other", Type: "url"})
				}
			}
			deletes, creates := 0, 0
			fake.mux.HandleFunc(path+"/list_tabs", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"chat_tabs": existing}})
			})
			fake.mux.HandleFunc(path+"/delete_tabs", func(w http.ResponseWriter, r *http.Request) {
				deletes++
				var body struct {
					IDs []string `json:"tab_ids"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if r.Method != http.MethodDelete || !reflect.DeepEqual(body.IDs, []string{"owned"}) {
					t.Fatalf("must delete only the persisted duplicate: %s %+v", r.Method, body)
				}
				if tc.deleteFails && deletes == 1 {
					writeJSON(w, map[string]any{"code": 232001, "msg": "denied"})
					return
				}
				existing = existing[:1]
				writeJSON(w, map[string]any{"code": 0})
			})
			fake.mux.HandleFunc(path+"/update_tabs", func(w http.ResponseWriter, r *http.Request) {
				t.Error("must not update the manual/legacy tab")
				writeJSON(w, map[string]any{"code": 232001})
			})
			fake.mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
				creates++
				var body struct {
					Tabs []chatTab `json:"chat_tabs"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if len(body.Tabs) != 1 || body.Tabs[0].Name != "任务进展 (2)" {
					t.Fatalf("different content should still get a distinct tab: %+v", body)
				}
				body.Tabs[0].ID = "new"
				existing = append(existing, body.Tabs...)
				writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"chat_tabs": body.Tabs}})
			})
			tabs := []ChatURLTab{{Key: "progress", ID: tc.ownedID, Name: "任务进展", URL: "https://docs.example/report"}}
			client := newTestClient(fake, time.Now)
			for attempt := range 3 {
				err := client.SyncChatURLTabs(context.Background(), testCreds(), "oc_issue", tabs)
				if tc.deleteFails && attempt == 0 {
					var apiErr *APIError
					if !errors.As(err, &apiErr) || tabs[0].ID != "owned" || len(existing) != 2 {
						t.Fatalf("failed cleanup must retain ownership for retry: err=%v tabs=%+v existing=%+v", err, tabs, existing)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			wantDeletes := 0
			if tc.wantDelete {
				wantDeletes = 1
			}
			if tc.deleteFails {
				wantDeletes++
			}
			if deletes != wantDeletes || (creates == 1) != tc.wantCreate || creates > 1 {
				t.Fatalf("unexpected writes: deletes=%d creates=%d", deletes, creates)
			}
			if existing[0] != legacy {
				t.Fatalf("legacy tab changed: %+v", existing[0])
			}
			if !tc.wantCreate && tabs[0].ID != "" {
				t.Fatalf("must clear obsolete ownership without adopting the legacy tab: %+v", tabs)
			}
		})
	}
}
