package lark

// Agent runs on an issue-bound chat (docs/lark-issue-group-sync.md, P2): each
// run is one live card posted by the agent's own bot, updated with progress
// and settled when the run ends.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
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

// runCards remembers when each run card was last patched.
type runCards struct {
	mu   sync.Mutex
	last map[pgtype.UUID]time.Time
}

func (r *runCards) allow(taskID pgtype.UUID, now time.Time, force bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last = map[pgtype.UUID]time.Time{}
	}
	if !force && now.Sub(r.last[taskID]) < runCardMinInterval {
		return false
	}
	if force {
		delete(r.last, taskID)
	} else {
		r.last[taskID] = now
	}
	return true
}

// registerRuns subscribes the run mirror.
func (s *IssueChatService) registerRuns(bus *events.Bus) {
	for _, t := range []string{protocol.EventTaskRunning, protocol.EventTaskProgress, protocol.EventTaskCompleted, protocol.EventTaskFailed, protocol.EventTaskCancelled} {
		bus.Subscribe(t, s.handleRunEvent)
	}
}

func (s *IssueChatService) handleRunEvent(e events.Event) {
	ev, ok := runEventFrom(e)
	if !ok || e.ChatSessionID != "" {
		return
	}
	if ev.eventType == protocol.EventTaskProgress && !s.runs.allow(ev.taskID, time.Now(), false) {
		return
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

func (s *IssueChatService) syncRun(ctx context.Context, ev runEvent) error {
	card, err := s.cfg.Store.GetChannelIssueMessageByTask(ctx, ev.taskID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if ev.eventType != protocol.EventTaskRunning || !ev.issueID.Valid {
			return nil
		}
		return s.openRunCard(ctx, ev)
	}
	var body string
	switch ev.eventType {
	case protocol.EventTaskProgress:
		body = progressLine(ev)
	case protocol.EventTaskCompleted:
		body = "**运行已完成**"
	case protocol.EventTaskFailed:
		if ev.retryPending {
			body = "**运行失败，正在重试…**"
		} else {
			body = "**运行失败**"
			if ev.errText != "" {
				body += "\n" + ev.errText
			}
		}
	case protocol.EventTaskCancelled:
		body = "**运行已取消**"
	default:
		return nil
	}
	if ev.eventType != protocol.EventTaskProgress {
		s.runs.allow(ev.taskID, time.Now(), true)
	}
	inst, err := s.cfg.Store.GetLarkInstallation(ctx, card.InstallationID)
	if err != nil {
		return err
	}
	creds, err := installationCredentialsFor(inst, s.cfg.Credentials)
	if err != nil {
		return err
	}
	header := s.runHeader(ctx, inst, ev.agentID)
	cardJSON, err := issueChatCardJSON(header, runSubtitle(header), body)
	if err != nil {
		return err
	}
	return s.cfg.API.PatchInteractiveCard(ctx, PatchCardParams{InstallationID: creds, LarkCardMessageID: card.ChannelMessageID, CardJSON: cardJSON})
}

func progressLine(ev runEvent) string {
	line := "**运行中**"
	if ev.total > 0 {
		line += fmt.Sprintf("（%d/%d）", ev.step, ev.total)
	}
	if ev.summary != "" {
		line += "\n" + ev.summary
	}
	return line
}

func (s *IssueChatService) openRunCard(ctx context.Context, ev runEvent) error {
	binding, err := s.cfg.Store.GetChannelIssueChatByIssue(ctx, db.GetChannelIssueChatByIssueParams{IssueID: ev.issueID, ChannelType: channelTypeFeishu})
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
	if inst, ok, err := s.agentInstallation(ctx, binding.WorkspaceID, ev.agentID); err == nil && ok {
		sender = inst
	}
	var target ReplyTarget
	if binding.ThreadRootMessageID != "" {
		target = ReplyTarget{MessageID: binding.ThreadRootMessageID, InThread: true}
	}
	header := s.runHeader(ctx, sender, ev.agentID)
	msgID, err := s.sendCard(ctx, sender, binding, header, "**运行中**", target)
	var apiErr *APIError
	if err != nil && sender.ID != steward.ID && errors.As(err, &apiErr) {
		sender = steward
		header = s.runHeader(ctx, sender, ev.agentID)
		msgID, err = s.sendCard(ctx, sender, binding, header, "**运行中**", target)
	}
	if err != nil {
		return err
	}
	return s.cfg.Store.RecordChannelIssueMessage(ctx, db.RecordChannelIssueMessageParams{
		WorkspaceID: binding.WorkspaceID, IssueID: binding.IssueID, BindingID: binding.ID,
		InstallationID: sender.ID, ChannelMessageID: msgID,
		Direction: issueMessageOutbound, Kind: issueMessageKindTask, TaskID: ev.taskID,
	})
}

// runHeader names the agent when its run card is relayed by another bot.
func (s *IssueChatService) runHeader(ctx context.Context, sender Installation, agentID pgtype.UUID) string {
	if sender.AgentID == agentID {
		return ""
	}
	if agent, err := s.cfg.Store.GetAgent(ctx, agentID); err == nil {
		return agent.Name
	}
	return "Agent"
}

func runSubtitle(header string) string {
	if header == "" {
		return ""
	}
	return "来自 Multica"
}
