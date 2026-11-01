package lark

// Issue events for issue-bound Feishu chats (docs/lark-issue-group-sync.md,
// P1): status / assignee notices, automatic chats for squad issues and
// sub-issue topics, adding new participants, and steward handover.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

var builtinStatusLabels = map[string]string{
	"backlog":     "待规划",
	"todo":        "待办",
	"in_progress": "进行中",
	"in_review":   "评审中",
	"done":        "已完成",
	"blocked":     "已阻塞",
	"cancelled":   "已取消",
}

// statusLabel names a built-in status; a workspace's custom status keeps
// its key.
func statusLabel(status string) string {
	if label, ok := builtinStatusLabels[status]; ok {
		return label
	}
	return status
}

// issueChange is what an issue event tells the chat sync.
type issueChange struct {
	created         bool
	statusChanged   bool
	assigneeChanged bool
	prevStatus      string
	actorMember     pgtype.UUID
	issueID         pgtype.UUID
}

func issueChangeFromEvent(e events.Event) (issueChange, bool) {
	payload, ok := e.Payload.(map[string]any)
	if !ok {
		return issueChange{}, false
	}
	var issue struct {
		ID string `json:"id"`
	}
	raw, err := json.Marshal(payload["issue"])
	if err != nil || json.Unmarshal(raw, &issue) != nil {
		return issueChange{}, false
	}
	id, err := util.ParseUUID(issue.ID)
	if err != nil {
		return issueChange{}, false
	}
	c := issueChange{created: e.Type == protocol.EventIssueCreated, issueID: id}
	c.statusChanged, _ = payload["status_changed"].(bool)
	c.assigneeChanged, _ = payload["assignee_changed"].(bool)
	c.prevStatus, _ = payload["prev_status"].(string)
	if e.ActorType == "member" {
		if actor, err := util.ParseUUID(e.ActorID); err == nil {
			c.actorMember = actor
		}
	}
	return c, c.created || c.statusChanged || c.assigneeChanged
}

func (s *IssueChatService) handleIssueEvent(e events.Event) {
	change, ok := issueChangeFromEvent(e)
	if !ok {
		return
	}
	s.dispatch(func() {
		defer s.lock("issue:" + uuidString(change.issueID))()
		ctx, cancel := context.WithTimeout(context.Background(), issueChatSyncTimeout)
		defer cancel()
		if err := s.syncIssueChange(ctx, change); err != nil {
			s.cfg.Logger.Warn("lark issue chat: issue sync failed", "issue_id", uuidString(change.issueID), "error", err)
		}
	})
}

func (s *IssueChatService) syncIssueChange(ctx context.Context, change issueChange) error {
	issue, err := s.cfg.Store.GetIssue(ctx, change.issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	binding, err := s.cfg.Store.GetChannelIssueChatByIssue(ctx, db.GetChannelIssueChatByIssueParams{IssueID: issue.ID, ChannelType: channelTypeFeishu})
	if errors.Is(err, pgx.ErrNoRows) {
		if change.created || change.assigneeChanged {
			return s.autoCreate(ctx, issue, pgtype.UUID{}, change.actorMember)
		}
		return nil
	}
	if err != nil || binding.Status != "active" {
		return err
	}
	steward, ok, err := s.currentSteward(ctx, &binding)
	if err != nil || !ok {
		return err
	}
	if change.assigneeChanged {
		steward = s.handover(ctx, &binding, steward, issue)
		if creds, err := installationCredentialsFor(steward, s.cfg.Credentials); err == nil {
			agents, users := s.participants(ctx, issue, pgtype.UUID{})
			s.inviteParticipants(ctx, steward, creds, ChatID(binding.ChatID), agents, users)
		}
	}
	var lines []string
	if change.statusChanged && change.prevStatus != issue.Status {
		lines = append(lines, fmt.Sprintf("**状态**：%s → %s", statusLabel(change.prevStatus), statusLabel(issue.Status)))
	}
	if change.assigneeChanged {
		name := s.assigneeName(ctx, issue)
		if name == "" {
			name = "未指派"
		}
		lines = append(lines, "**负责人**："+name)
	}
	if len(lines) == 0 {
		return nil
	}
	return s.postActivity(ctx, binding, steward, strings.Join(lines, "\n"))
}

// postActivity posts a timeline notice, threaded into a sub-issue's topic.
func (s *IssueChatService) postActivity(ctx context.Context, binding db.ChannelIssueChat, steward Installation, markdown string) error {
	var target ReplyTarget
	if binding.ThreadRootMessageID != "" {
		target = ReplyTarget{MessageID: binding.ThreadRootMessageID, InThread: true}
	}
	msgID, err := s.sendCard(ctx, steward, binding, "", markdown, target)
	if err != nil {
		return err
	}
	return s.cfg.Store.RecordChannelIssueMessage(ctx, db.RecordChannelIssueMessageParams{
		WorkspaceID: binding.WorkspaceID, IssueID: binding.IssueID, BindingID: binding.ID,
		InstallationID: steward.ID, ChannelMessageID: msgID,
		Direction: issueMessageOutbound, Kind: issueMessageKindActivity,
	})
}

// autoCreate opens a chat without anyone clicking: always as a topic when an
// ancestor has a chat, otherwise when the hosting squad (the @-mentioned
// one, else the assignee) has auto_create_channel_chat on. An issue no bot
// can host is skipped silently.
func (s *IssueChatService) autoCreate(ctx context.Context, issue db.Issue, hostSquad, actor pgtype.UUID) error {
	if s.cfg.Chats == nil {
		return nil
	}
	if _, hasAncestor, err := s.ancestorChat(ctx, issue); err != nil {
		return err
	} else if !hasAncestor {
		squadID := hostSquad
		if !squadID.Valid && issue.AssigneeType.String == "squad" {
			squadID = issue.AssigneeID
		}
		if !squadID.Valid {
			return nil
		}
		squad, err := s.cfg.Store.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: squadID, WorkspaceID: issue.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !squad.AutoCreateChannelChat || squad.ArchivedAt.Valid {
			return nil
		}
		if !actor.Valid {
			actor = squad.CreatorID
		}
	}
	if !actor.Valid {
		actor = issue.CreatorID
	}
	_, err := s.EnsureChat(ctx, EnsureIssueChatParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorUserID: actor, SquadID: hostSquad,
	})
	if errors.Is(err, ErrIssueChatNoSteward) {
		return nil
	}
	return err
}

// handleSquadMention opens a chat for an issue whose comment @-mentions a
// squad, hosted by that squad's leader.
func (s *IssueChatService) handleSquadMention(e events.Event) {
	job, ok := commentSyncJobFromEvent(e)
	if !ok || e.Type != protocol.EventCommentCreated {
		return
	}
	var squadIDs []pgtype.UUID
	for _, m := range util.ParseMentions(job.comment.Content) {
		if m.Type == "squad" {
			if id, err := util.ParseUUID(m.ID); err == nil {
				squadIDs = append(squadIDs, id)
			}
		}
	}
	issueID, err := util.ParseUUID(job.comment.IssueID)
	if len(squadIDs) == 0 || err != nil {
		return
	}
	var actor pgtype.UUID
	if job.comment.AuthorType == "member" {
		actor, _ = util.ParseUUID(job.comment.AuthorID)
	}
	s.dispatch(func() {
		defer s.lock("issue:" + uuidString(issueID))()
		ctx, cancel := context.WithTimeout(context.Background(), issueChatSyncTimeout)
		defer cancel()
		if _, bound, err := s.GetChat(ctx, issueID); err != nil || bound {
			return
		}
		issue, err := s.cfg.Store.GetIssue(ctx, issueID)
		if err != nil {
			return
		}
		if err := s.autoCreate(ctx, issue, squadIDs[0], actor); err != nil {
			s.cfg.Logger.Warn("lark issue chat: squad mention chat failed", "issue_id", uuidString(issueID), "error", err)
		}
	})
}

// currentSteward returns the chat's steward, handing the chat to the root
// issue's present steward when the recorded one is gone or disconnected.
func (s *IssueChatService) currentSteward(ctx context.Context, binding *db.ChannelIssueChat) (Installation, bool, error) {
	inst, err := s.cfg.Store.GetLarkInstallation(ctx, binding.StewardInstallationID)
	if err == nil && InstallationStatus(inst.Status) == InstallationActive {
		return inst, true, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Installation{}, false, err
	}
	root, err := s.cfg.Store.GetIssue(ctx, binding.RootIssueID)
	if err != nil {
		return Installation{}, false, nil
	}
	want, err := s.steward(ctx, root, pgtype.UUID{})
	if err != nil {
		return Installation{}, false, nil
	}
	if err := s.setSteward(ctx, binding, want); err != nil {
		return Installation{}, false, err
	}
	return want, true, nil
}

// handover moves a root chat to the steward its issue now calls for (a new
// squad, a new leader, a new agent assignee). The outgoing steward invites
// the incoming bot first, so the new steward can post in the chat.
func (s *IssueChatService) handover(ctx context.Context, binding *db.ChannelIssueChat, steward Installation, issue db.Issue) Installation {
	if binding.RootIssueID != binding.IssueID {
		return steward
	}
	want, err := s.steward(ctx, issue, pgtype.UUID{})
	if err != nil || want.ID == steward.ID {
		return steward
	}
	if creds, err := installationCredentialsFor(steward, s.cfg.Credentials); err == nil {
		s.addMembers(ctx, creds, ChatID(binding.ChatID), MemberIDTypeAppID, []string{want.AppID})
	}
	if err := s.setSteward(ctx, binding, want); err != nil {
		s.cfg.Logger.Warn("lark issue chat: steward handover failed", "chat_id", binding.ChatID, "error", err)
		return steward
	}
	return want
}

func (s *IssueChatService) setSteward(ctx context.Context, binding *db.ChannelIssueChat, want Installation) error {
	if err := s.cfg.Store.SetChannelIssueChatSteward(ctx, db.SetChannelIssueChatStewardParams{
		StewardInstallationID: want.ID, ChannelType: channelTypeFeishu, ChatID: binding.ChatID,
	}); err != nil {
		return err
	}
	binding.StewardInstallationID = want.ID
	binding.UpdatedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return nil
}
