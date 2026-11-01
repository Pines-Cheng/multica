package lark

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	icWorkspace   = "1c0a0000-0000-4000-8000-000000000001"
	icIssue       = "1c0a0000-0000-4000-8000-000000000002"
	icStewardAgt  = "1c0a0000-0000-4000-8000-000000000003"
	icCoderAgent  = "1c0a0000-0000-4000-8000-000000000004"
	icMember      = "1c0a0000-0000-4000-8000-000000000005"
	icParentCmt   = "1c0a0000-0000-4000-8000-000000000006"
	icNewComment  = "1c0a0000-0000-4000-8000-000000000007"
	icInstaller   = "1c0a0000-0000-4000-8000-000000000008"
	icChatID      = "oc_issue_chat_test"
	icStewardApp  = "cli_ic_steward"
	icCoderApp    = "cli_ic_coder"
	icStewardBot  = "on_ic_steward_bot"
	icCoderBot    = "on_ic_coder_bot"
	icMemberUnion = "on_ic_member"
)

type issueChatFixture struct {
	pool    *pgxpool.Pool
	store   *ChannelStore
	steward Installation
	coder   Installation
	binding db.ChannelIssueChat
}

func newIssueChatFixture(t *testing.T) *issueChatFixture {
	t.Helper()
	pool := channelScopeTestDB(t)
	ctx := context.Background()
	clean := func() {
		for _, table := range []string{"channel_issue_message", "channel_issue_chat", "channel_installation", "channel_user_binding"} {
			_, _ = pool.Exec(context.Background(), "DELETE FROM "+table+" WHERE workspace_id = $1", icWorkspace)
		}
	}
	clean()
	t.Cleanup(clean)
	f := &issueChatFixture{pool: pool, store: NewChannelStore(db.New(pool))}
	f.steward = f.insertInstallation(t, icStewardAgt, icStewardApp, icStewardBot)
	f.coder = f.insertInstallation(t, icCoderAgent, icCoderApp, icCoderBot)
	if _, err := pool.Exec(ctx, `
		INSERT INTO channel_user_binding (workspace_id, multica_user_id, installation_id, channel_type, channel_user_id, config)
		VALUES ($1, $2, $3, 'feishu', 'ou_ic_member', $4)`,
		icWorkspace, icMember, f.steward.ID, `{"union_id":"`+icMemberUnion+`"}`); err != nil {
		t.Fatalf("insert binding: %v", err)
	}
	binding, err := f.store.CreateChannelIssueChat(ctx, db.CreateChannelIssueChatParams{
		WorkspaceID:           uuidFromString(t, icWorkspace),
		IssueID:               uuidFromString(t, icIssue),
		RootIssueID:           uuidFromString(t, icIssue),
		ChannelType:           channelTypeFeishu,
		ChatID:                icChatID,
		StewardInstallationID: f.steward.ID,
		CreatedBy:             uuidFromString(t, icInstaller),
	})
	if err != nil {
		t.Fatalf("create binding: %v", err)
	}
	f.binding = binding
	return f
}

func (f *issueChatFixture) insertInstallation(t *testing.T, agentID, appID, botUnion string) Installation {
	t.Helper()
	cfg, _ := json.Marshal(map[string]string{
		"app_id":               appID,
		"app_secret_encrypted": base64.StdEncoding.EncodeToString([]byte("sealed")),
		"bot_union_id":         botUnion,
		"region":               "feishu",
	})
	var row db.ChannelInstallation
	err := f.pool.QueryRow(context.Background(), `
		INSERT INTO channel_installation (workspace_id, agent_id, channel_type, config, installer_user_id)
		VALUES ($1, $2, 'feishu', $3, $4)
		RETURNING id`, icWorkspace, agentID, cfg, icInstaller).Scan(&row.ID)
	if err != nil {
		t.Fatalf("insert installation: %v", err)
	}
	inst, err := f.store.GetLarkInstallation(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("load installation: %v", err)
	}
	return inst
}

type recordingCommentWriter struct {
	mu    sync.Mutex
	calls []ChannelIssueCommentParams
	id    pgtype.UUID
}

func (w *recordingCommentWriter) CreateChannelIssueComment(_ context.Context, p ChannelIssueCommentParams) (pgtype.UUID, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, p)
	return w.id, nil
}

func groupInbound(t *testing.T, lm InboundMessage, threadID, rootID string) channel.InboundMessage {
	t.Helper()
	msg := channelMessageFromLark(lm)
	msg.Source.ThreadID = threadID
	if rootID != "" {
		msg.ReplyTo = &channel.ReplyCtx{RootID: rootID}
	}
	return msg
}

func TestIssueChatRouter_Lookup(t *testing.T) {
	f := newIssueChatFixture(t)
	r := NewFeishuIssueChatRouter(f.store, &recordingCommentWriter{}, newDiscardLogger())
	ctx := context.Background()

	for _, tc := range []struct {
		name         string
		chat, thread string
		root         string
		wantBound    bool
	}{
		{"chat level", icChatID, "", "", true},
		{"topic under an ordinary message falls back to the chat's issue", icChatID, "omt_x", "om_unmapped", true},
		{"other chat", "oc_other", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := groupInbound(t, InboundMessage{ChatID: ChatID(tc.chat), ChatType: ChatTypeGroup, MessageID: "om_q"}, tc.thread, tc.root)
			route, bound, err := r.LookupIssueChat(ctx, msg)
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if bound != tc.wantBound {
				t.Fatalf("bound = %v, want %v", bound, tc.wantBound)
			}
			if bound && (route.IssueID != f.binding.IssueID || route.StewardInstallationID != f.steward.ID) {
				t.Fatalf("route = %+v", route)
			}
		})
	}
}

// A native @ of a bot becomes an agent mention (so it triggers the agent like
// a web comment would), a bound person a member mention, and a quote reply to
// a mirrored comment lands under that comment.
func TestIssueChatRouter_RecordComment(t *testing.T) {
	f := newIssueChatFixture(t)
	ctx := context.Background()
	writer := &recordingCommentWriter{id: uuidFromString(t, icNewComment)}
	r := NewFeishuIssueChatRouter(f.store, writer, newDiscardLogger())
	if err := f.store.RecordChannelIssueMessage(ctx, db.RecordChannelIssueMessageParams{
		WorkspaceID: f.binding.WorkspaceID, IssueID: f.binding.IssueID, BindingID: f.binding.ID,
		InstallationID: f.steward.ID, ChannelMessageID: "om_parent_card",
		Direction: issueMessageOutbound, Kind: issueMessageKindComment,
		CommentID: uuidFromString(t, icParentCmt),
	}); err != nil {
		t.Fatalf("seed parent mirror: %v", err)
	}

	lm := InboundMessage{
		AppID: icStewardApp, ChatID: icChatID, ChatType: ChatTypeGroup,
		MessageID: "om_inbound_1", MessageType: "text",
		Content:  `{"text":"@_user_1 @_user_2 please look"}`,
		ParentID: "om_parent_card", SenderType: "user",
		Mentions: []InboundMention{
			{Key: "@_user_1", Name: "Coder", UnionID: icCoderBot},
			{Key: "@_user_2", Name: "Ada", UnionID: icMemberUnion},
		},
	}
	msg := groupInbound(t, lm, "", "")
	route := engine.IssueChatRoute{BindingID: f.binding.ID, WorkspaceID: f.binding.WorkspaceID, IssueID: f.binding.IssueID, StewardInstallationID: f.steward.ID}
	inst := engine.ResolvedInstallation{ID: f.steward.ID, WorkspaceID: f.steward.WorkspaceID}
	identity := engine.ResolvedIdentity{UserID: uuidFromString(t, icMember)}

	if err := r.RecordIssueComment(ctx, inst, identity, route, msg); err != nil {
		t.Fatalf("record: %v", err)
	}
	if len(writer.calls) != 1 {
		t.Fatalf("writer calls = %d, want 1", len(writer.calls))
	}
	got := writer.calls[0]
	want := "[@Coder](mention://agent/" + icCoderAgent + ") [@Ada](mention://member/" + icMember + ") please look"
	if got.Content != want {
		t.Fatalf("content = %q\nwant      %q", got.Content, want)
	}
	if uuidString(got.ParentCommentID) != icParentCmt {
		t.Fatalf("parent = %s, want %s", uuidString(got.ParentCommentID), icParentCmt)
	}
	if got.Origin.MessageID != "om_inbound_1" || got.AuthorUserID != identity.UserID {
		t.Fatalf("params = %+v", got)
	}
	mapped, err := f.store.GetChannelIssueMessageByChannelID(ctx, "om_inbound_1")
	if err != nil || uuidString(mapped.CommentID) != icNewComment || mapped.Direction != issueMessageInbound {
		t.Fatalf("mapping = %+v err=%v", mapped, err)
	}

	// A replay of the same event must not write a second comment.
	if err := r.RecordIssueComment(ctx, inst, identity, route, msg); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(writer.calls) != 1 {
		t.Fatalf("replay wrote another comment (calls = %d)", len(writer.calls))
	}
}

func newSyncService(f *issueChatFixture, api *fakeAPIClient) *IssueChatService {
	return NewIssueChatService(IssueChatServiceConfig{
		Store:       f.store,
		Credentials: fakeCredentials{secret: "shh"},
		API:         api,
		Logger:      newDiscardLogger(),
		Synchronous: true,
	})
}

func commentCreated(id, authorType, authorID, content string, extra map[string]any) events.Event {
	payload := map[string]any{"comment": map[string]any{
		"id": id, "issue_id": icIssue, "author_type": authorType, "author_id": authorID,
		"content": content, "type": "comment",
	}}
	for k, v := range extra {
		payload[k] = v
	}
	return events.Event{Type: protocol.EventCommentCreated, WorkspaceID: icWorkspace, Payload: payload}
}

func TestIssueChatService_MirrorsComments(t *testing.T) {
	f := newIssueChatFixture(t)
	ctx := context.Background()
	bus := events.New()

	t.Run("an agent speaks through its own bot", func(t *testing.T) {
		api := &fakeAPIClient{sendReturn: "om_out_agent"}
		newSyncService(f, api).Register(bus)
		bus.Publish(commentCreated(icNewComment, "agent", icCoderAgent, "done: see PR", nil))
		if len(api.sent) != 1 {
			t.Fatalf("sent = %d, want 1", len(api.sent))
		}
		sent := api.sent[0]
		if sent.InstallationID.AppID != icCoderApp || string(sent.ChatID) != icChatID {
			t.Fatalf("sent via %q to %q", sent.InstallationID.AppID, sent.ChatID)
		}
		if strings.Contains(sent.CardJSON, `"header"`) {
			t.Fatalf("own-bot card must not carry a relay header: %s", sent.CardJSON)
		}
		rows, err := f.store.ListChannelIssueMessagesByComment(ctx, uuidFromString(t, icNewComment))
		if err != nil || len(rows) != 1 || rows[0].ChannelMessageID != "om_out_agent" {
			t.Fatalf("mirror rows = %+v err=%v", rows, err)
		}

		// Editing the comment patches the same card.
		bus = events.New()
		newSyncService(f, api).Register(bus)
		bus.Publish(events.Event{Type: protocol.EventCommentUpdated, WorkspaceID: icWorkspace, Payload: map[string]any{
			"comment": map[string]any{"id": icNewComment, "issue_id": icIssue, "author_type": "agent", "author_id": icCoderAgent, "content": "edited", "type": "comment"},
		}})
		if len(api.patched) != 1 || api.patched[0].LarkCardMessageID != "om_out_agent" || !strings.Contains(api.patched[0].CardJSON, "edited") {
			t.Fatalf("patched = %+v", api.patched)
		}
	})

	t.Run("a member's web comment is relayed by the steward", func(t *testing.T) {
		api := &fakeAPIClient{sendReturn: "om_out_member"}
		bus := events.New()
		newSyncService(f, api).Register(bus)
		bus.Publish(commentCreated(icParentCmt, "member", icMember, "hello", nil))
		if len(api.sent) != 1 || api.sent[0].InstallationID.AppID != icStewardApp {
			t.Fatalf("sent = %+v", api.sent)
		}
		if !strings.Contains(api.sent[0].CardJSON, `"header"`) {
			t.Fatalf("relayed card must name its author: %s", api.sent[0].CardJSON)
		}
	})

	t.Run("a comment that came from the chat is not mirrored back", func(t *testing.T) {
		api := &fakeAPIClient{sendReturn: "om_out_echo"}
		bus := events.New()
		newSyncService(f, api).Register(bus)
		bus.Publish(commentCreated("1c0a0000-0000-4000-8000-0000000000aa", "member", icMember, "from chat",
			map[string]any{CommentChannelOriginKey: CommentChannelOrigin{ChannelType: "feishu", MessageID: "om_in"}}))
		if len(api.sent) != 0 {
			t.Fatalf("echoed a chat message back: %+v", api.sent)
		}
	})
}
