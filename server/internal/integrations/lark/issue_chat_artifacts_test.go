package lark

import (
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestIssueChatArtifactsRefreshWithoutKindRulesOrRepinning(t *testing.T) {
	f := newActivityFixture(t)
	f.svc.cfg.AppURL = "https://multica.example"
	root := f.fx.Issue(t, "Generic deliverable", testutil.Cols{
		"assignee_type": "squad", "assignee_id": f.squad,
		"artifacts": `{"report":{"title":"压测报告","url":"https://docs.example/report","kind":"unregistered-kind-v9"}}`,
	})
	f.publishIssue(protocol.EventIssueCreated, root, nil)
	assertArtifact := func(title, url string) {
		t.Helper()
		last := f.chats.syncedTabs[len(f.chats.syncedTabs)-1]
		if len(last.tabs) != 1 || last.tabs[0].Name != title || last.tabs[0].URL != url {
			t.Fatalf("tabs must follow declared data: %+v", last.tabs)
		}
		if card := f.progressCard(t, root); !strings.Contains(card, "["+title+"]("+url+")") || strings.Contains(card, "unregistered-kind-v9") {
			t.Fatalf("card must use the declared title, not the kind: %s", card)
		}
	}
	assertArtifact("压测报告", "https://docs.example/report")
	firstID := f.chats.syncedTabs[len(f.chats.syncedTabs)-1].tabs[0].ID
	f.fx.Exec(t, `UPDATE issue SET artifacts = '{"report":{"title":"最新压测报告","url":"https://docs.example/revised"}}' WHERE id = $1`, root)
	f.bus.Publish(events.Event{Type: protocol.EventIssueArtifactsChanged, WorkspaceID: f.ws, Payload: map[string]any{"issue_id": root}})
	assertArtifact("最新压测报告", "https://docs.example/revised")
	if last := f.chats.syncedTabs[len(f.chats.syncedTabs)-1].tabs[0]; firstID == "" || last.ID != firstID {
		t.Fatalf("renamed artifact must reuse its persisted tab ID: %+v; first ID=%s", last, firstID)
	}
	f.fx.Exec(t, `UPDATE issue SET artifacts = '{}' WHERE id = $1`, root)
	f.bus.Publish(events.Event{Type: protocol.EventIssueArtifactsChanged, WorkspaceID: f.ws, Payload: map[string]any{"issue_id": root}})
	if card := f.progressCard(t, root); strings.Contains(card, "相关资料") || strings.Contains(card, "docs.example") {
		t.Fatalf("deleted artifacts must disappear from the pinned card: %s", card)
	}
	if len(f.chats.pinned) != 1 {
		t.Fatalf("artifact changes must refresh the existing card, pin calls=%d", len(f.chats.pinned))
	}
}
