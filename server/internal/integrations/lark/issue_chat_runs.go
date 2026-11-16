package lark

// Agent runs on an issue-bound chat (docs/lark-issue-group-sync.md, P2): each
// run is one live card posted by the agent's own bot, updated with progress
// and settled when the run ends.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

const (
	issueMessageKindTask = "task"
	// runCardMinInterval throttles progress patches of one run card.
	runCardMinInterval = 3 * time.Second
)

// runEvent is the slice of a task event the run mirror reads.
type runEvent struct {
	eventType    string
	taskID       pgtype.UUID
	issueID      pgtype.UUID
	agentID      pgtype.UUID
	summary      string
	step, total  int
	errText      string
	retryPending bool
}

func runEventFrom(e events.Event) (runEvent, bool) {
	ev := runEvent{eventType: e.Type}
	var raw struct {
		TaskID       string `json:"task_id"`
		AgentID      string `json:"agent_id"`
		IssueID      string `json:"issue_id"`
		Summary      string `json:"summary"`
		Step         int    `json:"step"`
		Total        int    `json:"total"`
		Error        string `json:"error"`
		RetryPending bool   `json:"retry_pending"`
	}
	body, err := json.Marshal(e.Payload)
	if err != nil || json.Unmarshal(body, &raw) != nil {
		return runEvent{}, false
	}
	if raw.TaskID == "" {
		raw.TaskID = e.TaskID
	}
	ev.taskID, err = util.ParseUUID(raw.TaskID)
	if err != nil {
		return runEvent{}, false
	}
	ev.issueID, _ = util.ParseUUID(raw.IssueID)
	ev.agentID, _ = util.ParseUUID(raw.AgentID)
	ev.summary, ev.step, ev.total = raw.Summary, raw.Step, raw.Total
	ev.errText, ev.retryPending = raw.Error, raw.RetryPending
	return ev, true
}

// runCards keeps, per running task, what the live card shows.
type runCards struct {
	mu    sync.Mutex
	state map[pgtype.UUID]*runState
}

type runState struct {
	started  time.Time
	patched  time.Time
	issueID  pgtype.UUID
	agentID  pgtype.UUID
	activity []string
}

const (
	runActivityLines = 2
	// runCardDelay keeps short runs (a leader delegating) out of the chat:
	// a card opens only once a run has gone on this long.
	runCardDelay = 20 * time.Second
)

func (r *runCards) get(taskID pgtype.UUID) *runState {
	if r.state == nil {
		r.state = map[pgtype.UUID]*runState{}
	}
	st, ok := r.state[taskID]
	if !ok {
		st = &runState{started: time.Now()}
		r.state[taskID] = st
	}
	return st
}

func (r *runCards) start(ev runEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.get(ev.taskID)
	st.issueID, st.agentID = ev.issueID, ev.agentID
}

// record adds an activity line and reports whether the card is due a patch.
func (r *runCards) record(taskID pgtype.UUID, line string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.get(taskID)
	if line != "" && (len(st.activity) == 0 || st.activity[len(st.activity)-1] != line) {
		st.activity = append(st.activity, line)
		if len(st.activity) > runActivityLines {
			st.activity = st.activity[len(st.activity)-runActivityLines:]
		}
	}
	if now.Sub(st.patched) < runCardMinInterval {
		return false
	}
	st.patched = now
	return true
}

func (r *runCards) snapshot(taskID pgtype.UUID, finish bool) runState {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.get(taskID)
	out := *st
	out.activity = append([]string(nil), st.activity...)
	if finish {
		delete(r.state, taskID)
	}
	return out
}

// registerRuns subscribes the run mirror.
func (s *IssueChatService) registerRuns(bus *events.Bus) {
	for _, t := range []string{protocol.EventTaskRunning, protocol.EventTaskProgress, protocol.EventTaskMessage, protocol.EventTaskCompleted, protocol.EventTaskFailed, protocol.EventTaskCancelled} {
		bus.Subscribe(t, s.handleRunEvent)
	}
}

func (s *IssueChatService) handleRunEvent(e events.Event) {
	ev, ok := runEventFrom(e)
	if !ok || e.ChatSessionID != "" {
		return
	}
	switch ev.eventType {
	case protocol.EventTaskRunning:
		s.runs.start(ev)
		if ev.issueID.Valid {
			s.dispatch(func() {
				ctx, cancel := context.WithTimeout(context.Background(), issueChatSyncTimeout)
				defer cancel()
				if err := s.activateChat(ctx, ev.issueID); err != nil {
					s.cfg.Logger.Warn("lark issue chat: activate chat failed", "issue_id", uuidString(ev.issueID), "error", err)
				}
			})
		}
		return
	case protocol.EventTaskProgress:
		if !s.runs.record(ev.taskID, "", time.Now()) {
			return
		}
	case protocol.EventTaskMessage:
		if !s.runs.record(ev.taskID, activityLine(e.Payload), time.Now()) {
			return
		}
	}
	s.dispatch(func() {
		defer s.lock("run:" + uuidString(ev.taskID))()
		ctx, cancel := context.WithTimeout(context.Background(), issueChatSyncTimeout)
		defer cancel()
		if err := s.syncRun(ctx, ev); err != nil {
			s.cfg.Logger.Warn("lark issue chat: run card failed", "task_id", uuidString(ev.taskID), "event_type", ev.eventType, "error", err)
		}
	})
}

// toolCategories names what a tool does without exposing its arguments,
// which may carry commands, paths or secrets.
var toolCategories = map[string]string{
	"bash": "运行命令", "shell": "运行命令", "exec": "运行命令", "exec_command": "运行命令", "local_shell": "运行命令",
	"edit": "修改文件", "write": "修改文件", "multiedit": "修改文件", "apply_patch": "修改文件", "str_replace_editor": "修改文件",
	"read": "读取文件", "view": "读取文件",
	"grep": "搜索代码", "glob": "搜索代码", "search": "搜索代码", "ls": "浏览目录",
	"webfetch": "查阅资料", "websearch": "查阅资料", "web_search": "查阅资料",
	"todowrite": "整理计划", "update_plan": "整理计划",
}

// activityLine renders one transcript frame as a short progress line: the
// kind of work the agent is doing, or the opening of what it says.
func activityLine(payload any) string {
	var m protocol.TaskMessagePayload
	switch v := payload.(type) {
	case protocol.TaskMessagePayload:
		m = v
	case *protocol.TaskMessagePayload:
		m = *v
	default:
		raw, err := json.Marshal(payload)
		if err != nil || json.Unmarshal(raw, &m) != nil {
			return ""
		}
	}
	switch m.Type {
	case "tool_use":
		if category, ok := toolCategories[strings.ToLower(m.Tool)]; ok {
			return category
		}
		if m.Tool != "" {
			return "使用工具 " + oneLine(m.Tool, 30)
		}
	case "text":
		return oneLine(m.Content, 60)
	case "error":
		return "遇到错误"
	}
	return ""
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

func formatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	return fmt.Sprintf("%d 分 %d 秒", int(d.Minutes()), int(d.Seconds())%60)
}

func (s *IssueChatService) syncRun(ctx context.Context, ev runEvent) error {
	finish := ev.eventType == protocol.EventTaskCompleted || ev.eventType == protocol.EventTaskCancelled ||
		(ev.eventType == protocol.EventTaskFailed && !ev.retryPending)
	st := s.runs.snapshot(ev.taskID, finish)
	card, err := s.cfg.Store.GetChannelIssueMessageByTask(ctx, ev.taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		// A run that ends before the delay never gets a card, and neither
		// does one that has already commented: its comment card (or the
		// card it took over) is the run's message in the chat.
		if finish || time.Since(st.started) < runCardDelay || !st.issueID.Valid {
			return nil
		}
		if _, err := s.cfg.Store.GetLatestCommentBySourceTask(ctx, ev.taskID); err == nil {
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return s.openRunCard(ctx, ev.taskID, st)
	}
	if err != nil {
		return err
	}
	inst, err := s.cfg.Store.GetLarkInstallation(ctx, card.InstallationID)
	if err != nil {
		return err
	}
	var body string
	switch {
	case ev.eventType == protocol.EventTaskCompleted:
		// A run that commented has already turned this card into its
		// comment; one that did not leaves only a one-line receipt.
		body = s.finishedBody(ctx, inst, ev.taskID, "**已完成**", st, "")
	case ev.eventType == protocol.EventTaskCancelled:
		body = s.finishedBody(ctx, inst, ev.taskID, "**已取消**", st, "")
	case ev.eventType == protocol.EventTaskFailed && !ev.retryPending:
		reason := "失败原因请在 Multica 中查看，或在群里 @ 它重试。"
		if ev.errText != "" {
			reason = oneLine(ev.errText, 120) + "\n可在群里 @ 它重试。"
		}
		body = s.finishedBody(ctx, inst, ev.taskID, "**运行失败**", st, reason)
	case ev.eventType == protocol.EventTaskFailed:
		body = s.liveBody(ctx, inst, ev.taskID, "**运行失败，正在重试…**", st)
	default:
		body = s.liveBody(ctx, inst, ev.taskID, "**运行中**", st)
	}
	return s.patchRunCard(ctx, inst, card.ChannelMessageID, body+s.detailsLink(ctx, card.IssueID))
}

func (s *IssueChatService) liveBody(ctx context.Context, sender Installation, taskID pgtype.UUID, status string, st runState) string {
	var b strings.Builder
	b.WriteString(s.agentPrefix(ctx, sender, taskID) + status + " · 已运行 " + formatElapsed(time.Since(st.started)))
	for _, line := range st.activity {
		b.WriteString("\n- " + line)
	}
	return b.String()
}

func (s *IssueChatService) finishedBody(ctx context.Context, sender Installation, taskID pgtype.UUID, status string, st runState, detail string) string {
	body := s.agentPrefix(ctx, sender, taskID) + status + " · 用时 " + formatElapsed(time.Since(st.started))
	if detail != "" {
		body += "\n" + detail
	}
	return body
}

// takeOverRunCard turns the live card of the run that wrote c into c's card,
// so a run shows up in the chat as one message rather than a run card
// followed by a near-identical comment card. Replies keep their own card:
// they need to quote the comment they answer.
func (s *IssueChatService) takeOverRunCard(ctx context.Context, binding db.ChannelIssueChat, commentID pgtype.UUID, c commentEvent) (bool, error) {
	if c.SourceTaskID == nil || c.ParentID != nil {
		return false, nil
	}
	taskID, err := util.ParseUUID(*c.SourceTaskID)
	if err != nil {
		return false, nil
	}
	defer s.lock("run:" + uuidString(taskID))()
	card, err := s.cfg.Store.GetChannelIssueMessageByTask(ctx, taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if card.BindingID != binding.ID {
		return false, nil
	}
	inst, err := s.cfg.Store.GetLarkInstallation(ctx, card.InstallationID)
	if err != nil {
		return false, err
	}
	header := ""
	if c.AuthorType != "agent" || c.AuthorID != uuidString(inst.AgentID) {
		header = s.authorLabel(ctx, c)
	}
	cardJSON, err := s.commentCardJSON(ctx, binding.WorkspaceID, inst, header, c)
	if err != nil {
		return false, errSyncPermanent
	}
	creds, err := installationCredentialsFor(inst, s.cfg.Credentials)
	if err != nil {
		return false, err
	}
	if err := s.cfg.API.PatchInteractiveCard(ctx, PatchCardParams{InstallationID: creds, LarkCardMessageID: card.ChannelMessageID, CardJSON: cardJSON}); err != nil {
		return false, err
	}
	if _, err := s.cfg.Store.ConvertChannelIssueRunCardToComment(ctx, db.ConvertChannelIssueRunCardToCommentParams{
		ChannelMessageID: card.ChannelMessageID, CommentID: commentID,
	}); err != nil {
		// The card already shows the comment; a retry would post it twice.
		s.cfg.Logger.Warn("lark issue chat: record run card takeover failed", "comment_id", c.ID, "error", err)
	}
	return true, nil
}

func (s *IssueChatService) detailsLink(ctx context.Context, issueID pgtype.UUID) string {
	issue, err := s.cfg.Store.GetIssue(ctx, issueID)
	if err != nil {
		return ""
	}
	ws, err := s.cfg.Store.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return ""
	}
	if link := channel.IssueWebLink(s.cfg.AppURL, ws.Slug, service.IssueIdentifier(ws.IssuePrefix, issue.Number)); link != "" {
		return "\n[查看详情](" + link + ")"
	}
	return ""
}

// agentPrefix names the agent when another bot relays its card; the
// sender's avatar already says who it is otherwise.
func (s *IssueChatService) agentPrefix(ctx context.Context, sender Installation, taskID pgtype.UUID) string {
	task, err := s.cfg.Store.GetAgentTask(ctx, taskID)
	if err != nil || task.AgentID == sender.AgentID {
		return ""
	}
	if agent, err := s.cfg.Store.GetAgent(ctx, task.AgentID); err == nil {
		return "**" + agent.Name + "** · "
	}
	return ""
}

func (s *IssueChatService) patchRunCard(ctx context.Context, inst Installation, messageID, body string) error {
	creds, err := installationCredentialsFor(inst, s.cfg.Credentials)
	if err != nil {
		return err
	}
	cardJSON, err := issueChatCardJSON("", "", body)
	if err != nil {
		return err
	}
	return s.cfg.API.PatchInteractiveCard(ctx, PatchCardParams{InstallationID: creds, LarkCardMessageID: messageID, CardJSON: cardJSON})
}

func (s *IssueChatService) openRunCard(ctx context.Context, taskID pgtype.UUID, st runState) error {
	binding, err := s.cfg.Store.GetChannelIssueChatByIssue(ctx, db.GetChannelIssueChatByIssueParams{IssueID: st.issueID, ChannelType: channelTypeFeishu})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil || binding.Status != "active" {
		return err
	}
	steward, ok, err := s.currentSteward(ctx, &binding)
	if err != nil || !ok {
		return err
	}
	sender := steward
	if inst, ok, err := s.agentInstallation(ctx, binding.WorkspaceID, st.agentID); err == nil && ok {
		sender = inst
	}
	var target ReplyTarget
	if binding.ThreadRootMessageID != "" {
		target = ReplyTarget{MessageID: binding.ThreadRootMessageID, InThread: true}
	}
	link := s.detailsLink(ctx, binding.IssueID)
	msgID, err := s.sendCard(ctx, sender, binding, "", s.liveBody(ctx, sender, taskID, "**运行中**", st)+link, target)
	var apiErr *APIError
	if err != nil && sender.ID != steward.ID && errors.As(err, &apiErr) {
		sender = steward
		msgID, err = s.sendCard(ctx, sender, binding, "", s.liveBody(ctx, sender, taskID, "**运行中**", st)+link, target)
	}
	if err != nil {
		return err
	}
	return s.cfg.Store.RecordChannelIssueMessage(ctx, db.RecordChannelIssueMessageParams{
		WorkspaceID: binding.WorkspaceID, IssueID: binding.IssueID, BindingID: binding.ID,
		InstallationID: sender.ID, ChannelMessageID: msgID,
		Direction: issueMessageOutbound, Kind: issueMessageKindTask, TaskID: taskID,
	})
}
