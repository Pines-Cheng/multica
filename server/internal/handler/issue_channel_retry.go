package handler

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/lark"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func channelCommentResult(id pgtype.UUID, outcomes []CommentTriggerOutcome) lark.ChannelIssueCommentResult {
	result := lark.ChannelIssueCommentResult{ID: id}
	for _, o := range outcomes {
		result.Outcomes = append(result.Outcomes, lark.ChannelCommentOutcome{
			TargetType: o.TargetType, TargetID: o.TargetID, Status: string(o.Status), ReasonCode: string(o.ReasonCode),
		})
	}
	return result
}

// RetryChannelIssueComment re-admits only previously blocked explicit targets.
// The worker owns the durable request lease; the original author and comment
// remain the source of authorization and attribution.
func (h *Handler) RetryChannelIssueComment(ctx context.Context, job db.ChannelIssueCommentPending) (lark.ChannelIssueCommentResult, error) {
	empty := lark.ChannelIssueCommentResult{}
	current, err := h.Queries.GetChannelIssueCommentRequest(ctx, db.GetChannelIssueCommentRequestParams{
		ChannelMessageID: job.ChannelMessageID, WorkspaceID: job.WorkspaceID,
	})
	if err != nil {
		return empty, err
	}
	if current.LeaseToken != job.LeaseToken || current.State != "dispatching" {
		return empty, errors.New("channel request is not claimed for dispatch")
	}
	job = current
	_, err = h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: job.AuthorUserID, WorkspaceID: job.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, lark.ErrIssueCommentRejected
	}
	if err != nil {
		return empty, err
	}
	issue, err := h.Queries.GetIssue(ctx, job.IssueID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && issue.WorkspaceID != job.WorkspaceID) {
		return empty, lark.ErrIssueCommentRejected
	}
	if err != nil {
		return empty, err
	}
	comment, err := h.Queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{ID: job.CommentID, WorkspaceID: job.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, lark.ErrChannelCommentChanged
	}
	if err != nil {
		return empty, err
	}
	if comment.DeletedAt.Valid || comment.IssueID != job.IssueID || comment.AuthorType != "member" || comment.AuthorID != job.AuthorUserID || comment.Content != job.CommentContent {
		return empty, lark.ErrChannelCommentChanged
	}
	result := lark.ChannelIssueCommentResult{ID: comment.ID}
	if err := json.Unmarshal(job.DispatchOutcomes, &result.Outcomes); err != nil {
		return empty, err
	}
	blocked := map[string]bool{}
	for _, o := range result.Outcomes {
		if o.TargetType == "agent" && o.Status == "blocked" {
			blocked[o.TargetID] = true
		}
	}
	var parent *db.Comment
	if comment.ParentID.Valid {
		row, err := h.Queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{ID: comment.ParentID, WorkspaceID: job.WorkspaceID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return empty, err
		}
		if err == nil {
			parent = &row
		}
	}
	author := uuidToString(job.AuthorUserID)
	triggers, targets := h.computeCommentAgentTriggers(ctx, issue, comment.Content, parent, "member", author, commentTriggerComputeOptions{
		ExcludeTriggerCommentID: comment.ID, OriginatorUserID: author,
	})
	var retryTriggers []commentAgentTrigger
	for _, trigger := range triggers {
		if blocked[uuidToString(trigger.Agent.ID)] {
			retryTriggers = append(retryTriggers, trigger)
		}
	}
	var retryTargets []commentMentionTarget
	for _, target := range targets {
		if target.TargetType == "agent" && blocked[target.TargetID] {
			retryTargets = append(retryTargets, target)
		}
	}
	h.noteBlockedRuntimeTargets(ctx, issue, retryTargets)
	outcomes := commentTriggerOutcomes(retryTargets, h.enqueueCommentAgentTriggers(ctx, issue, comment.ID, retryTriggers))
	h.noteCommentInvocationDenied(ctx, issue, comment, outcomes)
	for _, o := range channelCommentResult(comment.ID, outcomes).Outcomes {
		for i := range result.Outcomes {
			if result.Outcomes[i].TargetID == o.TargetID && result.Outcomes[i].TargetType == o.TargetType {
				result.Outcomes[i] = o
			}
		}
	}
	raw, err := json.Marshal(result.Outcomes)
	if err != nil {
		return empty, err
	}
	n, err := h.Queries.SetChannelIssueCommentOutcome(ctx, db.SetChannelIssueCommentOutcomeParams{
		MessageID: job.ChannelMessageID, WorkspaceID: job.WorkspaceID, LeaseToken: job.LeaseToken, Outcomes: raw,
	})
	if err != nil {
		return empty, err
	}
	if n != 1 {
		return empty, errors.New("channel request dispatch lease lost")
	}
	return result, nil
}
