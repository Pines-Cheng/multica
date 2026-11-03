package lark

import (
	"context"
	"strings"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// artifactTabs reserves these names for the root issue's published artifacts.
// Other metadata links remain available on the card.
func artifactTabs(raw []byte) []ChatURLTab {
	links := metadataCardLinks(raw)
	var tabs []ChatURLTab
	for _, name := range []string{"Bits", "MR", "Meego", "PPE", "PRD"} {
		for _, link := range links {
			if strings.EqualFold(strings.TrimSpace(link.label), name) {
				tabs = append(tabs, ChatURLTab{Name: name, URL: link.url})
				break
			}
		}
	}
	return tabs
}

// syncArtifactTabs is best-effort, like the task-detail tab. Topics never
// replace the root issue's artifacts, and tab failures do not block cards.
func (s *IssueChatService) syncArtifactTabs(ctx context.Context, binding db.ChannelIssueChat) {
	if s.cfg.Chats == nil || binding.Status != "active" || binding.IssueID != binding.RootIssueID {
		return
	}
	defer s.lock("tabs:" + binding.ChatID)()
	if err := s.updateArtifactTabs(ctx, binding); err != nil {
		s.cfg.Logger.Warn("lark issue chat: artifact tabs sync failed", "chat_id", binding.ChatID, "error", err)
	}
}

func (s *IssueChatService) updateArtifactTabs(ctx context.Context, binding db.ChannelIssueChat) error {
	issue, err := s.cfg.Store.GetIssue(ctx, binding.IssueID)
	if err != nil {
		return err
	}
	tabs := artifactTabs(issue.Metadata)
	if len(tabs) == 0 {
		return nil
	}
	steward, ok, err := s.currentSteward(ctx, &binding)
	if err != nil || !ok {
		return err
	}
	creds, err := installationCredentialsFor(steward, s.cfg.Credentials)
	if err != nil {
		return err
	}
	return s.cfg.Chats.SyncChatURLTabs(ctx, creds, ChatID(binding.ChatID), tabs)
}
