package lark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// artifactTabs uses the same data-driven titles and URLs as the progress card.
func artifactTabs(metadata, artifacts []byte) []ChatURLTab {
	links := issueArtifactLinks(metadata, artifacts)
	var tabs []ChatURLTab
	seen := make(map[string]bool)
	for _, link := range links {
		name := chatTabName(link.label, 30)
		for n := 2; seen[name]; n++ {
			suffix := fmt.Sprintf(" (%d)", n)
			name = chatTabName(link.label, 30-len(suffix)) + suffix
		}
		seen[name] = true
		tabs = append(tabs, ChatURLTab{Key: link.key, Name: name, URL: link.url})
	}
	return tabs
}

func chatTabName(title string, maxBytes int) string {
	for len(title) > maxBytes {
		_, size := utf8.DecodeLastRuneInString(title)
		title = title[:len(title)-size]
	}
	return title
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
	// Reload ownership after acquiring the chat lock: a preceding refresh may
	// have created tabs after this binding snapshot was read.
	binding, err := s.cfg.Store.GetChannelIssueChatByIssue(ctx, db.GetChannelIssueChatByIssueParams{IssueID: binding.IssueID, ChannelType: channelTypeFeishu})
	if err != nil {
		return err
	}
	if binding.Status != "active" {
		return nil
	}
	issue, err := s.cfg.Store.GetIssue(ctx, binding.IssueID)
	if err != nil {
		return err
	}
	tabs := artifactTabs(issue.Metadata, issue.Artifacts)
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
	owned := make(map[string]string)
	if err := json.Unmarshal(binding.ArtifactTabs, &owned); err != nil {
		return err
	}
	if owned == nil {
		owned = make(map[string]string)
	}
	for i := range tabs {
		tabs[i].ID = owned[tabs[i].Key]
	}
	syncErr := s.cfg.Chats.SyncChatURLTabs(ctx, creds, ChatID(binding.ChatID), tabs)
	changed := false
	for _, tab := range tabs {
		if owned[tab.Key] != tab.ID {
			if tab.ID == "" {
				delete(owned, tab.Key)
			} else {
				owned[tab.Key] = tab.ID
			}
			changed = true
		}
	}
	if !changed {
		return syncErr
	}
	raw, err := json.Marshal(owned)
	if err != nil {
		return errors.Join(syncErr, err)
	}
	return errors.Join(syncErr, s.cfg.Store.SetChannelIssueArtifactTabs(ctx, db.SetChannelIssueArtifactTabsParams{
		ID: binding.ID, WorkspaceID: binding.WorkspaceID, ArtifactTabs: raw,
	}))
}
