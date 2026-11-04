package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/integrations/lark"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type syncTestCredentials struct{}

func (syncTestCredentials) DecryptAppSecret(lark.Installation) (string, error) {
	return "test-secret", nil
}

type syncTestMedia struct {
	calls int
	ref   channel.MediaRef
}

func (m *syncTestMedia) HasMedia(channel.InboundMessage) bool { return true }
func (m *syncTestMedia) ResolveMedia(_ context.Context, _ engine.ResolvedInstallation, _ engine.ResolvedIdentity, _, _ pgtype.UUID, msg channel.InboundMessage) channel.InboundMessage {
	m.calls++
	msg.MediaRefs = []channel.MediaRef{m.ref}
	return msg
}

func TestRetryChannelMessageSyncRepairsExistingCommentWithoutRetriggering(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, scenario := range []string{"success and replay", "edited", "deleted", "other workspace", "other author"} {
		t.Run(scenario, func(t *testing.T) {
			fx := testutil.New(testPool, testWorkspaceID, testUserID)
			issueID := fx.Issue(t, "Forwarded conversation")
			commentID := dbid.NewV7()
			fx.Insert(t, "comment", testutil.Cols{"id": commentID, "issue_id": issueID, "workspace_id": testWorkspaceID, "author_type": "member", "author_id": testUserID, "content": "Partial forward", "type": "comment"})
			installationID, bindingID := dbid.NewV7(), dbid.NewV7()
			messageID := "om_sync_" + uuidToString(dbid.NewV7())
			fx.Insert(t, "channel_installation", testutil.Cols{"id": installationID, "workspace_id": testWorkspaceID, "agent_id": dbid.NewV7(), "installer_user_id": testUserID, "channel_type": "feishu", "status": "active", "config": []byte(`{"app_id":"cli_sync","app_secret_encrypted":"c2VhbGVk"}`)})
			state := lark.ForwardedCommentState{
				Message: lark.InboundMessage{MessageID: messageID, MessageType: "merge_forward"}, Content: "Partial forward", Incomplete: true,
				Root: lark.ForwardedCommentMessage{Message: lark.LarkMessage{MessageID: messageID, MessageType: "merge_forward"}, Loaded: true, Children: []lark.ForwardedCommentMessage{
					{Message: lark.LarkMessage{MessageID: "om_image", MessageType: "image", Content: `{"image_key":"image"}`}, Sender: "Alice"},
				}},
			}
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			fx.Insert(t, "channel_issue_message", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issueID, "binding_id": bindingID, "installation_id": installationID, "channel_message_id": messageID, "direction": "inbound", "kind": "comment", "comment_id": commentID, "sync_state": raw})
			key := "forwarded-retry/" + messageID
			fx.InsertNoID(t, "channel_media_pending_object", testutil.Cols{"storage_key": key, "workspace_id": testWorkspaceID, "chat_message_id": dbid.NewV7(), "storage_url": "https://storage.test/" + key, "installation_id": installationID}, "storage_key = $1", key)
			media := &syncTestMedia{ref: channel.MediaRef{Type: channel.MsgTypeImage, StorageKey: key, StorageURL: "https://storage.test/" + key, Filename: "image.png", MimeType: "image/png", SizeBytes: 5}}
			h := *testHandler
			h.Bus = events.New()
			created, updated := 0, 0
			h.Bus.Subscribe(protocol.EventCommentCreated, func(events.Event) { created++ })
			h.Bus.Subscribe(protocol.EventCommentUpdated, func(events.Event) { updated++ })
			h.LarkIssueComments = lark.NewFeishuIssueChatRouter(lark.NewChannelStore(h.Queries), &h, nil, syncTestCredentials{}, nil)
			h.LarkIssueComments.SetMediaResolver(media)
			want := http.StatusNoContent
			switch scenario {
			case "edited":
				fx.Exec(t, `UPDATE comment SET content = 'Human edit', revision = revision + 1 WHERE id = $1`, commentID)
				want = http.StatusConflict
			case "deleted":
				fx.Exec(t, `UPDATE comment SET deleted_at = now() WHERE id = $1`, commentID)
				want = http.StatusNotFound
			case "other workspace":
				fx.Exec(t, `UPDATE channel_issue_message SET workspace_id = $1 WHERE channel_message_id = $2`, dbid.NewV7(), messageID)
				want = http.StatusNotFound
			case "other author":
				user := fx.User(t, "Forward author", "forward-author-"+messageID+"@example.test")
				fx.Exec(t, `UPDATE comment SET author_id = $1 WHERE id = $2`, user, commentID)
				// The fixture user is an owner; make this request as an ordinary member.
				member := fx.User(t, "Reader", "forward-reader-"+messageID+"@example.test")
				fx.Member(t, testWorkspaceID, member, "member")
				userID := member
				req := withURLParam(newRequest(http.MethodPost, "/api/channel-messages/"+messageID+"/retry", nil), "messageID", messageID)
				req.Header.Set("X-User-ID", userID)
				testutil.Call(t, h.RetryChannelMessageSync, req).Want(http.StatusForbidden)
				if media.calls != 0 {
					t.Fatal("unauthorized request downloaded media")
				}
				return
			}
			call := func() {
				req := withURLParam(newRequest(http.MethodPost, "/api/channel-messages/"+messageID+"/retry", nil), "messageID", messageID)
				testutil.Call(t, h.RetryChannelMessageSync, req).Want(want)
			}
			call()
			if want != http.StatusNoContent {
				if media.calls != 0 || updated != 0 {
					t.Fatalf("rejected retry changed comment: downloads=%d updated=%d", media.calls, updated)
				}
				return
			}
			call()
			if media.calls != 1 || created != 0 || updated != 1 {
				t.Fatalf("retry/replay effects: downloads=%d created=%d updated=%d", media.calls, created, updated)
			}
			if n := fx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, issueID); n != 1 {
				t.Fatalf("retry created %d comments", n)
			}
			if n := fx.Count(t, `SELECT count(*) FROM attachment WHERE comment_id = $1`, commentID); n != 1 {
				t.Fatalf("retry created %d attachments", n)
			}
			var body string
			fx.QueryRow(t, `SELECT content FROM comment WHERE id = $1`, commentID).Scan(&body)
			if strings.Contains(body, "Retry sync") || !strings.Contains(body, "Alice") || !strings.Contains(body, "/api/attachments/") {
				t.Fatalf("synced body: %s", body)
			}
			if n := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID); n != 0 {
				t.Fatalf("retry triggered %d runs", n)
			}
		})
	}
}
