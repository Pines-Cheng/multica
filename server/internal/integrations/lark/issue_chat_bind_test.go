package lark

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// A chat opened before work starts holds no task tab, pinned card or
// artifact tabs; the issue's first run adds them once.
func TestIssueChat_NewChatWaitsForFirstRun(t *testing.T) {
	f := newActivityFixture(t)
	f.svc.cfg.AppURL = "https://multica.example"
	issue := f.fx.Issue(t, "Planned", testutil.Cols{
		"status": "backlog", "assignee_type": "squad", "assignee_id": f.squad,
		"metadata": `{"PRD":"https://docs.example/prd"}`,
	})
	f.publishIssue(protocol.EventIssueCreated, issue, nil)
	binding, ok := f.binding(t, issue)
	if !ok || len(f.chats.created) != 1 {
		t.Fatal("the chat must still be created and bound")
	}
	f.bus.Publish(events.Event{Type: protocol.EventIssueMetadataChanged, WorkspaceID: f.ws, Payload: map[string]any{"issue_id": issue}})
	if len(f.chats.tabs) != 0 || len(f.chats.syncedTabs) != 0 || len(f.chats.pinned) != 0 || len(f.api.sent) != 0 {
		t.Fatalf("an unstarted issue touched its chat: tabs=%v synced=%d pinned=%v sent=%d",
			f.chats.tabs, len(f.chats.syncedTabs), f.chats.pinned, len(f.api.sent))
	}

	f.startRun(issue)
	if len(f.chats.tabs) != 1 || !strings.HasPrefix(f.chats.tabs[0], "任务详情=https://multica.example/") {
		t.Fatalf("task tab = %v", f.chats.tabs)
	}
	if len(f.chats.pinned) != 1 || len(f.chats.syncedTabs) != 1 || string(f.chats.syncedTabs[0].chatID) != binding.ChatID {
		t.Fatalf("first run must pin the progress card and sync artifacts: pinned=%v synced=%+v", f.chats.pinned, f.chats.syncedTabs)
	}
	if card := f.progressCard(t, issue); !strings.Contains(card, "[PRD](https://docs.example/prd)") {
		t.Fatalf("progress card = %s", card)
	}

	f.startRun(issue)
	if len(f.chats.tabs) != 1 || len(f.chats.pinned) != 1 {
		t.Fatalf("a later run repeated the setup: tabs=%v pinned=%v", f.chats.tabs, f.chats.pinned)
	}
}

func TestIssueChat_BindExistingChat(t *testing.T) {
	f := newActivityFixture(t)
	member := f.fx.Agent(t, "Member", f.fx.Runtime(t, "member-runtime"))
	f.fx.SquadMember(t, f.squad, "agent", member)
	f.installBot(t, member, "cli_bind_member")
	ctx := context.Background()
	bind := func(issueID, chatID string) (IssueChat, error) {
		return f.svc.BindChat(ctx, BindIssueChatParams{
			WorkspaceID: uuidFromString(t, f.ws), IssueID: uuidFromString(t, issueID),
			ActorUserID: uuidFromString(t, f.user), ChatID: chatID,
		})
	}

	issue := f.fx.Issue(t, "Bound", testutil.Cols{"assignee_type": "squad", "assignee_id": f.squad})
	f.publishIssue(protocol.EventIssueCreated, issue, map[string]any{IssueEventBindsChat: true})
	if len(f.chats.created) != 0 {
		t.Fatal("an issue binding an existing chat must not open a new one")
	}
	chat, err := bind(issue, "oc_existing")
	if err != nil || chat.ChatID != "oc_existing" {
		t.Fatalf("BindChat = %+v, %v", chat, err)
	}
	row, ok := f.binding(t, issue)
	if !ok || row.StewardInstallationID != f.steward.ID || row.RootIssueID != row.IssueID || row.ThreadRootMessageID != "" {
		t.Fatalf("binding = %+v", row)
	}
	invited := false
	for _, a := range f.chats.added {
		invited = invited || (a.ChatID == "oc_existing" && a.MemberIDType == MemberIDTypeAppID && len(a.IDs) == 1 && a.IDs[0] == "cli_bind_member")
	}
	if !invited {
		t.Fatalf("squad bots were not invited: %+v", f.chats.added)
	}
	if len(f.chats.tabs) != 0 || len(f.chats.pinned) != 0 || len(f.api.sent) != 0 {
		t.Fatal("binding must leave the chat's tabs, top notice and messages alone until work starts")
	}

	if again, err := bind(issue, "oc_existing"); err != nil || again.ChatID != "oc_existing" {
		t.Fatalf("rebinding the same chat = %+v, %v", again, err)
	}
	if _, err := bind(issue, "oc_other"); !errors.Is(err, ErrIssueChatIssueBound) {
		t.Fatalf("rebinding to another chat err = %v", err)
	}
	other := f.fx.Issue(t, "Other", testutil.Cols{"assignee_type": "squad", "assignee_id": f.squad})
	if _, err := bind(other, "oc_existing"); !errors.Is(err, ErrIssueChatTaken) {
		t.Fatalf("binding a taken chat err = %v", err)
	}
	f.chats.infos = map[ChatID]ChatInfo{"oc_no_bot": {}, "oc_external": {BotInChat: true, External: true}}
	if _, err := bind(other, "oc_no_bot"); !errors.Is(err, ErrIssueChatBotNotInChat) {
		t.Fatalf("binding without the steward bot err = %v", err)
	}
	if _, err := bind(other, "oc_external"); !errors.Is(err, ErrIssueChatExternal) {
		t.Fatalf("binding an external chat err = %v", err)
	}
	if _, ok := f.binding(t, other); ok {
		t.Fatal("a refused bind must not leave a binding")
	}

	f.startRun(issue)
	if len(f.chats.pinned) != 1 || len(f.api.sent) != 1 || string(f.api.sent[0].ChatID) != "oc_existing" {
		t.Fatalf("the first run must post and pin the progress card in the bound chat: pinned=%v sent=%+v", f.chats.pinned, f.api.sent)
	}
}

func TestHTTPClient_GetChatInfo(t *testing.T) {
	fake := newLarkFake(t)
	fake.stubToken("tok_chat_info", 7200)
	replies := map[string]func(w http.ResponseWriter){
		"oc_member": func(w http.ResponseWriter) {
			writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"chat_mode": "group", "external": false}})
		},
		"oc_external": func(w http.ResponseWriter) {
			writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"chat_mode": "group", "external": true}})
		},
		"oc_public_view": func(w http.ResponseWriter) {
			writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"name": "Group", "user_count": "3"}})
		},
		"oc_refused": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":232011,"msg":"Operator can NOT be out of the chat."}`))
		},
		"oc_missing": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":232006,"msg":"invalid chat_id"}`))
		},
		"oc_forbidden": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":99991672,"msg":"Access denied"}`))
		},
	}
	for chatID, reply := range replies {
		fake.mux.HandleFunc("/open-apis/im/v1/chats/"+chatID, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("method = %s, want GET", r.Method)
			}
			reply(w)
		})
	}
	client := newTestClient(fake, time.Now)
	for chatID, want := range map[string]ChatInfo{
		"oc_member":      {BotInChat: true},
		"oc_external":    {BotInChat: true, External: true},
		"oc_public_view": {},
		"oc_refused":     {},
	} {
		got, err := client.GetChatInfo(context.Background(), testCreds(), ChatID(chatID))
		if err != nil || got != want {
			t.Errorf("%s: GetChatInfo = %+v, %v; want %+v", chatID, got, err, want)
		}
	}
	if _, err := client.GetChatInfo(context.Background(), testCreds(), "oc_missing"); !errors.Is(err, ErrChatNotFound) {
		t.Errorf("invalid chat err = %v", err)
	}
	var apiErr *APIError
	if _, err := client.GetChatInfo(context.Background(), testCreds(), "oc_forbidden"); !errors.As(err, &apiErr) || apiErr.Code != 99991672 {
		t.Errorf("permission refusal err = %v", err)
	}
}
