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
		for _, table := range []string{"channel_issue_comment_pending", "channel_issue_message", "channel_issue_chat", "channel_installation", "channel_user_binding"} {
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
	r := NewFeishuIssueChatRouter(f.store, &recordingCommentWriter{}, nil, nil, newDiscardLogger())
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
	r := NewFeishuIssueChatRouter(f.store, writer, nil, nil, newDiscardLogger())
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
	if len(writer.calls) != 0 {
		t.Fatal("the connector ACK path must only enqueue, not write comments")
	}
	var raw []byte
	if err := f.pool.QueryRow(ctx, `SELECT message FROM channel_issue_comment_pending WHERE channel_message_id = $1`, lm.MessageID).Scan(&raw); err != nil {
		t.Fatalf("load queued message: %v", err)
	}
	var queued InboundMessage
	if err := json.Unmarshal(raw, &queued); err != nil {
		t.Fatal(err)
	}
	got, err := r.renderComment(ctx, route.WorkspaceID, queued, flattenContent(queued.MessageType, queued.Content))
	want := "[@Coder](mention://agent/" + icCoderAgent + ") [@Ada](mention://member/" + icMember + ") please look"
	if err != nil || got != want {
		t.Fatalf("content = %q, err=%v, want %q", got, err, want)
	}
	parent, err := r.parentComment(ctx, route.IssueID, queued)
	if err != nil || uuidString(parent) != icParentCmt {
		t.Fatalf("parent = %s, err=%v", uuidString(parent), err)
	}
	if err := r.RecordIssueComment(ctx, inst, identity, route, msg); err != nil {
		t.Fatalf("replay: %v", err)
	}
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM channel_issue_comment_pending WHERE channel_message_id = $1`, lm.MessageID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("queued messages = %d, err=%v", count, err)
	}
}

// namedBotsAPI answers /bot/v3/info with a display name per app.
type namedBotsAPI struct {
	fakeAPIClient
	names map[string]string
	calls int
}

func (a *namedBotsAPI) GetBotInfo(_ context.Context, creds InstallationCredentials) (BotInfo, error) {
	a.calls++
	return BotInfo{Name: a.names[creds.AppID]}, nil
}

// A bot whose union_id could not be resolved at install is still recognised
// by its display name; a name two bots share resolves to neither.
func TestIssueChatRouter_MentionFallsBackToBotName(t *testing.T) {
	f := newIssueChatFixture(t)
	ctx := context.Background()
	const (
		plannerAgent = "1c0a0000-0000-4000-8000-0000000000b1"
		twinA        = "1c0a0000-0000-4000-8000-0000000000b2"
		twinB        = "1c0a0000-0000-4000-8000-0000000000b3"
	)
	f.insertInstallation(t, plannerAgent, "cli_ic_planner", "")
	f.insertInstallation(t, twinA, "cli_ic_twin_a", "")
	f.insertInstallation(t, twinB, "cli_ic_twin_b", "")
	api := &namedBotsAPI{names: map[string]string{
		"cli_ic_planner": "Planner", "cli_ic_twin_a": "Twin", "cli_ic_twin_b": "Twin",
	}}
	writer := &recordingCommentWriter{id: uuidFromString(t, icNewComment)}
	r := NewFeishuIssueChatRouter(f.store, writer, api, fakeCredentials{secret: "shh"}, newDiscardLogger())

	lm := InboundMessage{
		AppID: icStewardApp, ChatID: icChatID, ChatType: ChatTypeGroup,
		MessageID: "om_inbound_name", MessageType: "text",
		Content:    `{"text":"@_user_1 and @_user_2 go"}`,
		SenderType: "user",
		Mentions: []InboundMention{
			{Key: "@_user_1", Name: "Planner", UnionID: "on_unknown_planner"},
			{Key: "@_user_2", Name: "Twin", UnionID: "on_unknown_twin"},
		},
	}
	got, err := r.renderComment(ctx, f.binding.WorkspaceID, lm, flattenContent(lm.MessageType, lm.Content))
	want := "[@Planner](mention://agent/" + plannerAgent + ") and @Twin go"
	if err != nil || got != want {
		t.Fatalf("content = %q, err=%v, want %q", got, err, want)
	}
	if api.calls != 3 {
		t.Fatalf("bot info calls = %d, want one per bot without a union_id", api.calls)
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

// unescapeCardJSON decodes a card payload so assertions can read its
// Markdown as sent, independent of JSON escaping.
func unescapeCardJSON(t *testing.T, card string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(card), &v); err != nil {
		t.Fatalf("card is not JSON: %v", err)
	}
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		case string:
			b.WriteString(x)
			b.WriteString("\n")
		}
	}
	walk(v)
	return b.String()
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

	t.Run("a member mention notifies the member when the sending bot knows them", func(t *testing.T) {
		mention := "[@Ada](mention://member/" + icMember + ") please confirm"
		api := &fakeAPIClient{sendReturn: "om_out_mention_steward"}
		bus := events.New()
		newSyncService(f, api).Register(bus)
		bus.Publish(commentCreated("1c0a0000-0000-4000-8000-0000000000b1", "member", icInstaller, mention, nil))
		if len(api.sent) != 1 {
			t.Fatalf("sent = %+v", api.sent)
		}
		card := unescapeCardJSON(t, api.sent[0].CardJSON)
		if !strings.Contains(card, "<at id=ou_ic_member></at> please confirm") {
			t.Fatalf("steward card must mention the bound member natively: %s", card)
		}
		if strings.Contains(card, "处理") {
			t.Fatalf("a member's comment must not be flagged as a request: %s", card)
		}

		// The coder bot has no binding for the member, so its open_id space
		// cannot address them: the name stays visible without a dead mention.
		api = &fakeAPIClient{sendReturn: "om_out_mention_coder"}
		bus = events.New()
		newSyncService(f, api).Register(bus)
		bus.Publish(commentCreated("1c0a0000-0000-4000-8000-0000000000b2", "agent", icCoderAgent, mention, nil))
		if len(api.sent) != 1 || api.sent[0].InstallationID.AppID != icCoderApp {
			t.Fatalf("sent = %+v", api.sent)
		}
		card = unescapeCardJSON(t, api.sent[0].CardJSON)
		if !strings.Contains(card, "**@Ada** please confirm") || strings.Contains(card, "<at") {
			t.Fatalf("coder card must fall back to the bold name: %s", card)
		}
		if strings.Contains(card, "待 Ada 处理") {
			t.Fatalf("an ordinary mention must not become an assistance request: %s", card)
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
