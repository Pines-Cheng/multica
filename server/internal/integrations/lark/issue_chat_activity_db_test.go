package lark

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// fakeChats records chat management calls.
type fakeChats struct {
	mu      sync.Mutex
	created []CreateChatParams
	added   []AddChatMembersParams
}

func (f *fakeChats) CreateChat(_ context.Context, _ InstallationCredentials, p CreateChatParams) (ChatID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, p)
	return ChatID("oc_auto_" + p.UUID), nil
}

func (f *fakeChats) AddChatMembers(_ context.Context, _ InstallationCredentials, p AddChatMembersParams) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, p)
	return nil, nil
}

// seqAPI hands out a fresh message id per send, so every mirror is mapped.
type seqAPI struct {
	fakeAPIClient
	n int
}

func (a *seqAPI) SendInteractiveCard(ctx context.Context, p SendCardParams) (string, error) {
	a.mu.Lock()
	a.n++
	id := "om_seq_" + uuid.NewString()
	a.mu.Unlock()
	_, _ = a.fakeAPIClient.SendInteractiveCard(ctx, p)
	return id, nil
}

type activityFixture struct {
	fx      *testutil.Fixture
	store   *ChannelStore
	ws      string
	user    string
	leader  string
	squad   string
	steward Installation
	api     *seqAPI
	chats   *fakeChats
	bus     *events.Bus
}

func newActivityFixture(t *testing.T) *activityFixture {
	t.Helper()
	pool := channelScopeTestDB(t)
	fx := testutil.New(pool, "", "")
	user := fx.User(t, "Chat Owner", "chat-owner-"+uuid.NewString()+"@multica.test")
	ws := fx.Workspace(t, "Issue chat P1", "issue-chat-"+uuid.NewString()[:8])
	fx.Member(t, ws, user, "owner")
	fx = testutil.New(pool, ws, user)
	runtime := fx.Runtime(t, "chat-runtime")
	leader := fx.Agent(t, "Leader", runtime)
	squad := fx.Squad(t, "Chat Squad", leader)
	f := &activityFixture{fx: fx, store: NewChannelStore(db.New(pool)), ws: ws, user: user, leader: leader, squad: squad,
		api: &seqAPI{}, chats: &fakeChats{}, bus: events.New()}
	t.Cleanup(func() {
		for _, table := range []string{"channel_issue_message", "channel_issue_chat", "channel_installation", "channel_user_binding"} {
			_, _ = pool.Exec(context.Background(), "DELETE FROM "+table+" WHERE workspace_id = $1", ws)
		}
	})
	f.steward = f.installBot(t, leader, "cli_p1_leader")
	NewIssueChatService(IssueChatServiceConfig{
		Store: f.store, Credentials: fakeCredentials{secret: "shh"}, API: f.api, Chats: f.chats,
		Logger: newDiscardLogger(), Synchronous: true,
	}).Register(f.bus)
	return f
}

func (f *activityFixture) installBot(t *testing.T, agentID, appID string) Installation {
	t.Helper()
	id := f.fx.Insert(t, "channel_installation", testutil.Cols{
		"workspace_id": f.ws, "agent_id": agentID, "channel_type": "feishu",
		"config":            `{"app_id":"` + appID + `","app_secret_encrypted":"c2VhbGVk","region":"feishu"}`,
		"installer_user_id": f.user,
	})
	inst, err := f.store.GetLarkInstallation(context.Background(), uuidFromString(t, id))
	if err != nil {
		t.Fatalf("load installation: %v", err)
	}
	return inst
}

func (f *activityFixture) publishIssue(eventType, issueID string, extra map[string]any) {
	payload := map[string]any{"issue": map[string]any{"id": issueID}}
	for k, v := range extra {
		payload[k] = v
	}
	f.bus.Publish(events.Event{Type: eventType, WorkspaceID: f.ws, ActorType: "member", ActorID: f.user, Payload: payload})
}

func (f *activityFixture) binding(t *testing.T, issueID string) (db.ChannelIssueChat, bool) {
	t.Helper()
	row, err := f.store.GetChannelIssueChatByIssue(context.Background(), db.GetChannelIssueChatByIssueParams{
		IssueID: uuidFromString(t, issueID), ChannelType: channelTypeFeishu,
	})
	return row, err == nil
}

func TestIssueChatP1_SquadAssignmentOpensChatUnlessSwitchedOff(t *testing.T) {
	f := newActivityFixture(t)
	issue := f.fx.Issue(t, "Auto chat", testutil.Cols{"assignee_type": "squad", "assignee_id": f.squad})
	f.publishIssue(protocol.EventIssueCreated, issue, nil)
	if len(f.chats.created) != 1 {
		t.Fatalf("chats created = %d, want 1", len(f.chats.created))
	}
	if _, ok := f.binding(t, issue); !ok {
		t.Fatal("auto-created chat was not bound")
	}

	f.fx.Exec(t, `UPDATE squad SET auto_create_channel_chat = false WHERE id = $1`, f.squad)
	off := f.fx.Issue(t, "No auto chat", testutil.Cols{"assignee_type": "squad", "assignee_id": f.squad})
	f.publishIssue(protocol.EventIssueCreated, off, nil)
	if len(f.chats.created) != 1 {
		t.Fatalf("a squad with the switch off must not open chats (created = %d)", len(f.chats.created))
	}
}

func TestIssueChatP1_SubIssueBecomesTopicAndThreadsItsTimeline(t *testing.T) {
	f := newActivityFixture(t)
	parent := f.fx.Issue(t, "Parent", testutil.Cols{"assignee_type": "squad", "assignee_id": f.squad})
	f.publishIssue(protocol.EventIssueCreated, parent, nil)
	parentChat, ok := f.binding(t, parent)
	if !ok {
		t.Fatal("parent chat missing")
	}
	child := f.fx.Issue(t, "Child", testutil.Cols{"parent_issue_id": parent, "assignee_type": "agent", "assignee_id": f.leader})
	f.publishIssue(protocol.EventIssueCreated, child, nil)
	topic, ok := f.binding(t, child)
	if !ok || topic.ChatID != parentChat.ChatID || topic.ThreadRootMessageID == "" || uuidString(topic.RootIssueID) != parent {
		t.Fatalf("topic binding = %+v ok=%v", topic, ok)
	}
	if len(f.chats.created) != 1 {
		t.Fatal("a sub-issue must not open a chat of its own")
	}

	sentBefore := len(f.api.sent)
	f.publishIssue(protocol.EventIssueUpdated, child, map[string]any{"status_changed": true, "prev_status": "todo"})
	f.fx.Exec(t, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, child)
	f.publishIssue(protocol.EventIssueUpdated, child, map[string]any{"status_changed": true, "prev_status": "todo"})
	last := f.api.sent[len(f.api.sent)-1]
	if len(f.api.sent) <= sentBefore || !strings.Contains(last.CardJSON, "进行中") {
		t.Fatalf("no status notice: %+v", f.api.sent[sentBefore:])
	}
	if last.ReplyTarget != (ReplyTarget{MessageID: topic.ThreadRootMessageID, InThread: true}) {
		t.Fatalf("notice not threaded into the topic: %+v", last.ReplyTarget)
	}
}

func TestIssueChatP1_ReassignmentHandsTheChatToTheNewSteward(t *testing.T) {
	f := newActivityFixture(t)
	issue := f.fx.Issue(t, "Handover", testutil.Cols{"assignee_type": "squad", "assignee_id": f.squad})
	f.publishIssue(protocol.EventIssueCreated, issue, nil)
	other := f.fx.Agent(t, "Other", f.fx.Runtime(t, "other-runtime"))
	otherBot := f.installBot(t, other, "cli_p1_other")

	f.fx.Exec(t, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, issue, other)
	f.publishIssue(protocol.EventIssueUpdated, issue, map[string]any{"assignee_changed": true})

	row, _ := f.binding(t, issue)
	if row.StewardInstallationID != otherBot.ID {
		t.Fatalf("steward = %s, want %s", uuidString(row.StewardInstallationID), uuidString(otherBot.ID))
	}
	invited := false
	for _, a := range f.chats.added {
		if a.MemberIDType == MemberIDTypeAppID && len(a.IDs) == 1 && a.IDs[0] == "cli_p1_other" {
			invited = true
		}
	}
	if !invited {
		t.Fatalf("the new steward's bot was not invited: %+v", f.chats.added)
	}
	last := f.api.sent[len(f.api.sent)-1]
	if last.InstallationID.AppID != "cli_p1_other" || !strings.Contains(last.CardJSON, "Other") {
		t.Fatalf("assignee notice = %+v", last)
	}
}
