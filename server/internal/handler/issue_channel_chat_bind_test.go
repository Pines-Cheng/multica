package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateIssue_ChannelChatIDMustBeAFeishuChat(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	w := httptest.NewRecorder()
	testHandler.CreateIssue(w, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": "Bad chat id", "channel_chat_id": "not-a-chat",
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// A failed bind keeps the issue and reports why, and the chat endpoint
// validates the chat id the retry names.
func TestCreateIssue_ChannelChatBindFailureKeepsTheIssue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	if testHandler.LarkIssueChat != nil {
		t.Skip("test expects the default handler without Feishu")
	}
	w := httptest.NewRecorder()
	testHandler.CreateIssue(w, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": "Bind existing chat", "channel_chat_id": "oc_existing",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created CreateIssueResponse
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	t.Cleanup(func() {
		cleanup := withURLParam(newRequest("DELETE", "/api/issues/"+created.ID, nil), "id", created.ID)
		testHandler.DeleteIssue(httptest.NewRecorder(), cleanup)
	})
	if created.ID == "" || created.ChannelChat != nil || created.ChannelChatError == nil || created.ChannelChatError.Code != "lark_not_configured" {
		t.Fatalf("response = %+v", created)
	}

	w = httptest.NewRecorder()
	req := withURLParam(newRequest("POST", "/api/issues/"+created.ID+"/channel-chat", map[string]any{"chat_id": "nope"}), "id", created.ID)
	testHandler.CreateIssueChannelChat(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid retry chat id: expected 400, got %d: %s", w.Code, w.Body.String())
	}
}
