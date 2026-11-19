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
	if job.State != "feedback" {
		var lm InboundMessage
		_ = json.Unmarshal(job.Message, &lm)
		if job.State == "pending" && (lm.AddressedToBot || job.Attempt > 0) && !isIssueRetryCommand(lm) {
			if id, err := r.writeIssueReceipt(ctx, job, "已收到，正在处理请求。", false); err == nil {
				job.ReceiptMessageID = id
			} else {
				r.logger.Warn("lark issue request acknowledgement failed", "message_id", job.ChannelMessageID, "error", err)
			}
		}
		result, processErr := r.processIssueComment(ctx, job)
		code := ""
		if processErr != nil {
			code = "processing_failed"
			current, readErr := r.store.GetChannelIssueCommentRequest(ctx, db.GetChannelIssueCommentRequestParams{ChannelMessageID: job.ChannelMessageID, WorkspaceID: job.WorkspaceID})
			if readErr != nil {
				return true, readErr
			}
			if current.State == "dispatching" {
				code = "dispatch_unknown"
			}
			var failure *issueRequestFailure
			switch {
			case errors.As(processErr, &failure):
				code = failure.code
			case errors.Is(processErr, ErrMentionIdentityUnresolved):
				code = "identity_unresolved"
			case errors.Is(processErr, ErrIssueCommentRejected):
				code = "request_rejected"
			}
			r.logger.Warn("lark issue request failed", "message_id", job.ChannelMessageID, "code", code, "error", processErr)
		}
		if processErr == nil && isIssueRetryCommand(lm) {
			return true, nil
		}
		if code == "" && len(result.Outcomes) == 0 && lm.AddressedToBot {
			code = "no_trigger"
		}
		if code == "" && len(result.Outcomes) == 0 {
			_, err = r.store.FinishChannelIssueComment(ctx, db.FinishChannelIssueCommentParams{
				ChannelMessageID: job.ChannelMessageID, WorkspaceID: job.WorkspaceID, LeaseToken: job.LeaseToken,
			})
			return true, err
		}
		// Ordinary conversation failures do not generate bot chatter.
		if code != "" && len(lm.Mentions) == 0 && !lm.AddressedToBot && !isIssueRetryCommand(lm) {
			if code == "request_rejected" || code == "dispatch_unknown" {
				_, err = r.store.FinishChannelIssueComment(ctx, db.FinishChannelIssueCommentParams{
					ChannelMessageID: job.ChannelMessageID, WorkspaceID: job.WorkspaceID, LeaseToken: job.LeaseToken,
				})
				return true, err
			}
			if err := r.store.RetryChannelIssueComment(ctx, db.RetryChannelIssueCommentParams{
				ChannelMessageID: job.ChannelMessageID, WorkspaceID: job.WorkspaceID, LeaseToken: job.LeaseToken,
			}); err != nil {
				return true, err
			}
			return true, processErr
		}
		outcomes, _ := json.Marshal(result.Outcomes)
		if result.Outcomes == nil {
			outcomes = job.DispatchOutcomes
			if len(outcomes) == 0 {
				outcomes = []byte("[]")
			}
		}
		count, err := r.store.SetChannelIssueCommentOutcome(ctx, db.SetChannelIssueCommentOutcomeParams{
			MessageID: job.ChannelMessageID, WorkspaceID: job.WorkspaceID, LeaseToken: job.LeaseToken,
			Outcomes: outcomes, FailureCode: code,
		})
		if err != nil {
			return true, err
		}
		if count != 1 {
			return true, errors.New("issue request lease lost")
		}
		job, err = r.store.GetChannelIssueCommentRequest(ctx, db.GetChannelIssueCommentRequestParams{ChannelMessageID: job.ChannelMessageID, WorkspaceID: job.WorkspaceID})
		if err != nil {
			return true, err
		}
	}
	if err := r.deliverIssueReceipt(ctx, job); err != nil {
		finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if retryErr := r.store.RetryChannelIssueComment(finalizeCtx, db.RetryChannelIssueCommentParams{
			ChannelMessageID: job.ChannelMessageID, WorkspaceID: job.WorkspaceID, LeaseToken: job.LeaseToken,
		}); retryErr != nil {
			return true, retryErr
		}
		return true, err
	}
	return true, nil
}

func (r *FeishuIssueChatRouter) processIssueComment(ctx context.Context, job db.ChannelIssueCommentPending) (ChannelIssueCommentResult, error) {
	ctx, cancel := context.WithTimeout(ctx, engine.DefaultMediaTimeout+10*time.Second)
	defer cancel()
	if job.State == "dispatching" {
		return ChannelIssueCommentResult{}, &issueRequestFailure{code: "dispatch_unknown"}
	}
	inst, err := r.store.GetLarkInstallationInWorkspace(ctx, GetInstallationInWorkspaceParams{
		ID: job.InstallationID, WorkspaceID: job.WorkspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && InstallationStatus(inst.Status) != InstallationActive) {
		return ChannelIssueCommentResult{}, ErrIssueCommentRejected
	}
	if err != nil {
		return ChannelIssueCommentResult{}, err
	}
	member, err := r.store.IsWorkspaceMember(ctx, job.WorkspaceID, job.AuthorUserID)
	if err != nil {
		return ChannelIssueCommentResult{}, err
	}
	if !member {
		return ChannelIssueCommentResult{}, ErrIssueCommentRejected
	}
	issue, err := r.store.GetIssue(ctx, job.IssueID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && issue.WorkspaceID != job.WorkspaceID) {
		return ChannelIssueCommentResult{}, ErrIssueCommentRejected
	}
	if err != nil {
		return ChannelIssueCommentResult{}, err
	}
	var lm InboundMessage
	if err := json.Unmarshal(job.Message, &lm); err != nil {
		return ChannelIssueCommentResult{}, err
	}
	if isIssueRetryCommand(lm) {
		return ChannelIssueCommentResult{}, r.restartIssueRequest(ctx, job, lm)
	}
	if job.CommentID.Valid {
		count, err := r.store.StartChannelIssueCommentRetry(ctx, db.StartChannelIssueCommentRetryParams{
			ChannelMessageID: job.ChannelMessageID, WorkspaceID: job.WorkspaceID, LeaseToken: job.LeaseToken,
		})
		if err != nil {
			return ChannelIssueCommentResult{}, err
		}
		if count != 1 {
			return ChannelIssueCommentResult{}, errors.New("issue request lease lost")
		}
		return r.writer.RetryChannelIssueComment(ctx, job)
	}
	if _, err := r.store.GetChannelIssueMessageByChannelID(ctx, job.ChannelMessageID); err == nil {
		return ChannelIssueCommentResult{}, &issueRequestFailure{code: "dispatch_unknown"}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return ChannelIssueCommentResult{}, err
	}
	resolvedInst := engine.ResolvedInstallation{ID: inst.ID, WorkspaceID: job.WorkspaceID, Platform: inst}
	if lm.MessageType == larkMsgTypeMergeForward {
		forward, err := r.PrepareForwardedComment(ctx, resolvedInst, lm, nil)
		if err != nil {
			return ChannelIssueCommentResult{}, err
		}
		parent, err := r.parentComment(ctx, job.IssueID, lm)
		if err != nil {
			return ChannelIssueCommentResult{}, err
		}
		result, err := r.writer.CreateChannelIssueComment(ctx, ChannelIssueCommentParams{
			WorkspaceID: job.WorkspaceID, IssueID: job.IssueID, AuthorUserID: job.AuthorUserID,
			ParentCommentID: parent, Content: forward.Content, BindingID: job.BindingID, LeaseToken: job.LeaseToken,
			Attachments: forward.Attachments, SyncState: forward.SyncState,
			Origin: CommentChannelOrigin{ChannelType: channelTypeFeishu, InstallationID: uuidString(inst.ID), MessageID: job.ChannelMessageID},
		})
		return result, err
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
		return ChannelIssueCommentResult{}, err
	}
	if lm.AddressedToBot && !strings.Contains(body, "mention://agent/") {
		return ChannelIssueCommentResult{}, ErrMentionIdentityUnresolved
	}
	if strings.TrimSpace(body) == "" {
		return ChannelIssueCommentResult{}, ErrIssueCommentRejected
	}
	parent, err := r.parentComment(ctx, job.IssueID, lm)
	if err != nil {
		return ChannelIssueCommentResult{}, err
	}
	result, err := r.writer.CreateChannelIssueComment(ctx, ChannelIssueCommentParams{
		WorkspaceID: job.WorkspaceID, IssueID: job.IssueID, AuthorUserID: job.AuthorUserID,
		ParentCommentID: parent, Content: body, BindingID: job.BindingID, LeaseToken: job.LeaseToken,
		Attachments: attachments,
		Origin:      CommentChannelOrigin{ChannelType: channelTypeFeishu, InstallationID: uuidString(inst.ID), MessageID: job.ChannelMessageID},
	})
	return result, err
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
