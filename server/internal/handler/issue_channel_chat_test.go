package handler

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/dbid"
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
	installationID, bindingID, leaseToken := dbid.NewV7(), dbid.NewV7(), dbid.NewV7()
	dbfx.Insert(t, "channel_issue_comment_pending", testutil.Cols{
		"id": dbid.NewV7(), "workspace_id": testWorkspaceID, "issue_id": issueID,
		"binding_id": bindingID, "installation_id": installationID, "author_user_id": testUserID,
		"channel_message_id": "om_channel_1", "message": []byte(`{}`), "lease_token": leaseToken,
	})
	dbfx.Cleanup(t, `DELETE FROM channel_issue_message WHERE channel_message_id = $1`, "om_channel_1")

	h := *testHandler
	h.Bus = events.New()
	var origins []any
	h.Bus.Subscribe(protocol.EventCommentCreated, func(e events.Event) {
		if payload, ok := e.Payload.(map[string]any); ok {
			origins = append(origins, payload[lark.CommentChannelOriginKey])
		}
	})

	result, err := h.CreateChannelIssueComment(context.Background(), lark.ChannelIssueCommentParams{
		WorkspaceID:  parseUUID(testWorkspaceID),
		IssueID:      parseUUID(issueID),
		AuthorUserID: parseUUID(testUserID),
		BindingID:    bindingID, LeaseToken: leaseToken,
		Content: fmt.Sprintf("[@Target](mention://agent/%s) please take this", agentID),
		Origin:  lark.CommentChannelOrigin{ChannelType: "feishu", InstallationID: uuidToString(installationID), MessageID: "om_channel_1"},
	})
	if err != nil {
		t.Fatalf("CreateChannelIssueComment: %v", err)
	}
	commentID := result.ID
	if len(result.Outcomes) != 1 || result.Outcomes[0].Status != "queued" {
		t.Fatalf("admission outcomes lost: %+v", result)
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

func TestCreateChannelIssueCommentCommitsMediaAndSourceAtomically(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, outcome := range []string{"success", "missing media intent", "lost queue lease"} {
		t.Run(outcome, func(t *testing.T) {
			agentID := createHandlerTestAgent(t, "Channel attachment target", nil)
			issueID := dbfx.Issue(t, "Channel attachment comment")
			attachmentID, installationID, bindingID, leaseToken := dbid.NewV7(), dbid.NewV7(), dbid.NewV7(), dbid.NewV7()
			messageID := "om_" + uuidToString(dbid.NewV7())
			key := "channel-comment/" + messageID
			url := "https://storage.example.test/" + key
			dbfx.Insert(t, "channel_issue_comment_pending", testutil.Cols{
				"id": dbid.NewV7(), "workspace_id": testWorkspaceID, "issue_id": issueID,
				"binding_id": bindingID, "installation_id": installationID, "author_user_id": testUserID,
				"channel_message_id": messageID, "message": []byte(`{}`), "lease_token": leaseToken,
			})
			if outcome != "missing media intent" {
				dbfx.InsertNoID(t, "channel_media_pending_object", testutil.Cols{
					"storage_key": key, "workspace_id": testWorkspaceID, "chat_message_id": dbid.NewV7(),
					"storage_url": url, "installation_id": installationID,
				}, "storage_key = $1", key)
			}
			dbfx.Cleanup(t, `DELETE FROM channel_issue_message WHERE channel_message_id = $1`, messageID)
			dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
			dbfx.Cleanup(t, `DELETE FROM attachment WHERE issue_id = $1`, issueID)
			dbfx.Cleanup(t, `DELETE FROM comment WHERE issue_id = $1`, issueID)
			h := *testHandler
			h.Bus = events.New()
			var broadcast *CommentResponse
			h.Bus.Subscribe(protocol.EventCommentCreated, func(e events.Event) {
				if payload, ok := e.Payload.(map[string]any); ok {
					if c, ok := payload["comment"].(CommentResponse); ok {
						broadcast = &c
					}
				}
			})
			if outcome == "lost queue lease" {
				leaseToken = dbid.NewV7()
			}
			result, err := h.CreateChannelIssueComment(context.Background(), lark.ChannelIssueCommentParams{
				WorkspaceID: parseUUID(testWorkspaceID), IssueID: parseUUID(issueID), AuthorUserID: parseUUID(testUserID),
				BindingID: bindingID, LeaseToken: leaseToken,
				SyncState: []byte(`{"Incomplete":true}`),
				Content:   fmt.Sprintf("[@Target](mention://agent/%s) ![](/api/attachments/%s/download)", agentID, uuidToString(attachmentID)),
				Origin:    lark.CommentChannelOrigin{ChannelType: "feishu", InstallationID: uuidToString(installationID), MessageID: messageID},
				Attachments: []lark.ChannelIssueCommentAttachment{{ID: attachmentID, MediaRef: channel.MediaRef{
					Type: channel.MsgTypeImage, StorageKey: key, StorageURL: url, Filename: "shot.png", MimeType: "image/png", SizeBytes: 12,
				}}},
			})
			commentID := result.ID
			if outcome != "success" {
				if err == nil {
					t.Fatal("expected the transaction to fail")
				}
				if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, issueID); n != 0 {
					t.Fatalf("comments after rollback = %d", n)
				}
				if n := dbfx.Count(t, `SELECT count(*) FROM attachment WHERE issue_id = $1`, issueID); n != 0 {
					t.Fatalf("attachments after rollback = %d", n)
				}
				if n := dbfx.Count(t, `SELECT count(*) FROM channel_issue_message WHERE channel_message_id = $1`, messageID); n != 0 {
					t.Fatalf("source mappings after rollback = %d", n)
				}
				if n := dbfx.Count(t, `SELECT count(*) FROM channel_issue_comment_pending WHERE channel_message_id = $1`, messageID); n != 1 {
					t.Fatalf("pending messages after rollback = %d", n)
				}
				if broadcast != nil || countQueuedCommentTriggerTasks(t, issueID, agentID) != 0 {
					t.Fatal("failed comment published an event or triggered an agent")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if broadcast == nil || len(broadcast.Attachments) != 1 || broadcast.Attachments[0].ID != uuidToString(attachmentID) {
				t.Fatalf("broadcast did not include committed attachment: %+v", broadcast)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM attachment WHERE id = $1 AND comment_id = $2 AND issue_id = $3 AND workspace_id = $4`, attachmentID, commentID, issueID, testWorkspaceID); n != 1 {
				t.Fatalf("linked attachment count = %d", n)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM channel_issue_message WHERE channel_message_id = $1 AND comment_id = $2`, messageID, commentID); n != 1 {
				t.Fatalf("source mapping count = %d", n)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM channel_issue_message WHERE channel_message_id = $1 AND sync_state ->> 'Incomplete' = 'true'`, messageID); n != 1 {
				t.Fatalf("sync state was not committed with comment: count=%d", n)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM channel_issue_comment_pending WHERE channel_message_id = $1 AND state = 'pending'`, messageID); n != 0 {
				t.Fatalf("pending messages after commit = %d", n)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM channel_media_pending_object WHERE storage_key = $1`, key); n != 0 {
				t.Fatalf("unclaimed upload intents = %d", n)
			}
			if n := countQueuedCommentTriggerTasks(t, issueID, agentID); n != 1 {
				t.Fatalf("agent runs = %d", n)
			}
		})
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
