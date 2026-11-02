package lark

// The pinned progress card: a root issue chat's opening card is kept up to
// date with where the work stands, so the chat answers "what stage, waiting
// on whom" without scrolling, in place of a notice per status change.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const progressHint = "群里的发言会同步为该任务的评论，任务的评论也会同步到群里。@ 某个 Agent 即可让它开始处理。"

// progressState is where a root issue's work stands. It drives the progress
// card, the chat name's status prefix and the time-sensitive pin.
type progressState struct {
	children []db.Issue
	// flow is the workflow from the issue's metadata, with each sub-issue
	// filed under its stage; current indexes the stage the work is at.
	flow    []flowStage
	current int
	// tag is the short status shown in front of the chat name.
	tag string
	// now says what the work is waiting on, for the card.
	now string
	// waiting lists the members the work waits on.
	waiting []pgtype.UUID
}

func (s *IssueChatService) progressState(ctx context.Context, issue db.Issue) progressState {
	children, err := s.cfg.Store.ListChildIssues(ctx, issue.ID)
	if err != nil {
		s.cfg.Logger.Warn("lark issue chat: list sub-issues for progress failed", "issue_id", uuidString(issue.ID), "error", err)
	}
	st := progressState{children: children, flow: placeChildren(flowStages(issue.Metadata), children)}
	st.current = currentStage(st.flow)
	switch issue.Status {
	case "done":
		st.tag, st.now = "已完成", "已结束"
		return st
	case "cancelled":
		st.tag, st.now = "已取消", "已结束"
		return st
	case "blocked":
		st.tag, st.now = "已阻塞", "已阻塞"
		return st
	}
	var active, stages []string
	for _, c := range children {
		if c.Status != "todo" && c.Status != "in_progress" && c.Status != "blocked" {
			continue
		}
		who := s.assigneeName(ctx, c)
		switch {
		case c.AssigneeType.String == "member" && who != "":
			active = append(active, "待 "+who+" 处理「"+childLabel(c)+"」")
			st.waiting = append(st.waiting, c.AssigneeID)
		case who != "":
			active = append(active, who+" 处理中「"+childLabel(c)+"」")
		default:
			active = append(active, "「"+childLabel(c)+"」"+statusLabel(c.Status))
		}
		stage := stageOf(st.flow, c)
		if stage == "" {
			stage = childLabel(c)
		}
		stages = append(stages, stage)
	}
	if len(active) > 0 {
		st.now = strings.Join(active, "；")
		st.tag = oneLine(strings.Join(uniqueStrings(stages), "·"), 12)
		if len(st.waiting) > 0 {
			st.tag = "待处理"
		}
		return st
	}
	// With no sub-issue in flight, an agent's latest comment asking members
	// for something (a confirmation, an acceptance) is what the work awaits.
	if c, err := s.cfg.Store.GetLatestCommentForIssue(ctx, issue.ID); err == nil && c.AuthorType == "agent" {
		if who := attentionNames(commentEvent{AuthorType: c.AuthorType, Content: c.Content}); who != "" {
			st.tag, st.now = "待处理", "待 "+who+" 处理"
			for _, m := range util.MentionRe.FindAllStringSubmatch(c.Content, -1) {
				if id, err := util.ParseUUID(m[3]); err == nil && m[2] == "member" {
					st.waiting = append(st.waiting, id)
				}
			}
			return st
		}
	}
	switch issue.Status {
	case "in_review":
		st.tag, st.now = "待验收", "待验收"
	case "backlog", "todo":
		st.tag = "待开始"
	default:
		st.tag = "进行中"
	}
	return st
}

// progressMarkdown renders the progress card body for a root issue.
func (s *IssueChatService) progressMarkdown(ctx context.Context, issue db.Issue, st progressState) string {
	var b strings.Builder
	identifier, link := s.issueIdentity(ctx, issue)
	fmt.Fprintf(&b, "**%s %s**\n状态：%s", identifier, issue.Title, statusLabel(issue.Status))
	if name := s.assigneeName(ctx, issue); name != "" {
		fmt.Fprintf(&b, " · 负责人：%s", name)
	}
	if st.now != "" {
		b.WriteString("\n当前：" + st.now)
	}
	var steps []string
	if len(st.flow) > 0 {
		steps = append(steps, "进度："+flowLine(st.flow, st.current))
	}
	// Finished steps are listed by name only; who is on a step matters only
	// while it runs. Rework rounds show on the progress line instead.
	var done, running []string
	for _, c := range st.children {
		label := childLabel(c)
		switch {
		case !childClosed(c):
			if name := s.assigneeName(ctx, c); name != "" {
				label += " · " + name
			}
			running = append(running, label)
		case len(st.flow) == 0 || !isRework(label):
			done = append(done, label)
		}
	}
	if done = uniqueStrings(done); len(done) > 0 {
		steps = append(steps, "<font color='grey'>已完成："+strings.Join(done, "、")+"</font>")
	}
	if len(running) > 0 {
		steps = append(steps, "进行中："+strings.Join(running, "；"))
	}
	if len(steps) > 0 {
		b.WriteString("\n\n" + strings.Join(steps, "\n"))
	}
	if links := metadataLinks(issue.Metadata); links != "" {
		b.WriteString("\n\n链接：" + links)
	}
	if link != "" {
		fmt.Fprintf(&b, "\n\n[查看任务详情](%s)", link)
	}
	return b.String()
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// childLabel names a sub-issue by its stage: titles follow "<stage>：<root
// title>", and repeating the root title on every line is noise.
func childLabel(c db.Issue) string {
	title := c.Title
	if i := strings.Index(title, "："); i > 0 {
		title = title[:i]
	}
	return oneLine(title, 40)
}

// metadataLinkLabels names the link keys the workflow uses; metadata keys
// are ASCII, the card speaks Chinese.
var metadataLinkLabels = map[string]string{"PRD": "需求文档", "PPE": "PPE 验证"}

// metadataLinks lists the issue's metadata entries whose value is a web
// link, so an agent can surface Meego, PRD, MR and test links on the card by
// setting issue metadata.
func metadataLinks(raw []byte) string {
	var meta map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &meta) != nil {
		return ""
	}
	keys := make([]string, 0, len(meta))
	for k, v := range meta {
		if u, ok := v.(string); ok && (strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")) && !strings.ContainsAny(u, " \n)") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		label := k
		if l, ok := metadataLinkLabels[k]; ok {
			label = l
		}
		label = strings.NewReplacer("[", "", "]", "").Replace(label)
		parts = append(parts, fmt.Sprintf("[%s](%s)", label, meta[k].(string)))
	}
	return strings.Join(parts, " · ")
}

func (s *IssueChatService) issueIdentity(ctx context.Context, issue db.Issue) (identifier, link string) {
	ws, err := s.cfg.Store.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return "", ""
	}
	identifier = service.IssueIdentifier(ws.IssuePrefix, issue.Number)
	return identifier, channel.IssueWebLink(s.cfg.AppURL, ws.Slug, identifier)
}

// refreshProgress re-renders the progress card of the chat issueID belongs
// to (its own, or its root issue's for a sub-issue topic).
func (s *IssueChatService) refreshProgress(ctx context.Context, issueID pgtype.UUID) error {
	binding, err := s.cfg.Store.GetChannelIssueChatByIssue(ctx, db.GetChannelIssueChatByIssueParams{IssueID: issueID, ChannelType: channelTypeFeishu})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if binding.RootIssueID != binding.IssueID {
		binding, err = s.cfg.Store.GetChannelIssueChatByIssue(ctx, db.GetChannelIssueChatByIssueParams{IssueID: binding.RootIssueID, ChannelType: channelTypeFeishu})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	if binding.Status != "active" {
		return nil
	}
	defer s.lock("progress:" + uuidString(binding.ID))()
	card, err := s.cfg.Store.GetChannelIssueIntroMessage(ctx, binding.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	issue, err := s.cfg.Store.GetIssue(ctx, binding.IssueID)
	if err != nil {
		return err
	}
	inst, err := s.cfg.Store.GetLarkInstallation(ctx, card.InstallationID)
	if err != nil {
		return err
	}
	creds, err := installationCredentialsFor(inst, s.cfg.Credentials)
	if err != nil {
		return err
	}
	st := s.progressState(ctx, issue)
	cardJSON, err := issueChatCardJSON("", "", s.progressMarkdown(ctx, issue, st)+"\n\n"+progressHint)
	if err != nil {
		return err
	}
	err = s.cfg.API.PatchInteractiveCard(ctx, PatchCardParams{InstallationID: creds, LarkCardMessageID: card.ChannelMessageID, CardJSON: cardJSON})
	// The chat name and the pin are where people notice the state without
	// opening the chat; the bot that opened the chat administers it.
	s.syncChatName(ctx, binding, creds, issue, st.tag)
	s.syncTimeSensitive(ctx, binding, inst, creds, st.waiting)
	return err
}

// statusChatName is the chat name with the work's status in front, so the
// state shows in the chat list and search results.
func (s *IssueChatService) statusChatName(ctx context.Context, issue db.Issue, tag string) string {
	identifier, _ := s.issueIdentity(ctx, issue)
	return issueChatName(tag, identifier, issue.Title)
}

// syncChatName renames the chat when its status prefix changes. Every rename
// posts a system notice in the chat, so an unchanged name is never resent.
func (s *IssueChatService) syncChatName(ctx context.Context, binding db.ChannelIssueChat, creds InstallationCredentials, issue db.Issue, tag string) {
	want := s.statusChatName(ctx, issue, tag)
	key := uuidString(binding.ID)
	if prev, ok := s.chatNames.Load(key); ok && prev.(string) == want {
		return
	}
	if current, err := s.cfg.Chats.GetChatName(ctx, creds, ChatID(binding.ChatID)); err == nil && current == want {
		s.chatNames.Store(key, want)
		return
	}
	if err := s.cfg.Chats.UpdateChatName(ctx, creds, ChatID(binding.ChatID), want); err != nil {
		s.cfg.Logger.Warn("lark issue chat: status chat name failed", "chat_id", binding.ChatID, "error", err)
		return
	}
	s.chatNames.Store(key, want)
}

// syncTimeSensitive keeps the chat pinned to the top of the message list of
// exactly the members the work waits on, and unpins it once they answer.
func (s *IssueChatService) syncTimeSensitive(ctx context.Context, binding db.ChannelIssueChat, inst Installation, creds InstallationCredentials, waiting []pgtype.UUID) {
	want := map[string]bool{}
	if len(waiting) > 0 {
		rows, err := s.cfg.Store.ListChannelUserBindingsForUsers(ctx, db.ListChannelUserBindingsForUsersParams{
			WorkspaceID: binding.WorkspaceID, ChannelType: channelTypeFeishu,
			UserIds: uniqueUUIDs(waiting), PreferredInstallationID: inst.ID,
		})
		if err != nil {
			s.cfg.Logger.Warn("lark issue chat: resolve waiting members failed", "chat_id", binding.ChatID, "error", err)
			return
		}
		for _, row := range rows {
			if id := safeMentionOpenID(row.ChannelUserID); row.InstallationID == inst.ID && id != "" {
				want[id] = true
			}
		}
	}
	key := uuidString(binding.ID)
	prev := map[string]bool{}
	if v, ok := s.pinnedUsers.Load(key); ok {
		prev = v.(map[string]bool)
	}
	var on, off []string
	for id := range want {
		if !prev[id] {
			on = append(on, id)
		}
	}
	for id := range prev {
		if !want[id] {
			off = append(off, id)
		}
	}
	sort.Strings(on)
	sort.Strings(off)
	next := map[string]bool{}
	for id := range prev {
		next[id] = true
	}
	if len(on) > 0 {
		if err := s.cfg.Chats.SetChatTimeSensitive(ctx, creds, ChatID(binding.ChatID), true, on); err != nil {
			s.cfg.Logger.Warn("lark issue chat: pin chat for waiting members failed", "chat_id", binding.ChatID, "error", err)
		} else {
			for _, id := range on {
				next[id] = true
			}
		}
	}
	if len(off) > 0 {
		if err := s.cfg.Chats.SetChatTimeSensitive(ctx, creds, ChatID(binding.ChatID), false, off); err != nil {
			s.cfg.Logger.Warn("lark issue chat: unpin chat failed", "chat_id", binding.ChatID, "error", err)
		} else {
			for _, id := range off {
				delete(next, id)
			}
		}
	}
	s.pinnedUsers.Store(key, next)
}

// handleProgressComment refreshes a root chat's progress card when its issue
// gets a comment: an agent asking a person, or that person answering, changes
// what the work is waiting on.
func (s *IssueChatService) handleProgressComment(e events.Event) {
	// Unlike the mirror, this also runs for replies written in the chat.
	payload, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	var c commentEvent
	raw, err := json.Marshal(payload["comment"])
	if err != nil || json.Unmarshal(raw, &c) != nil {
		return
	}
	issueID, err := util.ParseUUID(c.IssueID)
	if err != nil {
		return
	}
	s.dispatch(func() {
		ctx, cancel := context.WithTimeout(context.Background(), issueChatSyncTimeout)
		defer cancel()
		binding, err := s.cfg.Store.GetChannelIssueChatByIssue(ctx, db.GetChannelIssueChatByIssueParams{IssueID: issueID, ChannelType: channelTypeFeishu})
		if err != nil || binding.RootIssueID != binding.IssueID {
			return
		}
		if err := s.refreshProgress(ctx, issueID); err != nil {
			s.cfg.Logger.Warn("lark issue chat: progress card refresh failed", "issue_id", c.IssueID, "error", err)
		}
	})
}

// handleMetadataEvent refreshes the progress card when an issue's links change.
func (s *IssueChatService) handleMetadataEvent(e events.Event) {
	payload, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	raw, _ := payload["issue_id"].(string)
	issueID, err := util.ParseUUID(raw)
	if err != nil {
		return
	}
	s.dispatch(func() {
		ctx, cancel := context.WithTimeout(context.Background(), issueChatSyncTimeout)
		defer cancel()
		if err := s.refreshProgress(ctx, issueID); err != nil {
			s.cfg.Logger.Warn("lark issue chat: progress card refresh failed", "issue_id", raw, "error", err)
		}
	})
}
