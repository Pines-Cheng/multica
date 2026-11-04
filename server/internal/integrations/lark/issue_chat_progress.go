package lark

// The pinned progress card: a root issue chat's opening card is kept up to
// date with where the work stands, so the chat answers "what stage, waiting
// on whom" without scrolling, in place of a notice per status change.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const progressHint = "群内发言与任务评论双向同步；@ 智能体可让它处理。"

const progressOverviewTitle = "任务进展与产物"

// progressCardJSON uses the same current-state template for a chat's pinned
// card and a sub-issue's topic root. Links only navigate; none has a callback.
func (s *IssueChatService) progressCardJSON(ctx context.Context, issue db.Issue, rootIssueID pgtype.UUID, notes ...string) (string, error) {
	identifier, link := s.issueIdentity(ctx, issue)
	topic := rootIssueID.Valid && rootIssueID != issue.ID
	if topic {
		root, err := s.cfg.Store.GetIssue(ctx, rootIssueID)
		if err != nil {
			return "", err
		}
		rootIdentifier, _ := s.issueIdentity(ctx, root)
		path := []string{rootIdentifier}
		if issue.ParentIssueID.Valid && issue.ParentIssueID != rootIssueID {
			parent, err := s.cfg.Store.GetIssue(ctx, issue.ParentIssueID)
			if err != nil {
				return "", err
			}
			parentIdentifier, _ := s.issueIdentity(ctx, parent)
			path = append(path, parentIdentifier)
		}
		identifier = strings.Join(append(path, identifier), " › ")
	}
	entries, err := s.cfg.Store.ListIssueStatusEntries(ctx, db.ListIssueStatusEntriesParams{WorkspaceID: issue.WorkspaceID, IncludeArchived: true})
	if err != nil {
		return "", err
	}
	catalog := make(map[string]db.IssueStatus, len(entries))
	for _, entry := range entries {
		catalog[entry.Key] = entry
	}
	label, template := issueCardStatus(issue.Status, catalog)
	subtitle := []string{identifier, label}
	if name := s.assigneeName(ctx, issue); name != "" {
		subtitle = append(subtitle, name)
	}
	content := issueCardContent{
		title: issue.Title, subtitle: strings.Join(subtitle, " · "),
		summary: cardSummary("", label+" · "+strings.TrimSpace(identifier+" "+issue.Title)), template: template,
		action: issueCardLink{label: "查看任务", url: link}, links: metadataCardLinks(issue.Metadata),
	}
	needsAssistance := false
	if !topic {
		content.title = progressOverviewTitle
		content.subtitle = strings.Join(subtitle[1:], " · ")
		content.sections = append(content.sections, issueCardMarkdownText(issue.Title))
		summary, err := s.assistanceSummary(ctx, issue)
		if err != nil {
			return "", err
		}
		if summary != "" {
			needsAssistance = true
			content.sections = append(content.sections, summary)
		}
	}
	if issue.Status == issuestatus.InReview {
		content.action.label = "查看交付"
	} else if issue.Status == issuestatus.Done {
		content.action.label = "查看结果"
	}
	if topic {
		content.footnotes = append(content.footnotes, "本话题内的发言会同步为该子任务的评论。")
	} else {
		content.footnotes = append(content.footnotes, progressHint)
	}
	updatedAt := issue.UpdatedAt.Time
	children, err := s.cfg.Store.ListChildIssues(ctx, issue.ID)
	if err != nil {
		return "", err
	}
	var current, blocked []string
	if len(children) > 0 {
		for _, c := range children {
			child := childLabel(c)
			if _, childURL := s.issueIdentity(ctx, c); validIssueCardURL(childURL) {
				child = issueCardMarkdownLink(issueCardLink{label: child, url: childURL})
			}
			childStatus, _ := issueCardStatus(c.Status, catalog)
			if c.Status == issuestatus.Blocked {
				blocked = append(blocked, childLabel(c)+" · "+childStatus)
			}
			name := s.assigneeName(ctx, c)
			content.progress = append(content.progress, issueCardProgress{title: child, status: childStatus, assignee: name})
			if c.Status == issuestatus.Todo || issueCardCategory(c.Status, catalog) == issuestatus.CategoryStarted {
				activity := childLabel(c) + " · " + childStatus
				if name != "" {
					activity = name + " · " + activity
					if c.AssigneeType.String == "member" && (c.Status == issuestatus.Todo || c.Status == issuestatus.InReview) {
						activity = "待 " + name + " 处理 \"" + childLabel(c) + "\" · " + childStatus
					}
				}
				current = append(current, activity)
			}
			if c.UpdatedAt.Time.After(updatedAt) {
				updatedAt = c.UpdatedAt.Time
			}
		}
	}
	category := issueCardCategory(issue.Status, catalog)
	if issue.Status == issuestatus.InReview {
		content.sections = append(content.sections, "当前：待验收")
	} else if issue.Status == issuestatus.Blocked {
		content.sections = append(content.sections, "当前："+label)
	} else if len(current) > 0 && category != issuestatus.CategoryDone && category != issuestatus.CategoryClosed {
		content.sections = append(content.sections, "当前："+strings.Join(current, "；"))
	}
	if !topic {
		state := label
		if issue.Status == issuestatus.InReview {
			state = "待验收"
		} else if issue.Status != issuestatus.Blocked && category != issuestatus.CategoryDone && category != issuestatus.CategoryClosed {
			if len(blocked) > 0 {
				state = strings.Join(blocked, "；")
			} else if len(current) > 0 {
				state = strings.Join(current, "；")
			}
		}
		if needsAssistance {
			state = "待协助 · " + state
		}
		content.summary = cardSummary("", progressOverviewTitle+"｜"+state)
	}
	if !updatedAt.IsZero() {
		content.footnotes = append(content.footnotes, "更新于 "+updatedAt.UTC().Format("01-02 15:04 MST"))
	}
	content.sections = append(content.sections, notes...)
	return marshalIssueCard(content)
}

func issueCardStatus(key string, catalog map[string]db.IssueStatus) (label, template string) {
	label = statusLabel(key)
	if !issuestatus.IsBuiltIn(key) && catalog[key].Name != "" {
		label = catalog[key].Name
	}
	category := issueCardCategory(key, catalog)
	switch {
	case key == issuestatus.Blocked:
		return label, "red"
	case key == issuestatus.InReview:
		return label, "orange"
	case category == issuestatus.CategoryDone:
		return label, "green"
	case category == issuestatus.CategoryStarted:
		return label, "blue"
	default:
		return label, "grey"
	}
}

func issueCardCategory(key string, catalog map[string]db.IssueStatus) string {
	if category, builtin := issuestatus.CategoryForBehavior(key); builtin {
		return category
	}
	return catalog[key].Category
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

// metadataCardLinks lists the issue's metadata entries whose value is a web
// link, so an agent can surface Meego, PRD, MR and test links on the card by
// setting issue metadata.
func metadataCardLinks(raw []byte) []issueCardLink {
	var meta map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &meta) != nil {
		return nil
	}
	keys := make([]string, 0, len(meta))
	for k, v := range meta {
		if u, ok := v.(string); ok && strings.TrimSpace(k) != "" && validIssueCardURL(u) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	links := make([]issueCardLink, 0, len(keys))
	for _, k := range keys {
		links = append(links, issueCardLink{label: k, url: meta[k].(string)})
	}
	return links
}

func (s *IssueChatService) issueIdentity(ctx context.Context, issue db.Issue) (identifier, link string) {
	ws, err := s.cfg.Store.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return "", ""
	}
	identifier = service.IssueIdentifier(ws.IssuePrefix, issue.Number)
	return identifier, channel.IssueWebLink(s.cfg.AppURL, ws.Slug, identifier)
}

// refreshProgress updates a topic's own card and its root chat's overview.
func (s *IssueChatService) refreshProgress(ctx context.Context, issueID pgtype.UUID) error {
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
	var topicErr error
	if binding.RootIssueID != binding.IssueID {
		topicErr = s.refreshBoundIssueCard(ctx, binding)
		binding, err = s.cfg.Store.GetChannelIssueChatByIssue(ctx, db.GetChannelIssueChatByIssueParams{IssueID: binding.RootIssueID, ChannelType: channelTypeFeishu})
		if errors.Is(err, pgx.ErrNoRows) {
			return topicErr
		}
		if err != nil {
			return errors.Join(topicErr, err)
		}
	}
	return errors.Join(topicErr, s.refreshBoundIssueCard(ctx, binding))
}

func (s *IssueChatService) refreshBoundIssueCard(ctx context.Context, binding db.ChannelIssueChat) error {
	if binding.Status != "active" {
		return nil
	}
	defer s.lock("progress:" + uuidString(binding.ID))()
	defer s.syncArtifactTabs(ctx, binding)
	topic := binding.RootIssueID != binding.IssueID
	var card db.ChannelIssueMessage
	var err error
	if topic {
		card, err = s.cfg.Store.GetChannelIssueMessageByChannelID(ctx, binding.ThreadRootMessageID)
	} else {
		card, err = s.cfg.Store.GetChannelIssueIntroMessage(ctx, binding.ID)
	}
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
	cardJSON, err := s.progressCardJSON(ctx, issue, binding.RootIssueID)
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
