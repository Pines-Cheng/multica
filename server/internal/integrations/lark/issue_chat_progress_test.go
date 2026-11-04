package lark

import (
	"context"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestIssueChatOverview_ExplainsContentsAndReflectsCurrentWork(t *testing.T) {
	f := newActivityFixture(t)
	f.svc.cfg.AppURL = "https://multica.example"
	root := f.fx.Issue(t, "Search [beta] & *filters*", testutil.Cols{
		"status": "in_progress", "assignee_type": "squad", "assignee_id": f.squad,
		"metadata": `{"PRD":"https://docs.example/spec"}`,
	})
	f.publishIssue(protocol.EventIssueCreated, root, nil)
	render := func() decodedIssueCard {
		t.Helper()
		issue, err := f.store.GetIssue(context.Background(), uuidFromString(t, root))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := f.svc.progressCardJSON(context.Background(), issue, issue.ID)
		if err != nil {
			t.Fatal(err)
		}
		return decodeIssueCard(t, raw)
	}
	card := render()
	if card.Header.Title.Content != "任务进展与产物" || card.Header.Subtitle.Content != "进行中 · Chat Squad" || card.Config.Summary.Content != "任务进展与产物｜进行中" {
		t.Fatalf("overview must explain its contents without repeating the issue key or title: %+v", card)
	}
	if len(card.Body.Elements) == 0 || card.Body.Elements[0].Content != `Search \[beta\] &amp; \*filters\*` {
		t.Fatalf("full task title must remain literal in the body: %+v", card.Body.Elements)
	}
	var body string
	for _, element := range card.Body.Elements {
		body += cardMarkdown(element)
	}
	if !strings.Contains(body, "[PRD](https://docs.example/spec)") {
		t.Fatalf("overview lost its artifact links: %s", body)
	}

	f.fx.Issue(t, "开发：Search", testutil.Cols{"parent_issue_id": root, "status": "in_progress"})
	if got := render().Config.Summary.Content; got != "任务进展与产物｜开发 · 进行中" {
		t.Fatalf("active work must appear in the pinned preview: %s", got)
	}
	f.fx.Issue(t, "合入：Search", testutil.Cols{"parent_issue_id": root, "status": "blocked"})
	wantBlocked := "任务进展与产物｜合入 · 已阻塞"
	if got := render().Config.Summary.Content; got != wantBlocked {
		t.Fatalf("a blocker must precede other active work: %s", got)
	}
	f.fx.Issue(t, "回归：Search", testutil.Cols{"parent_issue_id": root, "status": "backlog"})
	if got := render().Config.Summary.Content; got != wantBlocked {
		t.Fatalf("adding planned work must not turn the preview into a completion ratio: %s", got)
	}
	for _, tc := range []struct{ status, want string }{
		{"in_review", "待验收"},
		{"blocked", "已阻塞"},
		{"done", "已完成"},
		{"cancelled", "已取消"},
	} {
		f.fx.Exec(t, "UPDATE issue SET status = $2 WHERE id = $1", root, tc.status)
		if got := render().Config.Summary.Content; got != "任务进展与产物｜"+tc.want {
			t.Errorf("%s: preview must reflect the parent state rather than stale child work: %s", tc.status, got)
		}
	}
}
