package lark

// Issue <-> Feishu group sync: chat creation and the outbound mirror of the
// comment timeline (docs/lark-issue-group-sync.md).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

var (
	// ErrIssueChatNoSteward: neither the issue's squad leader nor its agent
	// assignee has an active Feishu bot to host the chat.
	ErrIssueChatNoSteward = errors.New("lark: no connected feishu bot can host this issue's chat")
	// ErrIssueChatUnavailable: the deployment cannot manage Feishu chats.
	ErrIssueChatUnavailable = errors.New("lark: feishu chat management unavailable")
)

// maxBotsPerChat is Lark's cap on bots in one group, the steward included.
const maxBotsPerChat = 15

const (
	issueChatSyncTimeout  = 15 * time.Second
	issueChatSyncAttempts = 3
	maxMirroredRunes      = 8000
)

// IssueChatServiceConfig wires the service. Chats may be nil when the API
// client cannot manage chats; EnsureChat then reports ErrIssueChatUnavailable.
type IssueChatServiceConfig struct {
	Store       *ChannelStore
	Credentials CredentialsResolver
	API         APIClient
	Chats       ChatAdmin
	AppURL      string
	Logger      *slog.Logger
	// Synchronous runs outbound sync inline instead of on a goroutine and
	// without retry backoff. Tests only.
	Synchronous bool
}

// IssueChatService creates an issue's Feishu chat and mirrors the issue's
// comments into it.
type IssueChatService struct {
	cfg   IssueChatServiceConfig
	locks sync.Map
}

func NewIssueChatService(cfg IssueChatServiceConfig) *IssueChatService {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &IssueChatService{cfg: cfg}
}

// IssueChat is the public view of a binding.
type IssueChat struct {
	ChatID    string
	AppLink   string
	CreatedAt time.Time
}

// EnsureIssueChatParams names the issue and the member opening its chat.
type EnsureIssueChatParams struct {
	WorkspaceID pgtype.UUID
	IssueID     pgtype.UUID
	ActorUserID pgtype.UUID
}

// lock serializes work on one key inside this process. Cross-replica races
// on chat creation are absorbed by Lark's create uuid and the binding's
// unique index.
func (s *IssueChatService) lock(key string) func() {
	m, _ := s.locks.LoadOrStore(key, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// GetChat returns the issue's chat, ok=false when none is bound.
func (s *IssueChatService) GetChat(ctx context.Context, issueID pgtype.UUID) (IssueChat, bool, error) {
	row, err := s.cfg.Store.GetChannelIssueChatByIssue(ctx, db.GetChannelIssueChatByIssueParams{
		IssueID: issueID, ChannelType: channelTypeFeishu,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return IssueChat{}, false, nil
	}
	if err != nil {
		return IssueChat{}, false, err
	}
	return s.chatView(ctx, row), true, nil
}

func (s *IssueChatService) chatView(ctx context.Context, row db.ChannelIssueChat) IssueChat {
	region := RegionFeishu
	if inst, err := s.cfg.Store.GetLarkInstallation(ctx, row.StewardInstallationID); err == nil {
		region = RegionOrDefault(inst.Region)
	}
	return IssueChat{
		ChatID:    row.ChatID,
		AppLink:   chatAppLink(region, row.ChatID),
		CreatedAt: row.CreatedAt.Time,
	}
}

func chatAppLink(region Region, chatID string) string {
	host := "https://applink.feishu.cn"
	if region == RegionLark {
		host = "https://applink.larksuite.com"
	}
	return host + "/client/chat/open?openChatId=" + chatID
}

// CanHost reports whether the issue has a steward bot, so the UI can offer
// chat creation only where it would succeed.
func (s *IssueChatService) CanHost(ctx context.Context, issue db.Issue) (bool, error) {
	if s.cfg.Chats == nil {
		return false, nil
	}
	_, err := s.steward(ctx, issue)
	if errors.Is(err, ErrIssueChatNoSteward) {
		return false, nil
	}
	return err == nil, err
}

// EnsureChat returns the issue's chat, creating it on first call. Creation
// invites the issue's participants: the squad's (or assignee agent's) bots,
// the squad's human members, the issue creator and the actor, who becomes
// the chat owner with the steward bot as manager.
func (s *IssueChatService) EnsureChat(ctx context.Context, p EnsureIssueChatParams) (IssueChat, error) {
	if s.cfg.Chats == nil {
		return IssueChat{}, ErrIssueChatUnavailable
	}
	defer s.lock("ensure:" + uuidString(p.IssueID))()
	if chat, ok, err := s.GetChat(ctx, p.IssueID); err != nil || ok {
		return chat, err
	}
	issue, err := s.cfg.Store.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: p.IssueID, WorkspaceID: p.WorkspaceID})
	if err != nil {
		return IssueChat{}, err
	}
	plan, err := s.planChat(ctx, issue, p.ActorUserID)
	if err != nil {
		return IssueChat{}, err
	}
	creds, err := installationCredentialsFor(plan.steward, s.cfg.Credentials)
	if err != nil {
		return IssueChat{}, err
	}
	ws, err := s.cfg.Store.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return IssueChat{}, err
	}
	identifier := service.IssueIdentifier(ws.IssuePrefix, issue.Number)
	issueLink := channel.IssueWebLink(s.cfg.AppURL, ws.Slug, identifier)

	createBots := plan.botAppIDs
	if len(createBots) > maxBotsPerChatRequest {
		createBots = createBots[:maxBotsPerChatRequest]
	}
	chatID, err := s.cfg.Chats.CreateChat(ctx, creds, CreateChatParams{
		Name:        issueChatName(identifier, issue.Title),
		Description: issueLink,
		UserIDType:  plan.createIDType,
		UserIDs:     plan.createUsers,
		OwnerID:     plan.ownerID,
		BotAppIDs:   createBots,
		UUID:        uuidString(issue.ID),
	})
	if err != nil {
		return IssueChat{}, err
	}
	for rest := plan.botAppIDs[len(createBots):]; len(rest) > 0; {
		n := min(len(rest), maxBotsPerChatRequest)
		s.addMembers(ctx, creds, chatID, MemberIDTypeAppID, rest[:n])
		rest = rest[n:]
	}
	s.addMembers(ctx, creds, chatID, plan.extraIDType, plan.extraUsers)

	row, err := s.cfg.Store.CreateChannelIssueChat(ctx, db.CreateChannelIssueChatParams{
		WorkspaceID:           issue.WorkspaceID,
		IssueID:               issue.ID,
		RootIssueID:           issue.ID,
		ChannelType:           channelTypeFeishu,
		ChatID:                string(chatID),
		ThreadRootMessageID:   "",
		StewardInstallationID: plan.steward.ID,
		CreatedBy:             p.ActorUserID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Another replica bound the issue first; Lark's create uuid gave it
		// the same chat.
		chat, _, gerr := s.GetChat(ctx, issue.ID)
		return chat, gerr
	}
	if err != nil {
		return IssueChat{}, err
	}
	s.sendIntro(ctx, creds, row, issue, identifier, issueLink, plan)
	return s.chatView(ctx, row), nil
}

func (s *IssueChatService) addMembers(ctx context.Context, creds InstallationCredentials, chatID ChatID, idType string, ids []string) {
	if len(ids) == 0 {
		return
	}
	invalid, err := s.cfg.Chats.AddChatMembers(ctx, creds, AddChatMembersParams{ChatID: chatID, MemberIDType: idType, IDs: ids})
	if err != nil || len(invalid) > 0 {
		s.cfg.Logger.Warn("lark issue chat: some members could not be added",
			"chat_id", string(chatID), "id_type", idType, "invalid", invalid, "error", err)
	}
}

func issueChatName(identifier, title string) string {
	name := "[" + identifier + "] " + strings.TrimSpace(title)
	if r := []rune(name); len(r) > 60 {
		name = string(r[:59]) + "…"
	}
	return name
}

// chatPlan is who EnsureChat invites. Users bound on the steward bot are
// invited by open_id; users only bound on other bots of the workspace by
// union_id, which Lark accepts across one developer's apps.
type chatPlan struct {
	steward      Installation
	assignee     string
	botAppIDs    []string
	createIDType string
	createUsers  []string
	ownerID      string
	extraIDType  string
	extraUsers   []string
	uninvited    []string
}

// steward picks the bot that owns the chat: the squad leader's for a squad
// issue, otherwise the assignee agent's.
func (s *IssueChatService) steward(ctx context.Context, issue db.Issue) (Installation, error) {
	var agentID pgtype.UUID
	switch issue.AssigneeType.String {
	case "squad":
		squad, err := s.cfg.Store.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return Installation{}, ErrIssueChatNoSteward
		}
		if err != nil {
			return Installation{}, err
		}
		agentID = squad.LeaderID
	case "agent":
		agentID = issue.AssigneeID
	default:
		return Installation{}, ErrIssueChatNoSteward
	}
	inst, ok, err := s.agentInstallation(ctx, issue.WorkspaceID, agentID)
	if err != nil {
		return Installation{}, err
	}
	if !ok {
		return Installation{}, ErrIssueChatNoSteward
	}
	return inst, nil
}

func (s *IssueChatService) agentInstallation(ctx context.Context, workspaceID, agentID pgtype.UUID) (Installation, bool, error) {
	if !agentID.Valid {
		return Installation{}, false, nil
	}
	row, err := s.cfg.Store.GetActiveChannelInstallationByAgent(ctx, db.GetActiveChannelInstallationByAgentParams{
		WorkspaceID: workspaceID, AgentID: agentID, ChannelType: channelTypeFeishu,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Installation{}, false, nil
	}
	if err != nil {
		return Installation{}, false, err
	}
	inst, err := installationFromRow(row)
	return inst, err == nil, err
}

func (s *IssueChatService) planChat(ctx context.Context, issue db.Issue, actor pgtype.UUID) (chatPlan, error) {
	steward, err := s.steward(ctx, issue)
	if err != nil {
		return chatPlan{}, err
	}
	plan := chatPlan{steward: steward}
	var agentIDs, userIDs []pgtype.UUID
	switch issue.AssigneeType.String {
	case "squad":
		if squad, err := s.cfg.Store.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID}); err == nil {
			plan.assignee = squad.Name
		}
		members, err := s.cfg.Store.ListSquadMembers(ctx, issue.AssigneeID)
		if err != nil {
			return chatPlan{}, err
		}
		for _, m := range members {
			if m.MemberType == "agent" {
				agentIDs = append(agentIDs, m.MemberID)
			} else if m.MemberType == "member" {
				userIDs = append(userIDs, m.MemberID)
			}
		}
	case "agent":
		if agent, err := s.cfg.Store.GetAgent(ctx, issue.AssigneeID); err == nil {
			plan.assignee = agent.Name
		}
	}
	if issue.CreatorType == "member" {
		userIDs = append(userIDs, issue.CreatorID)
	}
	userIDs = append(userIDs, actor)

	seenApp := map[string]bool{steward.AppID: true}
	for _, agentID := range agentIDs {
		inst, ok, err := s.agentInstallation(ctx, issue.WorkspaceID, agentID)
		if err != nil {
			return chatPlan{}, err
		}
		if !ok || seenApp[inst.AppID] || len(plan.botAppIDs) >= maxBotsPerChat-1 {
			continue
		}
		seenApp[inst.AppID] = true
		plan.botAppIDs = append(plan.botAppIDs, inst.AppID)
	}

	userIDs = uniqueUUIDs(userIDs)
	bindings, err := s.cfg.Store.ListChannelUserBindingsForUsers(ctx, db.ListChannelUserBindingsForUsersParams{
		WorkspaceID: issue.WorkspaceID, ChannelType: channelTypeFeishu,
		UserIds: userIDs, PreferredInstallationID: steward.ID,
	})
	if err != nil {
		return chatPlan{}, err
	}
	openIDs, unionIDs := map[string]string{}, map[string]string{}
	for _, row := range bindings {
		b, err := userBindingFromRow(row)
		if err != nil {
			continue
		}
		key := uuidString(b.MulticaUserID)
		if b.InstallationID == steward.ID && openIDs[key] == "" {
			openIDs[key] = b.ChannelUserID
		}
		if b.UnionID.String != "" && unionIDs[key] == "" {
			unionIDs[key] = b.UnionID.String
		}
	}
	actorKey := uuidString(actor)
	plan.createIDType, plan.extraIDType = MemberIDTypeOpenID, MemberIDTypeUnionID
	if openIDs[actorKey] == "" && unionIDs[actorKey] != "" {
		plan.createIDType, plan.extraIDType = MemberIDTypeUnionID, MemberIDTypeOpenID
	}
	for _, id := range userIDs {
		key := uuidString(id)
		byType := map[string]string{MemberIDTypeOpenID: openIDs[key], MemberIDTypeUnionID: unionIDs[key]}
		switch {
		case byType[plan.createIDType] != "":
			plan.createUsers = append(plan.createUsers, byType[plan.createIDType])
			if key == actorKey {
				plan.ownerID = byType[plan.createIDType]
			}
		case byType[plan.extraIDType] != "":
			plan.extraUsers = append(plan.extraUsers, byType[plan.extraIDType])
		default:
			name := key
			if u, err := s.cfg.Store.GetUser(ctx, id); err == nil {
				name = u.Name
			}
			plan.uninvited = append(plan.uninvited, name)
		}
	}
	return plan, nil
}

func uniqueUUIDs(ids []pgtype.UUID) []pgtype.UUID {
	seen := make(map[pgtype.UUID]bool, len(ids))
	out := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		if id.Valid && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func (s *IssueChatService) sendIntro(ctx context.Context, creds InstallationCredentials, row db.ChannelIssueChat, issue db.Issue, identifier, issueLink string, plan chatPlan) {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s %s**\n", identifier, issue.Title)
	fmt.Fprintf(&b, "状态：%s", issue.Status)
	if plan.assignee != "" {
		fmt.Fprintf(&b, " · 负责人：%s", plan.assignee)
	}
	if issueLink != "" {
		fmt.Fprintf(&b, "\n[在 Multica 中打开](%s)", issueLink)
	}
	b.WriteString("\n\n群里的发言会同步为该任务的评论，任务的评论也会同步到群里。@ 某个 Agent 即可让它开始处理。")
	if len(plan.uninvited) > 0 {
		fmt.Fprintf(&b, "\n\n以下成员尚未绑定飞书，未能拉入群：%s", strings.Join(plan.uninvited, "、"))
	}
	card, err := issueChatCardJSON("", "", b.String())
	if err == nil {
		var msgID string
		msgID, err = s.cfg.API.SendInteractiveCard(ctx, SendCardParams{InstallationID: creds, ChatID: ChatID(row.ChatID), CardJSON: card})
		if err == nil {
			err = s.cfg.Store.RecordChannelIssueMessage(ctx, db.RecordChannelIssueMessageParams{
				WorkspaceID: row.WorkspaceID, IssueID: row.IssueID, BindingID: row.ID,
				InstallationID: row.StewardInstallationID, ChannelMessageID: msgID,
				Direction: issueMessageOutbound, Kind: issueMessageKindIntro,
			})
		}
	}
	if err != nil {
		s.cfg.Logger.Warn("lark issue chat: intro card failed", "chat_id", row.ChatID, "error", err)
	}
}

// ---- outbound comment mirror ----

// Register subscribes the comment mirror to the bus.
func (s *IssueChatService) Register(bus *events.Bus) {
	bus.Subscribe(protocol.EventCommentCreated, s.handleEvent)
	bus.Subscribe(protocol.EventCommentUpdated, s.handleEvent)
	bus.Subscribe(protocol.EventCommentDeleted, s.handleEvent)
}

// commentEvent is the slice of a comment event payload the mirror reads. The
// payload carries handler.CommentResponse or an equivalent map, so it is
// decoded through JSON rather than by type.
type commentEvent struct {
	ID         string  `json:"id"`
	IssueID    string  `json:"issue_id"`
	AuthorType string  `json:"author_type"`
	AuthorID   string  `json:"author_id"`
	Content    string  `json:"content"`
	Type       string  `json:"type"`
	ParentID   *string `json:"parent_id"`
	DeletedAt  *string `json:"deleted_at"`
}

type commentSyncJob struct {
	eventType   string
	workspaceID string
	comment     commentEvent
}

func commentSyncJobFromEvent(e events.Event) (commentSyncJob, bool) {
	payload, ok := e.Payload.(map[string]any)
	if !ok {
		return commentSyncJob{}, false
	}
	job := commentSyncJob{eventType: e.Type, workspaceID: e.WorkspaceID}
	if e.Type == protocol.EventCommentDeleted {
		job.comment.ID, _ = payload["comment_id"].(string)
		job.comment.IssueID, _ = payload["issue_id"].(string)
		return job, job.comment.ID != "" && job.comment.IssueID != ""
	}
	if _, fromChannel := payload[CommentChannelOriginKey]; fromChannel && e.Type == protocol.EventCommentCreated {
		return commentSyncJob{}, false
	}
	raw, err := json.Marshal(payload["comment"])
	if err != nil || json.Unmarshal(raw, &job.comment) != nil {
		return commentSyncJob{}, false
	}
	return job, job.comment.ID != "" && job.comment.IssueID != ""
}

func (s *IssueChatService) handleEvent(e events.Event) {
	job, ok := commentSyncJobFromEvent(e)
	if !ok {
		return
	}
	if s.cfg.Synchronous {
		s.runJob(job)
		return
	}
	go s.runJob(job)
}

// errSyncPermanent ends retries for a job that cannot succeed.
var errSyncPermanent = errors.New("lark issue chat: permanent sync failure")

// runJob mirrors one comment change. Jobs of one issue run in order. Delivery
// is best effort: a failed send is retried a few times and then logged; the
// issue itself is never affected.
func (s *IssueChatService) runJob(job commentSyncJob) {
	defer s.lock("issue:" + job.comment.IssueID)()
	var err error
	for attempt := 0; attempt < issueChatSyncAttempts; attempt++ {
		if attempt > 0 && !s.cfg.Synchronous {
			time.Sleep(time.Duration(1<<(attempt-1)) * time.Second)
		}
		ctx, cancel := context.WithTimeout(context.Background(), issueChatSyncTimeout)
		err = s.syncComment(ctx, job)
		cancel()
		if err == nil || errors.Is(err, errSyncPermanent) {
			break
		}
	}
	if err != nil {
		s.cfg.Logger.Warn("lark issue chat: comment mirror failed",
			"event_type", job.eventType, "comment_id", job.comment.ID, "issue_id", job.comment.IssueID, "error", err)
	}
}

func (s *IssueChatService) syncComment(ctx context.Context, job commentSyncJob) error {
	issueID, err := util.ParseUUID(job.comment.IssueID)
	if err != nil {
		return errSyncPermanent
	}
	commentID, err := util.ParseUUID(job.comment.ID)
	if err != nil {
		return errSyncPermanent
	}
	binding, err := s.cfg.Store.GetChannelIssueChatByIssue(ctx, db.GetChannelIssueChatByIssueParams{IssueID: issueID, ChannelType: channelTypeFeishu})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if binding.Status != "active" {
		return nil
	}
	steward, err := s.cfg.Store.GetLarkInstallation(ctx, binding.StewardInstallationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if InstallationStatus(steward.Status) != InstallationActive {
		return nil
	}
	mirrored, err := s.cfg.Store.ListChannelIssueMessagesByComment(ctx, commentID)
	if err != nil {
		return err
	}
	switch job.eventType {
	case protocol.EventCommentCreated:
		if len(mirrored) > 0 || !mirroredCommentType(job.comment.Type) || strings.TrimSpace(job.comment.Content) == "" {
			return nil
		}
		return s.mirrorNewComment(ctx, binding, steward, commentID, job.comment)
	case protocol.EventCommentUpdated:
		return s.patchMirrors(ctx, binding, steward, mirrored, &job.comment)
	case protocol.EventCommentDeleted:
		return s.patchMirrors(ctx, binding, steward, mirrored, nil)
	}
	return nil
}

func mirroredCommentType(t string) bool {
	return t == "" || t == "comment" || t == "progress_update"
}

// commentSender picks the bot that posts a comment: an agent speaks through
// its own bot; everyone else is relayed by the steward under a header.
func (s *IssueChatService) commentSender(ctx context.Context, binding db.ChannelIssueChat, steward Installation, c commentEvent) (Installation, string) {
	if c.AuthorType == "agent" {
		if agentID, err := util.ParseUUID(c.AuthorID); err == nil {
			if inst, ok, err := s.agentInstallation(ctx, binding.WorkspaceID, agentID); err == nil && ok {
				return inst, ""
			}
		}
	}
	return steward, s.authorLabel(ctx, c)
}

func (s *IssueChatService) authorLabel(ctx context.Context, c commentEvent) string {
	id, err := util.ParseUUID(c.AuthorID)
	if err != nil {
		return "Multica"
	}
	switch c.AuthorType {
	case "member":
		if u, err := s.cfg.Store.GetUser(ctx, id); err == nil {
			return u.Name
		}
	case "agent":
		if a, err := s.cfg.Store.GetAgent(ctx, id); err == nil {
			return a.Name
		}
	}
	return "Multica"
}

func (s *IssueChatService) mirrorNewComment(ctx context.Context, binding db.ChannelIssueChat, steward Installation, commentID pgtype.UUID, c commentEvent) error {
	sender, header := s.commentSender(ctx, binding, steward, c)
	body := s.commentMarkdown(ctx, binding.WorkspaceID, c.Content)
	target, err := s.replyTarget(ctx, binding, c)
	if err != nil {
		return err
	}
	msgID, err := s.sendCard(ctx, sender, binding, header, body, target)
	var apiErr *APIError
	if err != nil && sender.ID != steward.ID && errors.As(err, &apiErr) {
		// The author's bot may not be in this chat; the steward relays.
		sender, header = steward, s.authorLabel(ctx, c)
		msgID, err = s.sendCard(ctx, sender, binding, header, body, target)
	}
	if err != nil {
		return err
	}
	if rerr := s.cfg.Store.RecordChannelIssueMessage(ctx, db.RecordChannelIssueMessageParams{
		WorkspaceID: binding.WorkspaceID, IssueID: binding.IssueID, BindingID: binding.ID,
		InstallationID: sender.ID, ChannelMessageID: msgID,
		Direction: issueMessageOutbound, Kind: issueMessageKindComment, CommentID: commentID,
	}); rerr != nil {
		// The card is out; a retry would post it twice.
		s.cfg.Logger.Warn("lark issue chat: record mirrored comment failed", "comment_id", c.ID, "error", rerr)
	}
	return nil
}

func (s *IssueChatService) sendCard(ctx context.Context, sender Installation, binding db.ChannelIssueChat, header, body string, target ReplyTarget) (string, error) {
	creds, err := installationCredentialsFor(sender, s.cfg.Credentials)
	if err != nil {
		return "", err
	}
	subtitle := ""
	if header != "" {
		subtitle = "来自 Multica"
	}
	card, err := issueChatCardJSON(header, subtitle, body)
	if err != nil {
		return "", errSyncPermanent
	}
	return s.cfg.API.SendInteractiveCard(ctx, SendCardParams{
		InstallationID: creds, ChatID: ChatID(binding.ChatID), CardJSON: card, ReplyTarget: target,
	})
}

// replyTarget quotes the mirror of the parent comment, so a web reply reads
// as a reply in the chat too. A sub-issue topic threads everything under its
// root message.
func (s *IssueChatService) replyTarget(ctx context.Context, binding db.ChannelIssueChat, c commentEvent) (ReplyTarget, error) {
	inThread := binding.ThreadRootMessageID != ""
	if c.ParentID != nil {
		if parentID, err := util.ParseUUID(*c.ParentID); err == nil {
			rows, err := s.cfg.Store.ListChannelIssueMessagesByComment(ctx, parentID)
			if err != nil {
				return ReplyTarget{}, err
			}
			if len(rows) > 0 {
				return ReplyTarget{MessageID: rows[0].ChannelMessageID, InThread: inThread}, nil
			}
		}
	}
	if inThread {
		return ReplyTarget{MessageID: binding.ThreadRootMessageID, InThread: true}, nil
	}
	return ReplyTarget{}, nil
}

// patchMirrors re-renders every card mirroring a comment; c == nil renders
// the deleted state. Cards a person posted in the chat (inbound) are theirs
// and stay untouched.
func (s *IssueChatService) patchMirrors(ctx context.Context, binding db.ChannelIssueChat, steward Installation, mirrored []db.ChannelIssueMessage, c *commentEvent) error {
	var firstErr error
	for _, row := range mirrored {
		if row.Direction != issueMessageOutbound || row.Kind != issueMessageKindComment {
			continue
		}
		inst, err := s.cfg.Store.GetLarkInstallation(ctx, row.InstallationID)
		if err != nil {
			continue
		}
		header, subtitle, body := "", "", "~~该评论已删除~~"
		if c != nil && c.DeletedAt == nil {
			body = s.commentMarkdown(ctx, binding.WorkspaceID, c.Content)
			if inst.ID == steward.ID {
				if sender, label := s.commentSender(ctx, binding, steward, *c); sender.ID == steward.ID {
					header, subtitle = label, "来自 Multica"
				}
			}
		}
		card, err := issueChatCardJSON(header, subtitle, body)
		if err != nil {
			continue
		}
		creds, err := installationCredentialsFor(inst, s.cfg.Credentials)
		if err == nil {
			err = s.cfg.API.PatchInteractiveCard(ctx, PatchCardParams{InstallationID: creds, LarkCardMessageID: row.ChannelMessageID, CardJSON: card})
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// commentMarkdown renders comment Markdown for a card: Multica references
// become web links, and over-long bodies point to the issue.
func (s *IssueChatService) commentMarkdown(ctx context.Context, workspaceID pgtype.UUID, content string) string {
	content = linkWorkspaceReferences(ctx, s.cfg.Store, s.cfg.AppURL, workspaceID, content)
	if r := []rune(content); len(r) > maxMirroredRunes {
		content = string(r[:maxMirroredRunes]) + "\n\n…（内容过长，完整内容请在 Multica 中查看）"
	}
	return content
}
