package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

func TestChannelIssueRetry_OnlyBlockedTargetsAndSameComment(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	allowed := createHandlerTestAgent(t, "Already admitted", nil)
	blocked, _, _ := privateAgentTestFixture(t)
	issueID := dbfx.Issue(t, "Channel retry")
	dbfx.Cleanup(t, `DELETE FROM comment WHERE issue_id = $1`, issueID)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	messageID := "om_" + uuidToString(dbid.NewV7())
	installationID, bindingID, lease := dbid.NewV7(), dbid.NewV7(), dbid.NewV7()
	dbfx.Insert(t, "channel_issue_comment_pending", testutil.Cols{
		"id": dbid.NewV7(), "workspace_id": testWorkspaceID, "issue_id": issueID,
		"binding_id": bindingID, "installation_id": installationID, "author_user_id": testUserID,
		"channel_message_id": messageID, "message": []byte(`{}`), "lease_token": lease,
	})
	dbfx.Cleanup(t, `DELETE FROM channel_issue_message WHERE channel_message_id = $1`, messageID)
	result, err := testHandler.CreateChannelIssueComment(ctx, lark.ChannelIssueCommentParams{
		WorkspaceID: parseUUID(testWorkspaceID), IssueID: parseUUID(issueID), AuthorUserID: parseUUID(testUserID), BindingID: bindingID, LeaseToken: lease,
		Content: fmt.Sprintf("[@Allowed](mention://agent/%s) [@Blocked](mention://agent/%s) check progress", allowed, blocked),
		Origin:  lark.CommentChannelOrigin{ChannelType: "feishu", InstallationID: uuidToString(installationID), MessageID: messageID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Outcomes) != 2 || result.Outcomes[0].Status != "queued" || result.Outcomes[1].Status != "blocked" || result.Outcomes[1].ReasonCode != "invocation_not_allowed" {
		t.Fatalf("partial result lost: %+v", result.Outcomes)
	}
	raw, _ := json.Marshal(result.Outcomes)
	dbfx.Exec(t, `UPDATE channel_issue_comment_pending SET dispatch_outcomes = $1 WHERE channel_message_id = $2`, raw, messageID)
	// Finish the accepted task before retry. A duplicate would now be able to
	// enqueue, so the test does not rely on the active-task unique index.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE issue_id = $1`, issueID)
	dbfx.Exec(t, `UPDATE agent SET permission_mode = 'public_to', visibility = 'workspace' WHERE id = $1`, blocked)
	dbfx.Insert(t, "agent_invocation_target", testutil.Cols{"agent_id": blocked, "target_type": "workspace", "target_id": testWorkspaceID})
	job, err := testHandler.Queries.GetChannelIssueCommentRequest(ctx, db.GetChannelIssueCommentRequestParams{ChannelMessageID: messageID, WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	retried, err := testHandler.RetryChannelIssueComment(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if retried.ID != result.ID || len(retried.Outcomes) != 2 || retried.Outcomes[1].Status != "queued" {
		t.Fatalf("retry = %+v", retried)
	}
	for _, target := range []string{allowed, blocked} {
		if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, target); n != 1 {
			t.Fatalf("tasks for %s = %d", target, n)
		}
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'member'`, issueID); n != 1 {
		t.Fatalf("original comment duplicated: %d", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND trigger_comment_id = $2`, issueID, result.ID); n != 2 {
		t.Fatalf("retry lost original comment attribution: %d", n)
	}
	if _, err := testHandler.RetryChannelIssueComment(ctx, job); err == nil {
		t.Fatal("the same dispatch lease replayed after feedback became durable")
	}
	// Editing on the web invalidates the original retry without overwriting it.
	dbfx.Exec(t, `UPDATE channel_issue_comment_pending SET state = 'dispatching' WHERE channel_message_id = $1`, messageID)
	dbfx.Exec(t, `UPDATE comment SET content = 'edited on web' WHERE id = $1`, result.ID)
	if _, err := testHandler.RetryChannelIssueComment(ctx, job); !errors.Is(err, lark.ErrChannelCommentChanged) {
		t.Fatalf("edited original replayed: %v", err)
	}
}
