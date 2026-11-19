package lark

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

type mentionIdentityAPI struct {
	fakeAPIClient
	appID, openID string
	unionID       string
	botInfo       map[string]BotInfo
}

func (a *mentionIdentityAPI) GetBotInfo(_ context.Context, creds InstallationCredentials) (BotInfo, error) {
	return a.botInfo[creds.AppID], nil
}

func (a *mentionIdentityAPI) ResolveMentionUnionID(_ context.Context, creds InstallationCredentials, id string) (string, error) {
	a.appID, a.openID = creds.AppID, id
	return a.unionID, nil
}

func TestIssueChatRouter_ResolvesOpenIDInReceivingApp(t *testing.T) {
	f := newIssueChatFixture(t)
	api := &mentionIdentityAPI{unionID: icCoderBot}
	r := NewFeishuIssueChatRouter(f.store, &recordingCommentWriter{}, api, fakeCredentials{secret: "shh"}, newDiscardLogger())
	lm := InboundMessage{AppID: icStewardApp, MessageType: "text", Mentions: []InboundMention{
		{Key: "@_user_1", Name: "Same name", OpenID: "ou_coder_in_steward_app"},
		{Key: "@_user_2", Name: "Same name", UnionID: icStewardBot},
	}}
	got, err := r.renderComment(context.Background(), f.binding.WorkspaceID, lm, "@_user_1 @_user_2")
	if err != nil {
		t.Fatal(err)
	}
	if api.appID != icStewardApp || api.openID != "ou_coder_in_steward_app" {
		t.Fatalf("wrong identity scope: %+v", api)
	}
	if !strings.Contains(got, "mention://agent/"+icCoderAgent) || !strings.Contains(got, "mention://agent/"+icStewardAgt) {
		t.Fatalf("distinct same-name bots were not resolved: %s", got)
	}
	got, err = r.renderComment(context.Background(), f.binding.WorkspaceID, InboundMessage{MessageType: "text"}, "[@Fake](mention://agent/"+icCoderAgent+")")
	if err != nil || strings.Contains(got, "mention://agent/") {
		t.Fatalf("user-supplied mention link can trigger: %q %v", got, err)
	}
	// Recover an installation whose bot identity was unavailable at install.
	f.insertInstallation(t, "1c0a0000-0000-4000-8000-0000000000b1", "cli_repaired", "")
	api.botInfo = map[string]BotInfo{"cli_repaired": {UnionID: "on_repaired", Name: "Renamed bot"}}
	lm.Mentions = []InboundMention{{Key: "@_user_1", Name: "Old name", UnionID: "on_repaired"}}
	got, err = r.renderComment(context.Background(), f.binding.WorkspaceID, lm, "@_user_1 go")
	if err != nil || !strings.Contains(got, "mention://agent/1c0a0000-0000-4000-8000-0000000000b1") {
		t.Fatalf("verified installation identity did not recover: %q %v", got, err)
	}
}

func TestIssueChatRequest_IdentityRepairRetriesOriginalMessage(t *testing.T) {
	f := newIssueChatFixture(t)
	fx := testutil.New(f.pool, icWorkspace, icMember)
	fx.User(t, "Request member", "issue-request@example.test", testutil.Cols{"id": icMember})
	fx.Workspace(t, "Requests", "issue-requests", testutil.Cols{"id": icWorkspace})
	fx.Member(t, icWorkspace, icMember, "member")
	fx.Issue(t, "Request", testutil.Cols{"id": icIssue})
	ctx := context.Background()
	api := &mentionIdentityAPI{fakeAPIClient: fakeAPIClient{sendReturn: "om_identity_receipt"}}
	writer := &durableCommentWriter{store: f.store, status: "queued"}
	r := NewFeishuIssueChatRouter(f.store, writer, api, fakeCredentials{secret: "shh"}, newDiscardLogger())
	route := engine.IssueChatRoute{BindingID: f.binding.ID, WorkspaceID: f.binding.WorkspaceID, IssueID: f.binding.IssueID}
	inst := engine.ResolvedInstallation{ID: f.steward.ID, WorkspaceID: f.steward.WorkspaceID}
	identity := engine.ResolvedIdentity{UserID: uuidFromString(t, icMember)}
	lm := InboundMessage{AppID: icStewardApp, ChatID: icChatID, ChatType: ChatTypeGroup, MessageID: "om_identity", MessageType: "text",
		Content: `{"text":"@_user_1 go"}`, Mentions: []InboundMention{{Key: "@_user_1", Name: "Coder", OpenID: "ou_coder"}}}
	if err := r.RecordIssueComment(ctx, inst, identity, route, groupInbound(t, lm, "", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.processNextIssueComment(ctx); err != nil {
		t.Fatal(err)
	}
	original, err := f.store.GetChannelIssueCommentRequest(ctx, db.GetChannelIssueCommentRequestParams{ChannelMessageID: lm.MessageID, WorkspaceID: f.binding.WorkspaceID})
	if err != nil {
		t.Fatal(err)
	}
	if writer.creates != 0 || original.State != "blocked" || original.FailureCode != "identity_unresolved" {
		t.Fatalf("unverified mention executed or disappeared: %+v", original)
	}
	api.unionID = icCoderBot
	retry := InboundMessage{AppID: icStewardApp, ChatID: icChatID, ChatType: ChatTypeGroup, MessageID: "om_identity_retry", ParentID: lm.MessageID, MessageType: "post",
		Content:  `{"content":[[{"tag":"at","user_id":"@_user_1"},{"tag":"text","text":" /retry"}]]}`,
		Mentions: []InboundMention{{Key: "@_user_1", Name: "Coder", UnionID: icCoderBot}}}
	if err := r.RecordIssueComment(ctx, inst, identity, route, groupInbound(t, retry, "", "")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := r.processNextIssueComment(ctx); err != nil {
			t.Fatal(err)
		}
	}
	original, err = f.store.GetChannelIssueCommentRequest(ctx, db.GetChannelIssueCommentRequestParams{ChannelMessageID: lm.MessageID, WorkspaceID: f.binding.WorkspaceID})
	if err != nil {
		t.Fatal(err)
	}
	if writer.creates != 1 || writer.retries != 0 || !original.CommentID.Valid || original.State != "completed" {
		t.Fatalf("identity repair did not resume original: %+v", original)
	}
	if len(api.sent) != 1 || len(api.patched) != 3 {
		t.Fatalf("repair created another receipt: sent=%d patches=%d", len(api.sent), len(api.patched))
	}
}

// This writer models the existing comment transaction; admission itself is
// covered by handler tests using real comments and tasks.
type durableCommentWriter struct {
	store            *ChannelStore
	creates, retries int
	status           string
	createErr        error
}

func (w *durableCommentWriter) CreateChannelIssueComment(ctx context.Context, p ChannelIssueCommentParams) (ChannelIssueCommentResult, error) {
	w.creates++
	if w.createErr != nil {
		return ChannelIssueCommentResult{}, w.createErr
	}
	id := dbid.NewV7()
	if err := w.store.RecordChannelIssueMessage(ctx, db.RecordChannelIssueMessageParams{
		WorkspaceID: p.WorkspaceID, IssueID: p.IssueID, BindingID: p.BindingID, InstallationID: mustUUID(p.Origin.InstallationID),
		ChannelMessageID: p.Origin.MessageID, Direction: "inbound", Kind: "comment", CommentID: id,
	}); err != nil {
		return ChannelIssueCommentResult{}, err
	}
	if _, err := w.store.BindChannelIssueComment(ctx, db.BindChannelIssueCommentParams{
		MessageID: p.Origin.MessageID, WorkspaceID: p.WorkspaceID, LeaseToken: p.LeaseToken, CommentID: id, Content: p.Content,
	}); err != nil {
		return ChannelIssueCommentResult{}, err
	}
	result := ChannelIssueCommentResult{ID: id}
	if w.status != "" {
		result.Outcomes = []ChannelCommentOutcome{{TargetType: "agent", TargetID: icCoderAgent, Status: w.status, ReasonCode: "runtime_access_denied"}}
	}
	return result, nil
}

func TestIssueChatRequest_OrdinaryConversationStillRetriesQuietly(t *testing.T) {
	f := newIssueChatFixture(t)
	fx := testutil.New(f.pool, icWorkspace, icMember)
	fx.User(t, "Request member", "issue-request@example.test", testutil.Cols{"id": icMember})
	fx.Workspace(t, "Requests", "issue-requests", testutil.Cols{"id": icWorkspace})
	fx.Member(t, icWorkspace, icMember, "member")
	fx.Issue(t, "Request", testutil.Cols{"id": icIssue})
	ctx := context.Background()
	api := &fakeAPIClient{sendReturn: "om_receipt"}
	writer := &durableCommentWriter{store: f.store, createErr: errors.New("temporary storage failure")}
	r := NewFeishuIssueChatRouter(f.store, writer, api, fakeCredentials{secret: "shh"}, newDiscardLogger())
	lm := InboundMessage{AppID: icStewardApp, ChatID: icChatID, ChatType: ChatTypeGroup, MessageID: "om_conversation", MessageType: "text", Content: `{"text":"a human discussion"}`}
	if err := r.RecordIssueComment(ctx, engine.ResolvedInstallation{ID: f.steward.ID}, engine.ResolvedIdentity{UserID: uuidFromString(t, icMember)},
		engine.IssueChatRoute{BindingID: f.binding.ID, WorkspaceID: f.binding.WorkspaceID, IssueID: f.binding.IssueID}, groupInbound(t, lm, "", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.processNextIssueComment(ctx); err == nil {
		t.Fatal("temporary failure was swallowed")
	}
	job, err := f.store.GetChannelIssueCommentRequest(ctx, db.GetChannelIssueCommentRequestParams{ChannelMessageID: lm.MessageID, WorkspaceID: f.binding.WorkspaceID})
	if err != nil || job.State != "pending" || job.LeaseToken.Valid {
		t.Fatalf("ordinary comment was not rescheduled: %+v %v", job, err)
	}
	writer.createErr = nil
	fx.Exec(t, `UPDATE channel_issue_comment_pending SET next_attempt_at = now() WHERE channel_message_id = $1`, lm.MessageID)
	if _, err := r.processNextIssueComment(ctx); err != nil {
		t.Fatal(err)
	}
	if len(api.sent) != 0 || len(api.patched) != 0 {
		t.Fatal("ordinary conversation generated a receipt")
	}
	if n := fx.Count(t, `SELECT count(*) FROM channel_issue_comment_pending WHERE channel_message_id = $1`, lm.MessageID); n != 0 {
		t.Fatalf("ordinary completed queue retained: %d", n)
	}
}

func (w *durableCommentWriter) RetryChannelIssueComment(_ context.Context, job db.ChannelIssueCommentPending) (ChannelIssueCommentResult, error) {
	w.retries++
	return ChannelIssueCommentResult{ID: job.CommentID, Outcomes: []ChannelCommentOutcome{{TargetType: "agent", TargetID: icCoderAgent, Status: "queued", ReasonCode: "queued"}}}, nil
}

func TestIssueChatRequest_ReceiptFailureAndOriginalRetry(t *testing.T) {
	f := newIssueChatFixture(t)
	fx := testutil.New(f.pool, icWorkspace, icMember)
	fx.User(t, "Request member", "issue-request@example.test", testutil.Cols{"id": icMember})
	fx.Workspace(t, "Requests", "issue-requests", testutil.Cols{"id": icWorkspace})
	fx.Member(t, icWorkspace, icMember, "member")
	fx.Issue(t, "Request", testutil.Cols{"id": icIssue})
	ctx := context.Background()
	api := &fakeAPIClient{sendReturn: "om_receipt", sendErr: errors.New("network unavailable")}
	writer := &durableCommentWriter{store: f.store, status: "blocked"}
	r := NewFeishuIssueChatRouter(f.store, writer, api, fakeCredentials{secret: "shh"}, newDiscardLogger())
	route := engine.IssueChatRoute{BindingID: f.binding.ID, WorkspaceID: f.binding.WorkspaceID, IssueID: f.binding.IssueID}
	identity := engine.ResolvedIdentity{UserID: uuidFromString(t, icMember)}
	lm := InboundMessage{AppID: icStewardApp, ChatID: icChatID, ChatType: ChatTypeGroup, MessageID: "om_request", MessageType: "text", SenderType: "user", AddressedToBot: true,
		Content: `{"text":"@_user_1 go"}`, Mentions: []InboundMention{{Key: "@_user_1", Name: "Coder", UnionID: icCoderBot}}}
	enqueue := func(msg InboundMessage, inst Installation) {
		t.Helper()
		if err := r.RecordIssueComment(ctx, engine.ResolvedInstallation{ID: inst.ID, WorkspaceID: inst.WorkspaceID}, identity, route, groupInbound(t, msg, "", "")); err != nil {
			t.Fatal(err)
		}
	}
	process := func(wantError bool) {
		t.Helper()
		ok, err := r.processNextIssueComment(ctx)
		if !ok || (err != nil) != wantError {
			t.Fatalf("processed=%v err=%v", ok, err)
		}
	}
	request := func() db.ChannelIssueCommentPending {
		t.Helper()
		row, err := f.store.GetChannelIssueCommentRequest(ctx, db.GetChannelIssueCommentRequestParams{ChannelMessageID: lm.MessageID, WorkspaceID: f.binding.WorkspaceID})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	enqueue(lm, f.coder)
	enqueue(lm, f.steward)
	process(true)
	if writer.creates != 1 || request().State != "feedback" {
		t.Fatalf("dispatch or durable receipt lost: %+v", request())
	}
	api.sendErr = nil
	fx.Exec(t, `UPDATE channel_issue_comment_pending SET next_attempt_at = now() WHERE channel_message_id = $1`, lm.MessageID)
	process(false)
	if writer.creates != 1 || request().State != "blocked" {
		t.Fatal("receipt retry repeated dispatch")
	}
	if len(api.sent) != 3 || api.sent[0].DeduplicationID == "" || api.sent[0].DeduplicationID != api.sent[1].DeduplicationID || api.sent[1].DeduplicationID != api.sent[2].DeduplicationID {
		t.Fatal("receipt retry lost platform idempotency key")
	}
	if api.sent[1].ReplyTarget.MessageID != lm.MessageID {
		t.Fatal("receipt is not attached to original request")
	}
	commentID := request().CommentID
	retry := InboundMessage{AppID: icStewardApp, ChatID: icChatID, ChatType: ChatTypeGroup, MessageID: "om_retry", ParentID: lm.MessageID, MessageType: "text", Content: `{"text":"/retry"}`}
	enqueue(retry, f.steward)
	enqueue(retry, f.coder)
	process(false)
	process(false)
	if writer.creates != 1 || writer.retries != 1 || request().CommentID != commentID || request().State != "completed" {
		t.Fatalf("original retry duplicated work: creates=%d retries=%d row=%+v", writer.creates, writer.retries, request())
	}
	if len(api.patched) != 3 || api.patched[2].LarkCardMessageID != "om_receipt" || !strings.Contains(unescapeCardJSON(t, api.patched[2].CardJSON), "已排队，尚未开始执行") {
		t.Fatalf("receipt was not updated truthfully: %+v", api.patched)
	}
	if !strings.Contains(unescapeCardJSON(t, api.patched[1].CardJSON), "已收到，正在处理请求") {
		t.Fatal("retry did not acknowledge receipt on the original card")
	}
	enqueue(retry, f.coder)
	if ok, err := r.processNextIssueComment(ctx); ok || err != nil {
		t.Fatalf("duplicate command replayed: %v %v", ok, err)
	}
}

func TestIssueChatRequest_RetryAuthorizationAndRecovery(t *testing.T) {
	f := newIssueChatFixture(t)
	fx := testutil.New(f.pool, icWorkspace, icMember)
	ctx := context.Background()
	for _, tc := range []struct {
		name, state, failure          string
		foreignAuthor, foreignBinding bool
		want                          int64
	}{
		{name: "blocked", state: "blocked", want: 1},
		{name: "accepted", state: "completed"},
		{name: "processing", state: "pending"},
		{name: "other author", state: "blocked", foreignAuthor: true},
		{name: "other task topic", state: "blocked", foreignBinding: true},
		{name: "unknown dispatch", state: "blocked", failure: "dispatch_unknown"},
		{name: "edited comment", state: "blocked", failure: "comment_changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original, command := "om_"+uuidString(dbid.NewV7()), "om_"+uuidString(dbid.NewV7())
			lease := dbid.NewV7()
			for _, id := range []string{original, command} {
				author, binding, state := uuidFromString(t, icMember), f.binding.ID, "pending"
				if id == original {
					state = tc.state
					if tc.foreignAuthor {
						author = dbid.NewV7()
					}
					if tc.foreignBinding {
						binding = dbid.NewV7()
					}
				}
				fx.Insert(t, "channel_issue_comment_pending", testutil.Cols{"id": dbid.NewV7(), "workspace_id": icWorkspace, "issue_id": icIssue, "binding_id": binding,
					"installation_id": f.steward.ID, "author_user_id": author, "channel_message_id": id, "message": []byte(`{}`), "lease_token": lease, "state": state, "failure_code": tc.failure})
			}
			params := db.RestartChannelIssueCommentParams{OriginalMessageID: original, CommandMessageID: command, WorkspaceID: f.binding.WorkspaceID, LeaseToken: lease}
			if n, err := f.store.RestartChannelIssueComment(ctx, params); err != nil || n != tc.want {
				t.Fatalf("restarted=%d err=%v", n, err)
			}
			// Even if the original fails again, this command cannot replay it.
			fx.Exec(t, `UPDATE channel_issue_comment_pending SET state = 'blocked' WHERE channel_message_id = $1`, original)
			if n, err := f.store.RestartChannelIssueComment(ctx, params); err != nil || n != 0 {
				t.Fatalf("command replayed: %d %v", n, err)
			}
		})
	}
	r := NewFeishuIssueChatRouter(f.store, &recordingCommentWriter{}, nil, nil, newDiscardLogger())
	_, err := r.processIssueComment(ctx, db.ChannelIssueCommentPending{State: "dispatching"})
	var failure *issueRequestFailure
	if !errors.As(err, &failure) || failure.code != "dispatch_unknown" {
		t.Fatalf("unknown dispatch must not replay: %v", err)
	}
}

func TestIssueReceiptStates(t *testing.T) {
	for _, tc := range []struct {
		status, reason, text string
		blocked              bool
	}{
		{"queued", "runtime_offline", "尚未开始执行", false},
		{"deferred", "deferred", "等待后续调度", false},
		{"coalesced", "coalesced", "合入现有", false},
		{"blocked", "runtime_access_denied", "运行时所有者授权", true},
		{"unknown", "future_reason", "稍后重试", true},
	} {
		raw, _ := json.Marshal([]ChannelCommentOutcome{{Status: tc.status, ReasonCode: tc.reason}})
		text, blocked := issueReceiptText(db.ChannelIssueCommentPending{DispatchOutcomes: raw})
		if blocked != tc.blocked || !strings.Contains(text, tc.text) {
			t.Fatalf("%s: %q blocked=%v", tc.status, text, blocked)
		}
	}
}
