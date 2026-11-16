package lark

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type assistanceTestAPI struct{ *seqAPI }

func (a *assistanceTestAPI) GetMessage(_ context.Context, _ InstallationCredentials, messageID string) ([]LarkMessage, error) {
	return []LarkMessage{{MessageID: messageID, MessagePosition: "12", ThreadID: "omt_assistance", ThreadMessagePosition: "3"}}, nil
}

func TestIssueChatAssistance_CloseRefreshesBothCardsWithoutNewMessages(t *testing.T) {
	f := newActivityFixture(t)
	f.svc.cfg.AppURL = "https://multica.example"
	f.svc.cfg.API = &assistanceTestAPI{f.api}
	root := f.fx.Issue(t, "Parent", testutil.Cols{"assignee_type": "squad", "assignee_id": f.squad})
	f.publishIssue(protocol.EventIssueCreated, root, nil)
	f.startRun(root)
	child := f.fx.Issue(t, "开发：Parent", testutil.Cols{"parent_issue_id": root})
	f.publishIssue(protocol.EventIssueCreated, child, nil)
	binding, ok := f.binding(t, child)
	if !ok {
		t.Fatal("topic binding missing")
	}
	taskID := uuid.NewString()
	if err := f.store.RecordChannelIssueMessage(context.Background(), db.RecordChannelIssueMessageParams{
		WorkspaceID: uuidFromString(t, f.ws), IssueID: uuidFromString(t, child), BindingID: binding.ID,
		InstallationID: f.steward.ID, ChannelMessageID: "om_existing_run", Direction: "outbound", Kind: "task", TaskID: uuidFromString(t, taskID),
	}); err != nil {
		t.Fatal(err)
	}
	content := "Bits login requires authentication. [@Owner](mention://member/" + f.user + ")"
	comment := f.fx.Insert(t, "comment", testutil.Cols{"issue_id": child, "workspace_id": f.ws, "author_type": "agent", "author_id": f.leader, "content": content, "type": "comment", "assistance_status": "open", "source_task_id": taskID})
	publish := func(eventType, state string) {
		f.bus.Publish(events.Event{Type: eventType, WorkspaceID: f.ws, Payload: map[string]any{"comment": map[string]any{"id": comment, "issue_id": child, "author_type": "agent", "author_id": f.leader, "content": content, "type": "comment", "assistance_status": state, "source_task_id": taskID}}})
	}
	publish(protocol.EventCommentCreated, "open")
	messageID := f.api.lastSentID(t)
	for _, patch := range f.api.patched {
		if patch.LarkCardMessageID == "om_existing_run" {
			t.Fatal("a new assistance request must notify through a fresh card, not patch an already-read run card")
		}
	}
	card := decodeIssueCard(t, f.api.sent[len(f.api.sent)-1].CardJSON)
	if card.Header.Title.Content != "待 Owner 处理" {
		t.Fatalf("request card = %+v", card.Header)
	}
	progress := f.progressCard(t, root)
	if !strings.Contains(progress, "任务进展与产物｜待协助") {
		t.Fatalf("pinned preview must surface open assistance: %s", progress)
	}
	if !strings.Contains(progress, "待协助") || !strings.Contains(progress, "Bits login") || !strings.Contains(progress, "/client/thread/open?") || !strings.Contains(progress, "thread_position=3") {
		t.Fatalf("root card must expose the request and native topic link: %s", progress)
	}
	sent := len(f.api.sent)
	f.fx.Exec(t, "UPDATE comment SET assistance_status = 'closed' WHERE id = $1", comment)
	publish(protocol.EventCommentUpdated, "closed")
	if progress = f.progressCard(t, root); strings.Contains(progress, "待协助") || strings.Contains(progress, "Bits login") {
		t.Fatalf("closed request still pinned: %s", progress)
	}
	var latest string
	for _, patch := range f.api.patched {
		if patch.LarkCardMessageID == messageID {
			latest = patch.CardJSON
		}
	}
	if latest == "" || decodeIssueCard(t, latest).Header.Title.Content != "协助已处理" || !strings.Contains(latest, "Bits login") {
		t.Fatalf("original card did not close in place: %s", latest)
	}
	// A stale open event must read current persisted state rather than resurrecting the card.
	publish(protocol.EventCommentUpdated, "open")
	if len(f.api.sent) != sent || strings.Contains(f.progressCard(t, root), "待协助") {
		t.Fatal("close or replay posted messages or reopened assistance")
	}
	f.fx.Exec(t, "UPDATE comment SET assistance_status = 'open', content = 'Updated authentication request [@Owner](mention://member/"+f.user+")' WHERE id = $1", comment)
	publish(protocol.EventCommentUpdated, "open")
	if !strings.Contains(f.progressCard(t, root), "Updated authentication request") {
		t.Fatal("request edits and explicit reopen must refresh the pinned summary")
	}
	f.fx.Exec(t, "UPDATE comment SET deleted_at = now(), content = '' WHERE id = $1", comment)
	publish(protocol.EventCommentUpdated, "open")
	if strings.Contains(f.progressCard(t, root), "待协助") {
		t.Fatal("a deleted request must leave the pinned summary")
	}
	for _, patch := range f.api.patched {
		if patch.LarkCardMessageID == messageID {
			latest = patch.CardJSON
		}
	}
	if !strings.Contains(latest, "该评论已删除") || strings.Contains(latest, "待 Owner 处理") {
		t.Fatalf("a stale request update restored a deleted card: %s", latest)
	}
}

func TestAttentionNames_RequiresExplicitOpenAssistance(t *testing.T) {
	content := "[@Ada](mention://member/00000000-0000-4000-8000-000000000001)"
	for _, tc := range []struct{ state, body, want string }{{"", content, ""}, {"none", content, ""}, {"closed", content, ""}, {"open", content, "Ada"}, {"open", "`" + content + "`", ""}} {
		if got := attentionNames(commentEvent{AuthorType: "agent", Content: tc.body, AssistanceStatus: tc.state}); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.state, got, tc.want)
		}
	}
}

func TestChatMessageAppLink(t *testing.T) {
	link := chatMessageAppLink(RegionLark, "oc_chat", LarkMessage{ThreadID: "omt_topic", ThreadMessagePosition: "0"})
	u, err := url.Parse(link)
	if err != nil || u.Host != "applink.larksuite.com" || u.Path != "/client/thread/open" || u.Query().Get("openthreadid") != "omt_topic" || u.Query().Get("thread_position") != "0" {
		t.Fatalf("thread link: %s (%v)", link, err)
	}
	if got := chatMessageAppLink(RegionFeishu, "oc_chat", LarkMessage{MessagePosition: "12"}); got != "https://applink.feishu.cn/client/chat/open?openChatId=oc_chat&position=12" {
		t.Fatal(got)
	}
	if got := chatMessageAppLink(RegionFeishu, "oc_chat", LarkMessage{MessagePosition: "-1"}); got != "" {
		t.Fatal(got)
	}
}
