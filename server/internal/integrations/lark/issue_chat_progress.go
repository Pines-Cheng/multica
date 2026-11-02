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

// progressMarkdown renders the progress card body for a root issue.
func (s *IssueChatService) progressMarkdown(ctx context.Context, issue db.Issue) string {
	var b strings.Builder
	identifier, link := s.issueIdentity(ctx, issue)
	fmt.Fprintf(&b, "**%s %s**\n状态：%s", identifier, issue.Title, statusLabel(issue.Status))
	if name := s.assigneeName(ctx, issue); name != "" {
		fmt.Fprintf(&b, " · 负责人：%s", name)
	}
	children, err := s.cfg.Store.ListChildIssues(ctx, issue.ID)
	if err != nil {
		s.cfg.Logger.Warn("lark issue chat: list sub-issues for progress failed", "issue_id", uuidString(issue.ID), "error", err)
	}
	if now := s.progressNow(ctx, issue, children); now != "" {
		b.WriteString("\n当前：" + now)
	}
	if len(children) > 0 {
		b.WriteString("\n\n**子任务**")
		for _, c := range children {
			fmt.Fprintf(&b, "\n- %s · %s", statusLabel(c.Status), childLabel(c))
			if name := s.assigneeName(ctx, c); name != "" {
				b.WriteString(" · " + name)
			}
		}
	}
	if links := metadataLinks(issue.Metadata); links != "" {
		b.WriteString("\n\n**链接**\n" + links)
	}
	if link != "" {
		fmt.Fprintf(&b, "\n\n[查看任务详情](%s)", link)
	}
	return b.String()
}

// progressNow says what the work is waiting on.
func (s *IssueChatService) progressNow(ctx context.Context, issue db.Issue, children []db.Issue) string {
	switch issue.Status {
	case "done", "cancelled":
		return "已结束"
	case "blocked":
		return "已阻塞"
	}
	var active []string
	for _, c := range children {
		if c.Status != "todo" && c.Status != "in_progress" && c.Status != "blocked" {
			continue
		}
		who := s.assigneeName(ctx, c)
		switch {
		case c.AssigneeType.String == "member" && who != "":
			active = append(active, "待 "+who+" 处理「"+childLabel(c)+"」")
		case who != "":
			active = append(active, who+" 处理中「"+childLabel(c)+"」")
		default:
			active = append(active, "「"+childLabel(c)+"」"+statusLabel(c.Status))
		}
	}
	if len(active) > 0 {
		return strings.Join(active, "；")
	}
	if issue.Status == "in_review" {
		return "待验收"
	}
	return ""
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
		label := strings.NewReplacer("[", "", "]", "").Replace(k)
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
	cardJSON, err := issueChatCardJSON("", "", s.progressMarkdown(ctx, issue)+"\n\n"+progressHint)
	if err != nil {
		return err
	}
	return s.cfg.API.PatchInteractiveCard(ctx, PatchCardParams{InstallationID: creds, LarkCardMessageID: card.ChannelMessageID, CardJSON: cardJSON})
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
