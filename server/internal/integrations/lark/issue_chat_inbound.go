package lark

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/channelmedia"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Run processes durable inbound messages off the connector ACK path. A binding
// is serialized by the queue claim, so a reply can resolve its parent even when
// that parent contains a slow attachment. Other bindings can progress in parallel.
func (r *FeishuIssueChatRouter) Run(ctx context.Context) {
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for ctx.Err() == nil {
				processed, err := r.processNextIssueComment(ctx)
				if err != nil {
					r.logger.Warn("lark issue chat: inbound sync failed", "error", err)
				}
				if processed && err == nil {
					continue
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	workers.Wait()
}

func (r *FeishuIssueChatRouter) processNextIssueComment(ctx context.Context) (bool, error) {
	job, err := r.store.ClaimChannelIssueComment(ctx, dbid.NewV7())
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = r.processIssueComment(ctx, job)
	if err == nil {
		return true, nil
	}
	finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if errors.Is(err, ErrIssueCommentRejected) {
		_, finishErr := r.store.FinishChannelIssueComment(finalizeCtx, db.FinishChannelIssueCommentParams{
			ChannelMessageID: job.ChannelMessageID, WorkspaceID: job.WorkspaceID, LeaseToken: job.LeaseToken,
		})
		return true, finishErr
	}
	if retryErr := r.store.RetryChannelIssueComment(finalizeCtx, db.RetryChannelIssueCommentParams{
		ChannelMessageID: job.ChannelMessageID, WorkspaceID: job.WorkspaceID, LeaseToken: job.LeaseToken,
	}); retryErr != nil {
		return true, retryErr
	}
	return true, err
}

func (r *FeishuIssueChatRouter) processIssueComment(ctx context.Context, job db.ChannelIssueCommentPending) error {
	ctx, cancel := context.WithTimeout(ctx, engine.DefaultMediaTimeout+10*time.Second)
	defer cancel()
	if _, err := r.store.GetChannelIssueMessageByChannelID(ctx, job.ChannelMessageID); err == nil {
		return ErrIssueCommentRejected
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	inst, err := r.store.GetLarkInstallationInWorkspace(ctx, GetInstallationInWorkspaceParams{
		ID: job.InstallationID, WorkspaceID: job.WorkspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && InstallationStatus(inst.Status) != InstallationActive) {
		return ErrIssueCommentRejected
	}
	if err != nil {
		return err
	}
	member, err := r.store.IsWorkspaceMember(ctx, job.WorkspaceID, job.AuthorUserID)
	if err != nil {
		return err
	}
	if !member {
		return ErrIssueCommentRejected
	}
	issue, err := r.store.GetIssue(ctx, job.IssueID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && issue.WorkspaceID != job.WorkspaceID) {
		return ErrIssueCommentRejected
	}
	if err != nil {
		return err
	}
	var lm InboundMessage
	if err := json.Unmarshal(job.Message, &lm); err != nil {
		return err
	}
	resolvedInst := engine.ResolvedInstallation{ID: inst.ID, WorkspaceID: job.WorkspaceID, Platform: inst}
	if lm.MessageType == larkMsgTypeMergeForward {
		forward, err := r.PrepareForwardedComment(ctx, resolvedInst, lm, nil)
		if err != nil {
			return err
		}
		parent, err := r.parentComment(ctx, job.IssueID, lm)
		if err != nil {
			return err
		}
		_, err = r.writer.CreateChannelIssueComment(ctx, ChannelIssueCommentParams{
			WorkspaceID: job.WorkspaceID, IssueID: job.IssueID, AuthorUserID: job.AuthorUserID,
			ParentCommentID: parent, Content: forward.Content, BindingID: job.BindingID, LeaseToken: job.LeaseToken,
			Attachments: forward.Attachments, SyncState: forward.SyncState,
			Origin: CommentChannelOrigin{ChannelType: channelTypeFeishu, InstallationID: uuidString(inst.ID), MessageID: job.ChannelMessageID},
		})
		return err
	}
	msg := channelMessageFromLark(lm)
	// A fresh correlation ID keeps retried uploads independent of abandoned
	// intents already owned by the existing media reconciler.
	mediaID := dbid.NewV7()
	if r.media != nil && r.media.HasMedia(msg) {
		mediaCtx, mediaCancel := context.WithTimeout(ctx, engine.DefaultMediaTimeout)
		msg = r.media.ResolveMedia(mediaCtx, resolvedInst, engine.ResolvedIdentity{UserID: job.AuthorUserID}, pgtype.UUID{}, mediaID, msg)
		mediaCancel()
	}
	attachments := make([]ChannelIssueCommentAttachment, 0, len(msg.MediaRefs))
	markdownByKey := make(map[string]string, len(msg.MediaRefs))
	for _, ref := range msg.MediaRefs {
		id := dbid.NewV7()
		attachments = append(attachments, ChannelIssueCommentAttachment{ID: id, MediaRef: ref})
		markdownByKey[ref.StorageKey] = channelmedia.Link(uuidString(id), ref.Filename, ref.Type == channel.MsgTypeImage)
	}
	body := issueCommentMediaMarkdown(lm, resolvedInst, mediaID, markdownByKey)
	body, err = r.renderComment(ctx, job.WorkspaceID, lm, body)
	if err != nil {
		return err
	}
	if strings.TrimSpace(body) == "" {
		return ErrIssueCommentRejected
	}
	parent, err := r.parentComment(ctx, job.IssueID, lm)
	if err != nil {
		return err
	}
	_, err = r.writer.CreateChannelIssueComment(ctx, ChannelIssueCommentParams{
		WorkspaceID: job.WorkspaceID, IssueID: job.IssueID, AuthorUserID: job.AuthorUserID,
		ParentCommentID: parent, Content: body, BindingID: job.BindingID, LeaseToken: job.LeaseToken,
		Attachments: attachments,
		Origin:      CommentChannelOrigin{ChannelType: channelTypeFeishu, InstallationID: uuidString(inst.ID), MessageID: job.ChannelMessageID},
	})
	return err
}

// Render actual media spans rather than replacing bracketed strings: user text
// may itself contain "[Image]", and a failed first download must not shift the
// next image into that position. Repeated spans share the same attachment.
func issueCommentMediaMarkdown(lm InboundMessage, inst engine.ResolvedInstallation, mediaID pgtype.UUID, markdownByKey map[string]string) string {
	resources := mediaResourcesFromMessage(lm)
	byResource := make(map[string]string, len(resources))
	for _, res := range resources {
		markdown := markdownByKey[mediaObjectKey(inst, mediaID, res)]
		if markdown == "" {
			markdown = "[Attachment unavailable]"
		}
		byResource[res.fetchType+"\x00"+res.key] = markdown
	}
	if lm.MessageType != "post" {
		if len(resources) > 0 {
			res := resources[0]
			return byResource[res.fetchType+"\x00"+res.key]
		}
		return flattenContent(lm.MessageType, lm.Content)
	}
	doc, err := decodePostContent(lm.Content)
	if err != nil {
		return flattenContent(lm.MessageType, lm.Content)
	}
	for _, para := range doc.Content {
		for i, span := range para {
			key := ""
			if span.Tag == "img" {
				key = "image\x00" + span.ImageKey
			} else if span.Tag == "file" || span.Tag == "media" {
				key = "file\x00" + span.FileKey
			}
			if markdown, ok := byResource[key]; ok {
				para[i] = larkPostSpan{Tag: "text", Text: markdown}
			}
		}
	}
	return flattenPostDocument(doc)
}
