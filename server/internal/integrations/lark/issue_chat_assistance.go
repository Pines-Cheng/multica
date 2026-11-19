package lark

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Keep the opening request intact, including wrapped lines and inline links.
// Background paragraphs and lists remain available in the original comment.
func assistanceCardPreview(markdown string) string {
	source := []byte(markdown)
	first := goldmark.DefaultParser().Parse(text.NewReader(source)).FirstChild()
	paragraph, ok := first.(*ast.Paragraph)
	if !ok || paragraph.Lines().Len() == 0 {
		return markdown
	}
	lines := paragraph.Lines()
	return strings.TrimSpace(string(source[lines.At(0).Start:lines.At(lines.Len()-1).Stop]))
}

func (s *IssueChatService) issueCommentLink(ctx context.Context, issueID, commentID string) (string, string) {
	id, err := util.ParseUUID(issueID)
	if err != nil {
		return "", ""
	}
	issue, err := s.cfg.Store.GetIssue(ctx, id)
	if err != nil {
		return "", ""
	}
	identifier, link := s.issueIdentity(ctx, issue)
	if link != "" {
		link += "#comment-" + commentID
	}
	return identifier, link
}

// Assistance is independent of thread resolution and task status. Only an
// explicit request enters this list, and only an explicit close removes it.
func (s *IssueChatService) assistanceLinks(ctx context.Context, issue db.Issue) (map[pgtype.UUID]string, error) {
	binding, err := s.cfg.Store.GetChannelIssueChatByIssue(ctx, db.GetChannelIssueChatByIssueParams{IssueID: issue.ID, ChannelType: channelTypeFeishu})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	requests, err := s.cfg.Store.ListOpenAssistanceForChannelChat(ctx, db.ListOpenAssistanceForChannelChatParams{RootIssueID: issue.ID, WorkspaceID: issue.WorkspaceID, ChannelType: channelTypeFeishu})
	if err != nil || len(requests) == 0 {
		return nil, err
	}
	steward, ok, err := s.currentSteward(ctx, &binding)
	if err != nil || !ok {
		return nil, err
	}
	creds, err := installationCredentialsFor(steward, s.cfg.Credentials)
	if err != nil {
		return nil, err
	}
	links := make(map[pgtype.UUID]string)
	for _, request := range requests {
		if _, exists := links[request.IssueID]; exists {
			continue
		}
		_, link := s.issueCommentLink(ctx, uuidString(request.IssueID), uuidString(request.ID))
		if request.ChannelMessageID != "" {
			messages, getErr := s.cfg.API.GetMessage(ctx, creds, request.ChannelMessageID)
			if getErr != nil {
				s.cfg.Logger.Warn("lark issue chat: assistance topic link unavailable", "comment_id", uuidString(request.ID), "error", getErr)
			}
			for _, message := range messages {
				if message.MessageID == request.ChannelMessageID {
					if topicLink := chatMessageAppLink(RegionOrDefault(steward.Region), binding.ChatID, message); topicLink != "" {
						link = topicLink
					}
				}
			}
		}
		label := "待协助"
		if validIssueCardURL(link) {
			label = issueCardMarkdownLink(issueCardLink{label: label, url: link})
		}
		links[request.IssueID] = label
	}
	return links, nil
}

func chatMessageAppLink(region Region, chatID string, message LarkMessage) string {
	if n, err := strconv.ParseInt(message.ThreadMessagePosition, 10, 64); err == nil && n >= 0 && message.ThreadID != "" {
		host := "https://applink.feishu.cn"
		if region == RegionLark {
			host = "https://applink.larksuite.com"
		}
		q := url.Values{"openthreadid": {message.ThreadID}, "openchatid": {chatID}, "open_thread_id": {message.ThreadID}, "open_chat_id": {chatID}, "thread_position": {message.ThreadMessagePosition}}
		return host + "/client/thread/open?" + q.Encode()
	}
	if n, err := strconv.ParseInt(message.MessagePosition, 10, 64); err == nil && n >= 0 {
		return chatAppLink(region, chatID) + "&position=" + url.QueryEscape(message.MessagePosition)
	}
	return ""
}
