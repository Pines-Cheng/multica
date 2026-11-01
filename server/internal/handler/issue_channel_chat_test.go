package handler

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// A message in an issue-bound chat is the member's own comment: an agent
// mention in it queues that agent exactly as the web composer would, and the
// broadcast carries the channel origin so the mirror never posts it back.
func TestCreateChannelIssueComment_BehavesLikeAWebComment(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "Channel comment target", nil)
	issueID := dbfx.Issue(t, "Channel comment issue")
	dbfx.Cleanup(t, `DELETE FROM comment WHERE issue_id = $1`, issueID)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)

	h := *testHandler
	h.Bus = events.New()
	var origins []any
	h.Bus.Subscribe(protocol.EventCommentCreated, func(e events.Event) {
		if payload, ok := e.Payload.(map[string]any); ok {
			origins = append(origins, payload[lark.CommentChannelOriginKey])
		}
	})

	commentID, err := h.CreateChannelIssueComment(context.Background(), lark.ChannelIssueCommentParams{
		WorkspaceID:  parseUUID(testWorkspaceID),
		IssueID:      parseUUID(issueID),
		AuthorUserID: parseUUID(testUserID),
		Content:      fmt.Sprintf("[@Target](mention://agent/%s) please take this", agentID),
		Origin:       lark.CommentChannelOrigin{ChannelType: "feishu", MessageID: "om_channel_1"},
	})
	if err != nil {
		t.Fatalf("CreateChannelIssueComment: %v", err)
	}
	var authorType, authorID string
	dbfx.QueryRow(t, `SELECT author_type, author_id::text FROM comment WHERE id = $1`, commentID).Scan(&authorType, &authorID)
	if authorType != "member" || authorID != testUserID {
		t.Fatalf("comment author = %s/%s, want member/%s", authorType, authorID, testUserID)
	}
	if got := countQueuedCommentTriggerTasks(t, issueID, agentID); got != 1 {
		t.Fatalf("queued tasks for the mentioned agent = %d, want 1", got)
	}
	if len(origins) == 0 {
		t.Fatal("no comment:created broadcast")
	}
	origin, ok := origins[len(origins)-1].(lark.CommentChannelOrigin)
	if !ok || origin.MessageID != "om_channel_1" {
		t.Fatalf("broadcast origin = %#v", origins[len(origins)-1])
	}
}

func TestCreateChannelIssueComment_MissingIssueIsRejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	_, err := testHandler.CreateChannelIssueComment(context.Background(), lark.ChannelIssueCommentParams{
		WorkspaceID:  parseUUID(testWorkspaceID),
		IssueID:      parseUUID("00000000-0000-4000-8000-0000000000ff"),
		AuthorUserID: parseUUID(testUserID),
		Content:      "hello",
	})
	if !errors.Is(err, lark.ErrIssueCommentRejected) {
		t.Fatalf("err = %v, want ErrIssueCommentRejected", err)
	}
}
