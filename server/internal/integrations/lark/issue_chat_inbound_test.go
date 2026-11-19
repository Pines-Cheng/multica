package lark

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

func TestIssueCommentMediaMarkdownPreservesSpansAndPartialFailures(t *testing.T) {
	lm := InboundMessage{MessageID: "om_mixed", MessageType: "post", Content: `{
		"title":"Review",
		"content":[
			[{"tag":"text","text":"literal [Image]"},{"tag":"img","image_key":"failed"}],
			[{"tag":"text","text":"second"},{"tag":"img","image_key":"ok"},{"tag":"text","text":"after"}],
			[{"tag":"img","image_key":"ok"},{"tag":"file","file_key":"doc","file_name":"report.pdf"}]
		]}`}
	inst := engine.ResolvedInstallation{ID: uuidFromString(t, icStewardAgt), WorkspaceID: uuidFromString(t, icWorkspace)}
	mediaID := uuidFromString(t, icNewComment)
	links := map[string]string{}
	for _, res := range mediaResourcesFromMessage(lm) {
		switch res.key {
		case "ok":
			links[mediaObjectKey(inst, mediaID, res)] = "![](/api/attachments/image/download)"
		case "doc":
			links[mediaObjectKey(inst, mediaID, res)] = "[report.pdf](/api/attachments/file/download)"
		}
	}
	got := issueCommentMediaMarkdown(lm, inst, mediaID, links)
	want := "Review\nliteral [Image] [Attachment unavailable]\nsecond ![](/api/attachments/image/download) after\n![](/api/attachments/image/download) [report.pdf](/api/attachments/file/download)"
	if got != want {
		t.Fatalf("markdown = %q, want %q", got, want)
	}
}

func TestIssueCommentQueueOrdersRepliesAndReclaimsExpiredWork(t *testing.T) {
	f := newIssueChatFixture(t)
	fx := testutil.New(f.pool, icWorkspace, icMember)
	ctx := context.Background()
	for i, messageID := range []string{"om_order_image", "om_order_reply", "om_other_binding"} {
		bindingID := f.binding.ID
		if i == 2 {
			bindingID = dbid.NewV7()
		}
		// Sort ahead of normal v7 IDs without touching another test's jobs.
		id := pgtype.UUID{Valid: true}
		id.Bytes[15] = byte(i + 1)
		fx.Insert(t, "channel_issue_comment_pending", testutil.Cols{
			"id": id, "workspace_id": icWorkspace, "issue_id": icIssue, "binding_id": bindingID,
			"installation_id": f.steward.ID, "author_user_id": icMember,
			"channel_message_id": messageID, "message": []byte(`{}`),
		})
	}
	first, err := f.store.ClaimChannelIssueComment(ctx, dbid.NewV7())
	if err != nil || first.ChannelMessageID != "om_order_image" {
		t.Fatalf("first claim = %+v, err=%v", first, err)
	}
	other, err := f.store.ClaimChannelIssueComment(ctx, dbid.NewV7())
	if err != nil || other.ChannelMessageID != "om_other_binding" {
		t.Fatalf("another binding must progress ahead of the reply: %+v, err=%v", other, err)
	}
	fx.Exec(t, `UPDATE channel_issue_comment_pending SET lease_expires_at = now() - interval '1 second' WHERE channel_message_id = $1`, first.ChannelMessageID)
	reclaimed, err := f.store.ClaimChannelIssueComment(ctx, dbid.NewV7())
	if err != nil || reclaimed.ChannelMessageID != first.ChannelMessageID || reclaimed.LeaseToken == first.LeaseToken {
		t.Fatalf("expired claim = %+v, err=%v", reclaimed, err)
	}
	count, err := f.store.FinishChannelIssueComment(ctx, db.FinishChannelIssueCommentParams{
		ChannelMessageID: first.ChannelMessageID, WorkspaceID: first.WorkspaceID, LeaseToken: first.LeaseToken,
	})
	if err != nil || count != 0 {
		t.Fatalf("stale worker acknowledged message: count=%d err=%v", count, err)
	}
	count, err = f.store.FinishChannelIssueComment(ctx, db.FinishChannelIssueCommentParams{
		ChannelMessageID: reclaimed.ChannelMessageID, WorkspaceID: reclaimed.WorkspaceID, LeaseToken: reclaimed.LeaseToken,
	})
	if err != nil || count != 1 {
		t.Fatalf("current worker acknowledgement: count=%d err=%v", count, err)
	}
	reply, err := f.store.ClaimChannelIssueComment(ctx, dbid.NewV7())
	if err != nil || reply.ChannelMessageID != "om_order_reply" {
		t.Fatalf("reply must follow its completed parent: %+v, err=%v", reply, err)
	}
}

type gatedIssueMedia struct {
	engine.MediaResolver
	started chan struct{}
	release chan struct{}
}

func (m *gatedIssueMedia) ResolveMedia(ctx context.Context, inst engine.ResolvedInstallation, sender engine.ResolvedIdentity, sessionID, messageID pgtype.UUID, msg channel.InboundMessage) channel.InboundMessage {
	close(m.started)
	select {
	case <-ctx.Done():
		return msg
	case <-m.release:
		return m.MediaResolver.ResolveMedia(ctx, inst, sender, sessionID, messageID, msg)
	}
}

func TestIssueChatInboundResolvesMediaBeforeWritingComment(t *testing.T) {
	for _, failImage := range []bool{false, true} {
		t.Run(map[bool]string{false: "images and files", true: "partial download failure"}[failImage], func(t *testing.T) {
			f := newIssueChatFixture(t)
			fx := testutil.New(f.pool, icWorkspace, icMember)
			fx.User(t, "Group member", "issue-media@example.test", testutil.Cols{"id": icMember})
			fx.Workspace(t, "Group attachments", "issue-group-attachments", testutil.Cols{"id": icWorkspace})
			fx.Member(t, icWorkspace, icMember, "member")
			fx.Issue(t, "Group attachments", testutil.Cols{"id": icIssue})
			fx.Cleanup(t, `DELETE FROM channel_media_pending_object WHERE workspace_id = $1`, icWorkspace)
			api := &fakeSender{downloadedByKey: map[string]DownloadedResource{
				"image": {Data: []byte("image bytes"), ContentType: "image/png"},
				"file":  {Data: []byte("document bytes"), ContentType: "application/pdf", Filename: "report.pdf"},
			}}
			if failImage {
				api.downloadErrByKey = map[string]error{"image": errors.New("download failed")}
			}
			storage := &fakeMediaStorage{}
			media := &gatedIssueMedia{
				MediaResolver: NewFeishuMediaResolver(api, fakeCreds{secret: "plain"}, storage, engine.NewDBMediaIntentLedger(f.store.Queries), newDiscardLogger()),
				started:       make(chan struct{}), release: make(chan struct{}),
			}
			writer := &recordingCommentWriter{id: uuidFromString(t, icNewComment)}
			r := NewFeishuIssueChatRouter(f.store, writer, nil, nil, newDiscardLogger())
			r.SetMediaResolver(media)
			lm := InboundMessage{MessageID: "om_media", MessageType: "post", Content: `{"content":[[{"tag":"at","user_id":"@_user_1"},{"tag":"img","image_key":"image"},{"tag":"file","file_key":"file","file_name":"report.pdf"}]]}`,
				Mentions: []InboundMention{{Key: "@_user_1", Name: "Coder", UnionID: icCoderBot}}}
			raw, err := json.Marshal(lm)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := r.processIssueComment(ctx, db.ChannelIssueCommentPending{
					WorkspaceID: f.binding.WorkspaceID, IssueID: f.binding.IssueID,
					BindingID: f.binding.ID, InstallationID: f.steward.ID, AuthorUserID: uuidFromString(t, icMember),
					ChannelMessageID: lm.MessageID, Message: raw,
				})
				done <- err
			}()
			select {
			case <-media.started:
			case err := <-done:
				t.Fatalf("finished before downloading: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			writer.mu.Lock()
			calls := len(writer.calls)
			writer.mu.Unlock()
			if calls != 0 {
				t.Fatal("comment written before attachments were ready")
			}
			close(media.release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if len(writer.calls) != 1 {
				t.Fatalf("writer calls = %d", len(writer.calls))
			}
			got := writer.calls[0]
			wantCount := 2
			if failImage {
				wantCount = 1
			}
			if len(got.Attachments) != wantCount || len(storage.uploads) != wantCount || len(api.downloadCalls) != 2 {
				t.Fatalf("attachments=%d uploads=%d downloads=%d", len(got.Attachments), len(storage.uploads), len(api.downloadCalls))
			}
			if !strings.HasPrefix(got.Content, "[@Coder](mention://agent/"+icCoderAgent+") ") || got.AuthorUserID != uuidFromString(t, icMember) {
				t.Fatalf("comment = %+v", got)
			}
			for _, attachment := range got.Attachments {
				if !strings.Contains(got.Content, "/api/attachments/"+uuidString(attachment.ID)+"/download") {
					t.Fatalf("attachment absent from body: %+v", got)
				}
			}
			if strings.Contains(got.Content, "unavailable") != failImage {
				t.Fatalf("failure marker: %q", got.Content)
			}
		})
	}
}

func TestIssueCommentMediaMarkdownStandalone(t *testing.T) {
	for _, typ := range []string{"image", "file", "audio", "media"} {
		t.Run(typ, func(t *testing.T) {
			lm := InboundMessage{MessageID: "om_attachment", MessageType: typ, Content: `{"image_key":"img","file_key":"file","file_name":"report.pdf"}`}
			inst := engine.ResolvedInstallation{ID: uuidFromString(t, icStewardAgt), WorkspaceID: uuidFromString(t, icWorkspace)}
			mediaID := uuidFromString(t, icNewComment)
			res := mediaResourcesFromMessage(lm)[0]
			got := issueCommentMediaMarkdown(lm, inst, mediaID, map[string]string{mediaObjectKey(inst, mediaID, res): "download-link"})
			if got != "download-link" {
				t.Fatalf("markdown = %q", got)
			}
			if got := issueCommentMediaMarkdown(lm, inst, mediaID, nil); !strings.Contains(got, "unavailable") {
				t.Fatalf("missing download failure = %q", got)
			}
		})
	}
}
