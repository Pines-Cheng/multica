package lark

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// fakeChats records chat management calls.
type fakeChats struct {
	mu         sync.Mutex
	created    []CreateChatParams
	added      []AddChatMembersParams
	tabs       []string
	syncedTabs []struct {
		chatID ChatID
		appID  string
		tabs   []ChatURLTab
	}
	tabSyncErr error
	pinned     []string
}

func (f *fakeChats) CreateChat(_ context.Context, _ InstallationCredentials, p CreateChatParams) (ChatID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, p)
	return ChatID("oc_auto_" + p.UUID), nil
}

func (f *fakeChats) AddChatURLTab(_ context.Context, _ InstallationCredentials, _ ChatID, name, url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tabs = append(f.tabs, name+"="+url)
	return nil
}

func (f *fakeChats) SyncChatURLTabs(_ context.Context, creds InstallationCredentials, chatID ChatID, tabs []ChatURLTab) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncedTabs = append(f.syncedTabs, struct {
		chatID ChatID
		appID  string
		tabs   []ChatURLTab
	}{chatID: chatID, appID: creds.AppID, tabs: append([]ChatURLTab(nil), tabs...)})
	return f.tabSyncErr
}

func TestIssueChatTabs_IsolatesRootArtifactsAndKeepsCardsOnFailure(t *testing.T) {
	f := newActivityFixture(t)
	f.svc.cfg.AppURL = "https://multica.example"
	root := f.fx.Issue(t, "Root", testutil.Cols{
		"assignee_type": "squad", "assignee_id": f.squad,
		"metadata": `{"PRD":"https://docs.example/root","note":"https://other.example"}`,
	})
	f.chats.tabSyncErr = errors.New("tab permission denied")
	f.publishIssue(protocol.EventIssueCreated, root, nil)
	binding, ok := f.binding(t, root)
	if !ok || len(f.chats.syncedTabs) == 0 || len(f.chats.pinned) != 1 {
		t.Fatal("tab failure must not prevent chat binding or the pinned card")
	}
	assertTabs := func(chatID string, url string) {
		t.Helper()
		last := f.chats.syncedTabs[len(f.chats.syncedTabs)-1]
		if string(last.chatID) != chatID || !reflect.DeepEqual(last.tabs, []ChatURLTab{{Name: "PRD", URL: url}}) {
			t.Fatalf("wrong task's artifacts: %+v", last)
		}
	}
	assertTabs(binding.ChatID, "https://docs.example/root")

	child := f.fx.Issue(t, "Child", testutil.Cols{
		"parent_issue_id": root,
		"metadata":        `{"PRD":"https://docs.example/child"}`,
	})
	f.publishIssue(protocol.EventIssueCreated, child, nil)
	f.bus.Publish(events.Event{Type: protocol.EventIssueMetadataChanged, WorkspaceID: f.ws, Payload: map[string]any{"issue_id": child}})
	assertTabs(binding.ChatID, "https://docs.example/root")
	if card := f.progressCard(t, root); !strings.Contains(card, "[PRD](https://docs.example/root)") || !strings.Contains(card, "查看任务") {
		t.Fatalf("tab failure lost card information or navigation: %s", card)
	}

	f.chats.tabSyncErr = nil
	f.fx.Exec(t, `UPDATE issue SET metadata = '{"PRD":"https://docs.example/revised"}' WHERE id = $1`, root)
	f.bus.Publish(events.Event{Type: protocol.EventIssueMetadataChanged, WorkspaceID: f.ws, Payload: map[string]any{"issue_id": root}})
	assertTabs(binding.ChatID, "https://docs.example/revised")

	other := f.fx.Issue(t, "Independent", testutil.Cols{
		"assignee_type": "squad", "assignee_id": f.squad,
		"metadata": `{"PRD":"https://docs.example/independent"}`,
	})
	f.publishIssue(protocol.EventIssueCreated, other, nil)
	otherBinding, ok := f.binding(t, other)
	if !ok || otherBinding.ChatID == binding.ChatID {
		t.Fatal("independent tasks must have independent groups")
	}
	assertTabs(otherBinding.ChatID, "https://docs.example/independent")

	newAgent := f.fx.Agent(t, "New tab steward", f.fx.Runtime(t, "new-tab-runtime"))
	newSteward := f.installBot(t, newAgent, "cli_tabs_new_steward")
	f.fx.Exec(t, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, root, newAgent)
	f.publishIssue(protocol.EventIssueUpdated, root, map[string]any{"assignee_changed": true})
	assertTabs(binding.ChatID, "https://docs.example/revised")
	if got := f.chats.syncedTabs[len(f.chats.syncedTabs)-1].appID; got != newSteward.AppID {
		t.Fatalf("tab sync app = %s, want current steward %s", got, newSteward.AppID)
	}
}

func (f *fakeChats) PinTopNotice(_ context.Context, _ InstallationCredentials, _ ChatID, messageID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pinned = append(f.pinned, messageID)
	return nil
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
	n   int
	ids []string
}

func (a *seqAPI) SendInteractiveCard(ctx context.Context, p SendCardParams) (string, error) {
	a.mu.Lock()
	a.n++
	id := "om_seq_" + uuid.NewString()
	a.ids = append(a.ids, id)
	a.mu.Unlock()
	_, _ = a.fakeAPIClient.SendInteractiveCard(ctx, p)
	return id, nil
}

func (a *seqAPI) lastSentID(t *testing.T) string {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.ids) == 0 {
		t.Fatal("no card was sent")
	}
	return a.ids[len(a.ids)-1]
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
	svc     *IssueChatService
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
	f.svc = NewIssueChatService(IssueChatServiceConfig{
		Store: f.store, Credentials: fakeCredentials{secret: "shh"}, API: f.api, Chats: f.chats,
		Logger: newDiscardLogger(), Synchronous: true,
	})
	f.svc.Register(f.bus)
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

	// A status change updates the root overview and topic card instead of
	// posting a notice.
	sentBefore := len(f.api.sent)
	f.fx.Exec(t, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, child)
	f.publishIssue(protocol.EventIssueUpdated, child, map[string]any{"status_changed": true, "prev_status": "todo"})
	if len(f.api.sent) != sentBefore {
		t.Fatalf("a status change posted a notice: %+v", f.api.sent[sentBefore:])
	}
	progress := f.progressCard(t, parent)
	if !strings.Contains(progress, "Child · 进行中 · Leader") {
		t.Fatalf("progress card does not show the sub-issue's state: %s", progress)
	}

	f.fx.Exec(t, `UPDATE issue SET metadata = '{"Meego":"https://meego.example/story/1","note":"not-a-document-link"}' WHERE id = $1`, parent)
	f.bus.Publish(events.Event{Type: protocol.EventIssueMetadataChanged, WorkspaceID: f.ws, Payload: map[string]any{"issue_id": parent}})
	progress = f.progressCard(t, parent)
	if !strings.Contains(progress, "[Meego](https://meego.example/story/1)") || strings.Contains(progress, "not-a-document-link") {
		t.Fatalf("progress card links = %s", progress)
	}
}

// progressCard returns the latest render of a root chat's progress card and
// checks it is pinned.
func (f *activityFixture) progressCard(t *testing.T, rootIssue string) string {
	t.Helper()
	binding, ok := f.binding(t, rootIssue)
	if !ok {
		t.Fatal("root chat missing")
	}
	intro, err := f.store.GetChannelIssueIntroMessage(context.Background(), binding.ID)
	if err != nil {
		t.Fatalf("intro card: %v", err)
	}
	pinned := false
	for _, id := range f.chats.pinned {
		pinned = pinned || id == intro.ChannelMessageID
	}
	if !pinned {
		t.Fatalf("progress card %s not pinned (pinned %v)", intro.ChannelMessageID, f.chats.pinned)
	}
	for i := len(f.api.patched) - 1; i >= 0; i-- {
		if f.api.patched[i].LarkCardMessageID == intro.ChannelMessageID {
			return unescapeCardJSON(t, f.api.patched[i].CardJSON)
		}
	}
	t.Fatal("progress card was never refreshed")
	return ""
}

func TestIssueChatCards_RefreshesTopicWithItsOriginalSender(t *testing.T) {
	f := newActivityFixture(t)
	f.svc.cfg.AppURL = "https://multica.example"
	parent := f.fx.Issue(t, "Parent", testutil.Cols{"assignee_type": "squad", "assignee_id": f.squad})
	f.publishIssue(protocol.EventIssueCreated, parent, nil)
	child := f.fx.Issue(t, "开发：Parent", testutil.Cols{"parent_issue_id": parent, "assignee_type": "agent", "assignee_id": f.leader})
	f.publishIssue(protocol.EventIssueCreated, child, nil)
	topic, ok := f.binding(t, child)
	if !ok {
		t.Fatal("child topic missing")
	}

	other := f.fx.Agent(t, "New steward", f.fx.Runtime(t, "new-steward-runtime"))
	f.installBot(t, other, "cli_card_new_steward")
	f.fx.Exec(t, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, parent, other)
	f.publishIssue(protocol.EventIssueUpdated, parent, map[string]any{"assignee_changed": true})
	sentBefore := len(f.api.sent)

	for _, status := range []string{"in_progress", "done", "cancelled"} {
		f.fx.Exec(t, `UPDATE issue SET status = $2, assignee_type = 'member', assignee_id = $3 WHERE id = $1`, child, status, f.user)
		f.publishIssue(protocol.EventIssueUpdated, child, map[string]any{"status_changed": true, "assignee_changed": true})
		var latest PatchCardParams
		for _, patch := range f.api.patched {
			if patch.LarkCardMessageID == topic.ThreadRootMessageID {
				latest = patch
			}
		}
		if latest.LarkCardMessageID == "" {
			t.Fatal("status change did not refresh the child topic card")
		}
		card := unescapeCardJSON(t, latest.CardJSON)
		if latest.InstallationID.AppID != f.steward.AppID || !strings.Contains(card, statusLabel(status)) || !strings.Contains(card, "Chat Owner") {
			t.Fatalf("topic update must show %s and its assignee through the original bot: %+v card=%s", status, latest, card)
		}
		if progress := f.progressCard(t, parent); !strings.Contains(progress, statusLabel(status)) {
			t.Fatalf("parent progress does not reflect child status %s: %s", status, progress)
		}
	}
	if len(f.api.sent) != sentBefore {
		t.Fatal("refreshing topic cards must not post new messages")
	}
}

func TestIssueChatCards_RefreshesTopicTitleAndLinks(t *testing.T) {
	f := newActivityFixture(t)
	f.svc.cfg.AppURL = "https://multica.example"
	parent := f.fx.Issue(t, "Parent", testutil.Cols{"assignee_type": "squad", "assignee_id": f.squad})
	f.publishIssue(protocol.EventIssueCreated, parent, nil)
	child := f.fx.Issue(t, "开发：Parent", testutil.Cols{"parent_issue_id": parent})
	f.publishIssue(protocol.EventIssueCreated, child, nil)
	topic, ok := f.binding(t, child)
	if !ok {
		t.Fatal("child topic missing")
	}
	sentBefore := len(f.api.sent)
	f.fx.Exec(t, `UPDATE issue SET title = '自测：Parent' WHERE id = $1`, child)
	f.publishIssue(protocol.EventIssueUpdated, child, map[string]any{"title_changed": true})
	f.fx.Exec(t, `UPDATE issue SET metadata = '{"自测报告":"https://tests.example/report","note":"internal"}' WHERE id = $1`, child)
	f.bus.Publish(events.Event{Type: protocol.EventIssueMetadataChanged, WorkspaceID: f.ws, Payload: map[string]any{"issue_id": child}})
	var latest PatchCardParams
	for _, patch := range f.api.patched {
		if patch.LarkCardMessageID == topic.ThreadRootMessageID {
			latest = patch
		}
	}
	card := decodeIssueCard(t, latest.CardJSON)
	if card.Header.Title.Content != "自测：Parent" || !strings.Contains(latest.CardJSON, "https://tests.example/report") || strings.Contains(latest.CardJSON, "internal") {
		t.Fatalf("topic content did not refresh from the issue: %s", latest.CardJSON)
	}
	if !strings.Contains(f.progressCard(t, parent), "自测") {
		t.Fatal("parent overview did not pick up the renamed child")
	}
	if len(f.api.sent) != sentBefore {
		t.Fatal("title and metadata updates must not post new messages")
	}
}

func TestIssueChatCards_KeepsStandaloneIdentityAndPendingOwner(t *testing.T) {
	f := newActivityFixture(t)
	f.svc.cfg.AppURL = "https://multica.example"
	parent := f.fx.Issue(t, "项目空间添加搜索功能", testutil.Cols{"assignee_type": "squad", "assignee_id": f.squad})
	f.publishIssue(protocol.EventIssueCreated, parent, nil)
	const title = "验收：项目空间添加搜索功能"
	child := f.fx.Issue(t, title, testutil.Cols{"parent_issue_id": parent, "assignee_type": "member", "assignee_id": f.user})
	f.publishIssue(protocol.EventIssueCreated, child, nil)
	topic, ok := f.binding(t, child)
	if !ok {
		t.Fatal("child topic missing")
	}
	rootIssue, err := f.store.GetIssue(context.Background(), uuidFromString(t, parent))
	if err != nil {
		t.Fatal(err)
	}
	childIssue, err := f.store.GetIssue(context.Background(), uuidFromString(t, child))
	if err != nil {
		t.Fatal(err)
	}
	rootIdentifier, _ := f.svc.issueIdentity(context.Background(), rootIssue)
	childIdentifier, _ := f.svc.issueIdentity(context.Background(), childIssue)
	var card decodedIssueCard
	for _, patch := range f.api.patched {
		if patch.LarkCardMessageID == topic.ThreadRootMessageID {
			card = decodeIssueCard(t, patch.CardJSON)
		}
	}
	if card.Header.Title.Content != title || !strings.Contains(card.Header.Subtitle.Content, rootIdentifier+" › "+childIdentifier) {
		t.Fatalf("topic card must keep its full title and parent identity when viewed alone: %+v", card.Header)
	}
	if progress := f.progressCard(t, parent); !strings.Contains(progress, "待 Chat Owner 处理") {
		t.Fatalf("overview must make the pending member action visible: %s", progress)
	}
	grandchild := f.fx.Issue(t, "回归：项目空间添加搜索功能", testutil.Cols{"parent_issue_id": child})
	f.publishIssue(protocol.EventIssueCreated, grandchild, nil)
	grandTopic, ok := f.binding(t, grandchild)
	if !ok {
		t.Fatal("nested topic missing")
	}
	grandIssue, err := f.store.GetIssue(context.Background(), uuidFromString(t, grandchild))
	if err != nil {
		t.Fatal(err)
	}
	grandIdentifier, _ := f.svc.issueIdentity(context.Background(), grandIssue)
	for _, patch := range f.api.patched {
		if patch.LarkCardMessageID == grandTopic.ThreadRootMessageID {
			card = decodeIssueCard(t, patch.CardJSON)
		}
	}
	if !strings.Contains(card.Header.Subtitle.Content, rootIdentifier+" › "+childIdentifier+" › "+grandIdentifier) {
		t.Fatalf("nested topic must keep its immediate parent as well as the root: %+v", card.Header)
	}
	f.fx.Exec(t, `UPDATE issue SET status = 'done' WHERE id = $1`, child)
	f.publishIssue(protocol.EventIssueUpdated, child, map[string]any{"status_changed": true})
	f.fx.Exec(t, `UPDATE issue SET status = 'in_review' WHERE id = $1`, parent)
	f.publishIssue(protocol.EventIssueUpdated, parent, map[string]any{"status_changed": true})
	if progress := f.progressCard(t, parent); !strings.Contains(progress, "当前：待验收") {
		t.Fatalf("delivery after the children finish must retain the review prompt: %s", progress)
	}
}

func TestIssueChatCards_DeliveryAndAttentionOnlyLinkToExistingPages(t *testing.T) {
	f := newActivityFixture(t)
	f.svc.cfg.AppURL = "https://multica.example"
	issueID := f.fx.Issue(t, "Ready to review", testutil.Cols{
		"status": "in_review", "assignee_type": "squad", "assignee_id": f.squad,
		"metadata": `{"体验地址":"https://preview.example/search","自测报告":"https://tests.example/report"}`,
	})
	f.publishIssue(protocol.EventIssueCreated, issueID, nil)
	issue, err := f.store.GetIssue(context.Background(), uuidFromString(t, issueID))
	if err != nil {
		t.Fatal(err)
	}
	_, issueURL := f.svc.issueIdentity(context.Background(), issue)
	cardJSON := f.api.patched[len(f.api.patched)-1].CardJSON
	delivery := decodeIssueCard(t, cardJSON)
	if delivery.Header.Template != "orange" || !strings.Contains(cardJSON, "https://preview.example/search") || !strings.Contains(cardJSON, "https://tests.example/report") {
		t.Fatalf("delivery card must show saved status and links: %s", cardJSON)
	}
	for _, element := range delivery.Body.Elements {
		if element.Tag == "button" && (element.Text.Content != "查看交付" || len(element.Behaviors) != 1 || element.Behaviors[0].Type != "open_url" || element.Behaviors[0].DefaultURL != issueURL) {
			t.Fatalf("delivery action must open the existing issue: %+v", element)
		}
	}
	commentID := f.fx.Insert(t, "comment", testutil.Cols{
		"issue_id": issueID, "workspace_id": f.ws, "author_type": "agent", "author_id": f.leader,
		"content": "[@Owner](mention://member/" + f.user + ") please review", "type": "comment",
	})
	f.bus.Publish(events.Event{Type: protocol.EventCommentCreated, WorkspaceID: f.ws, Payload: map[string]any{"comment": map[string]any{
		"id": commentID, "issue_id": issueID, "author_type": "agent", "author_id": f.leader,
		"content": "[@Owner](mention://member/" + f.user + ") please review", "type": "comment",
	}}})
	attention := decodeIssueCard(t, f.api.sent[len(f.api.sent)-1].CardJSON)
	buttons := 0
	for _, element := range attention.Body.Elements {
		if element.Tag == "button" {
			buttons++
			if element.Text.Content != "查看讨论" || len(element.Behaviors) != 1 || element.Behaviors[0].Type != "open_url" || element.Behaviors[0].DefaultURL != issueURL+"#comment-"+commentID {
				t.Fatalf("attention action must open the original discussion: %+v", element)
			}
		}
	}
	if buttons != 1 || attention.Header.Template != "orange" {
		t.Fatalf("attention card missing its navigation action: %+v", attention)
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
	if progress := f.progressCard(t, issue); !strings.Contains(progress, " · Other") {
		t.Fatalf("progress card does not show the new assignee: %s", progress)
	}
}

// A run on an issue with a chat is one live card from the agent's own bot,
// settled in place when the run ends.
func TestIssueChatP2_RunIsOneLiveCard(t *testing.T) {
	f := newActivityFixture(t)
	issue := f.fx.Issue(t, "Run card", testutil.Cols{"assignee_type": "squad", "assignee_id": f.squad})
	f.publishIssue(protocol.EventIssueCreated, issue, nil)
	task := uuid.NewString()
	run := func(eventType string, extra map[string]any) {
		payload := map[string]any{"task_id": task, "agent_id": f.leader, "issue_id": issue}
		for k, v := range extra {
			payload[k] = v
		}
		f.bus.Publish(events.Event{Type: eventType, WorkspaceID: f.ws, TaskID: task, Payload: payload})
	}
	sent := len(f.api.sent)
	run(protocol.EventTaskRunning, nil)
	if len(f.api.sent) != sent {
		t.Fatal("a run that has only just started must not post a card yet")
	}
	f.svc.runs.mu.Lock()
	f.svc.runs.state[util.MustParseUUID(task)].started = time.Now().Add(-runCardDelay)
	f.svc.runs.mu.Unlock()
	f.bus.Publish(events.Event{Type: protocol.EventTaskMessage, WorkspaceID: f.ws, TaskID: task, Payload: protocol.TaskMessagePayload{
		TaskID: task, Type: "tool_use", Tool: "Bash", Input: map[string]any{"command": "go test ./secret"},
	}})
	if len(f.api.sent) != sent+1 || f.api.sent[sent].InstallationID.AppID != "cli_p1_leader" {
		t.Fatalf("running card = %+v", f.api.sent[sent:])
	}
	card := f.api.sent[sent].CardJSON
	if !strings.Contains(card, "运行命令") || strings.Contains(card, "go test") || strings.Contains(card, `"header"`) {
		t.Fatalf("running card shows the kind of work only, without a header: %s", card)
	}
	run(protocol.EventTaskCompleted, nil)
	if len(f.api.sent) != sent+1 {
		t.Fatal("a run must not post a second card")
	}
	if len(f.api.patched) == 0 || !strings.Contains(f.api.patched[len(f.api.patched)-1].CardJSON, "已完成") {
		t.Fatalf("completion did not settle the card: %+v", f.api.patched)
	}
}

// The comment a run writes replaces its live card, so the chat shows the run
// as one message, and nothing the run does afterwards brings a card back.
func TestIssueChatP2_RunCommentTakesOverTheRunCard(t *testing.T) {
	f := newActivityFixture(t)
	issue := f.fx.Issue(t, "Takeover", testutil.Cols{"assignee_type": "squad", "assignee_id": f.squad})
	f.publishIssue(protocol.EventIssueCreated, issue, nil)
	task := uuid.NewString()
	run := func(eventType string, payload any) {
		f.bus.Publish(events.Event{Type: eventType, WorkspaceID: f.ws, TaskID: task, Payload: payload})
	}
	ids := map[string]any{"task_id": task, "agent_id": f.leader, "issue_id": issue}
	run(protocol.EventTaskRunning, ids)
	f.svc.runs.mu.Lock()
	f.svc.runs.state[util.MustParseUUID(task)].started = time.Now().Add(-runCardDelay)
	f.svc.runs.mu.Unlock()
	run(protocol.EventTaskMessage, protocol.TaskMessagePayload{TaskID: task, Type: "tool_use", Tool: "Bash"})
	sent := len(f.api.sent)
	runCard := f.api.lastSentID(t)

	comment := f.fx.Insert(t, "comment", testutil.Cols{
		"issue_id": issue, "workspace_id": f.ws, "author_type": "agent", "author_id": f.leader,
		"content": "已交付，进入代码评审", "type": "comment", "source_task_id": task,
	})
	f.bus.Publish(events.Event{Type: protocol.EventCommentCreated, WorkspaceID: f.ws, Payload: map[string]any{"comment": map[string]any{
		"id": comment, "issue_id": issue, "author_type": "agent", "author_id": f.leader,
		"content": "已交付，进入代码评审", "type": "comment", "source_task_id": task,
	}}})
	if len(f.api.sent) != sent {
		t.Fatalf("the comment must reuse the run card, not post another: %+v", f.api.sent[sent:])
	}
	last := f.api.patched[len(f.api.patched)-1]
	if last.LarkCardMessageID != runCard || !strings.Contains(last.CardJSON, "已交付，进入代码评审") {
		t.Fatalf("run card was not turned into the comment: %+v", last)
	}

	patched := len(f.api.patched)
	f.svc.runs.mu.Lock()
	f.svc.runs.state[util.MustParseUUID(task)].patched = time.Time{}
	f.svc.runs.mu.Unlock()
	run(protocol.EventTaskMessage, protocol.TaskMessagePayload{TaskID: task, Type: "tool_use", Tool: "Bash"})
	run(protocol.EventTaskCompleted, ids)
	if len(f.api.sent) != sent || len(f.api.patched) != patched {
		t.Fatalf("the run touched the chat after its comment: sent %+v patched %+v", f.api.sent[sent:], f.api.patched[patched:])
	}
}

func TestIssueChatP2_ShortRunPostsNoCard(t *testing.T) {
	f := newActivityFixture(t)
	issue := f.fx.Issue(t, "Short run", testutil.Cols{"assignee_type": "squad", "assignee_id": f.squad})
	f.publishIssue(protocol.EventIssueCreated, issue, nil)
	task := uuid.NewString()
	sent := len(f.api.sent)
	for _, eventType := range []string{protocol.EventTaskRunning, protocol.EventTaskCompleted} {
		f.bus.Publish(events.Event{Type: eventType, WorkspaceID: f.ws, TaskID: task, Payload: map[string]any{"task_id": task, "agent_id": f.leader, "issue_id": issue}})
	}
	if len(f.api.sent) != sent {
		t.Fatalf("a short run posted a card: %+v", f.api.sent[sent:])
	}
}
