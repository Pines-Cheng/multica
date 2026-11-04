package lark

import (
	"context"
	"errors"
	"html"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

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
func (s *IssueChatService) assistanceSummary(ctx context.Context, issue db.Issue) (string, error) {
	binding, err := s.cfg.Store.GetChannelIssueChatByIssue(ctx, db.GetChannelIssueChatByIssueParams{IssueID: issue.ID, ChannelType: channelTypeFeishu})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	requests, err := s.cfg.Store.ListOpenAssistanceForChannelChat(ctx, db.ListOpenAssistanceForChannelChatParams{RootIssueID: issue.ID, WorkspaceID: issue.WorkspaceID, ChannelType: channelTypeFeishu})
	if err != nil || len(requests) == 0 {
		return "", err
	}
	steward, ok, err := s.currentSteward(ctx, &binding)
	if err != nil || !ok {
		return "", err
	}
	creds, err := installationCredentialsFor(steward, s.cfg.Credentials)
	if err != nil {
		return "", err
	}
	lines := []string{"**待协助**"}
	for _, request := range requests {
		c := commentEvent{Content: request.Content, AuthorType: request.AuthorType, AssistanceStatus: request.AssistanceStatus}
		identifier, webLink := s.issueCommentLink(ctx, uuidString(request.IssueID), uuidString(request.ID))
		label := html.EscapeString(attentionNames(c)) + " · " + identifier
		if validIssueCardURL(webLink) {
			label += " · " + issueCardMarkdownLink(issueCardLink{label: "处理请求", url: webLink})
		}
		summary := util.MentionRe.ReplaceAllString(request.Content, "")
		label += "\n" + html.EscapeString(oneLine(strings.TrimSpace(summary), 100))
		if request.ChannelMessageID != "" {
			messages, getErr := s.cfg.API.GetMessage(ctx, creds, request.ChannelMessageID)
			if getErr != nil {
				s.cfg.Logger.Warn("lark issue chat: assistance topic link unavailable", "comment_id", uuidString(request.ID), "error", getErr)
			}
			for _, message := range messages {
				if message.MessageID == request.ChannelMessageID {
					if link := chatMessageAppLink(RegionOrDefault(steward.Region), binding.ChatID, message); link != "" {
						label += " · " + issueCardMarkdownLink(issueCardLink{label: "查看话题", url: link})
					}
				}
			}
		}
		lines = append(lines, label)
	}
	return strings.Join(lines, "\n\n"), nil
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
