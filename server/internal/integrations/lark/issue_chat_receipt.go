package lark

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ChannelIssueCommentResult preserves admission outcomes across the channel bridge.
type ChannelIssueCommentResult struct {
	ID       pgtype.UUID
	Outcomes []ChannelCommentOutcome
}

type ChannelCommentOutcome struct {
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	Status     string `json:"status"`
	ReasonCode string `json:"reason_code"`
}

type issueRequestFailure struct{ code string }

func (e *issueRequestFailure) Error() string { return e.code }

// ErrChannelCommentChanged prevents replaying a request edited on another surface.
var ErrChannelCommentChanged = &issueRequestFailure{code: "comment_changed"}

func isIssueRetryCommand(lm InboundMessage) bool {
	if lm.MessageType != "text" && lm.MessageType != "post" {
		return false
	}
	body := flattenContent(lm.MessageType, lm.Content)
	for _, mention := range lm.Mentions {
		if mention.Key != "" {
			body = strings.ReplaceAll(body, mention.Key, "")
		}
	}
	return strings.TrimSpace(body) == "/retry"
}

func (r *FeishuIssueChatRouter) restartIssueRequest(ctx context.Context, job db.ChannelIssueCommentPending, lm InboundMessage) error {
	if lm.ParentID == "" {
		return &issueRequestFailure{code: "retry_not_allowed"}
	}
	n, err := r.store.RestartChannelIssueComment(ctx, db.RestartChannelIssueCommentParams{
		OriginalMessageID: lm.ParentID, WorkspaceID: job.WorkspaceID,
		CommandMessageID: job.ChannelMessageID, LeaseToken: job.LeaseToken,
	})
	if err != nil {
		return err
	}
	if n != 1 {
		return &issueRequestFailure{code: "retry_not_allowed"}
	}
	return nil
}

func (r *FeishuIssueChatRouter) deliverIssueReceipt(ctx context.Context, job db.ChannelIssueCommentPending) error {
	text, blocked := issueReceiptText(job)
	if _, err := r.writeIssueReceipt(ctx, job, text, true); err != nil {
		return err
	}
	state := "completed"
	if blocked {
		state = "blocked"
	}
	n, err := r.store.CompleteChannelIssueComment(ctx, db.CompleteChannelIssueCommentParams{
		MessageID: job.ChannelMessageID, WorkspaceID: job.WorkspaceID, LeaseToken: job.LeaseToken, State: state,
	})
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("issue receipt lease lost")
	}
	return nil
}

func (r *FeishuIssueChatRouter) writeIssueReceipt(ctx context.Context, job db.ChannelIssueCommentPending, text string, final bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if r.api == nil || r.creds == nil {
		return "", errors.New("issue receipt transport unavailable")
	}
	inst, err := r.store.GetLarkInstallationInWorkspace(ctx, GetInstallationInWorkspaceParams{ID: job.InstallationID, WorkspaceID: job.WorkspaceID})
	if err != nil {
		return "", err
	}
	creds, err := installationCredentialsFor(inst, r.creds)
	if err != nil {
		return "", err
	}
	var lm InboundMessage
	if err := json.Unmarshal(job.Message, &lm); err != nil {
		return "", err
	}
	card, err := markdownCardContent(text, "智能体请求状态")
	if err != nil {
		return "", err
	}
	if job.ReceiptMessageID == "" {
		id, err := r.api.SendInteractiveCard(ctx, SendCardParams{
			InstallationID: creds, ChatID: lm.ChatID, CardJSON: card,
			ReplyTarget: inboundReplyTarget(lm), DeduplicationID: uuidString(job.ID),
		})
		if err != nil {
			return "", err
		}
		if id == "" {
			return "", errors.New("issue receipt has no message ID")
		}
		n, err := r.store.SetChannelIssueCommentReceipt(ctx, db.SetChannelIssueCommentReceiptParams{
			MessageID: job.ChannelMessageID, WorkspaceID: job.WorkspaceID, LeaseToken: job.LeaseToken, ReceiptMessageID: id,
		})
		if err != nil {
			return "", err
		}
		if n != 1 {
			return "", errors.New("issue receipt lease lost")
		}
		job.ReceiptMessageID = id
	}
	// A replayed send may return the earlier acknowledgement via uuid dedup.
	// Always patch the final result after the receipt ID is durable.
	if final || job.Attempt > 0 {
		if err := r.api.PatchInteractiveCard(ctx, PatchCardParams{
			InstallationID: creds, LarkCardMessageID: job.ReceiptMessageID, CardJSON: card,
		}); err != nil {
			return "", err
		}
	}
	return job.ReceiptMessageID, nil
}

var receiptMention = regexp.MustCompile(`\[@([^\]]+)\]\(mention://agent/([^()]+)\)`)

func issueReceiptText(job db.ChannelIssueCommentPending) (string, bool) {
	labels := map[string]string{}
	for _, match := range receiptMention.FindAllStringSubmatch(job.CommentContent, -1) {
		labels[match[2]] = match[1]
	}
	var outcomes []ChannelCommentOutcome
	_ = json.Unmarshal(job.DispatchOutcomes, &outcomes)
	var lines []string
	blocked := job.FailureCode != "" && job.FailureCode != "no_trigger"
	if job.FailureCode != "" {
		lines = append(lines, issueFailureText(job.FailureCode))
	}
	for _, outcome := range outcomes {
		label := labels[outcome.TargetID]
		if label == "" {
			label = "智能体"
		}
		status := ""
		switch outcome.Status {
		case "queued":
			status = "已排队，尚未开始执行。"
		case "coalesced":
			status = "已合入现有排队请求。"
		case "deferred":
			status = "已接收，等待后续调度。"
		case "steered":
			status = "已送入当前执行。"
		default:
			blocked = true
			status = issueFailureText(outcome.ReasonCode)
		}
		lines = append(lines, "@"+label+"："+status)
	}
	if blocked && job.FailureCode != "dispatch_unknown" && job.FailureCode != "comment_changed" && job.FailureCode != "retry_not_allowed" {
		lines = append(lines, "修复后，由原发起人回复原消息发送 `/retry`。只重试未接收的目标。")
	}
	return strings.Join(lines, "\n"), blocked
}

func issueFailureText(code string) string {
	switch code {
	case "no_trigger":
		return "评论已保存，未产生执行请求。请在正文中直接 @ 智能体。"
	case "identity_unresolved":
		return "未能核实 @ 对象，尚未派发。请管理员检查机器人身份和通讯录权限。"
	case "invocation_not_allowed":
		return "未接收：没有调用权限，请联系管理员授权。"
	case "runtime_access_denied":
		return "未接收：没有所选运行时的使用权限，请运行时所有者授权。"
	case "runtime_offline":
		return "未接收：运行时离线，请先连接运行时。"
	case "agent_runtime_required":
		return "未接收：请先为智能体选择运行时。"
	case "runtime_unusable":
		return "未接收：运行时上的智能体程序不可用，请修复安装。"
	case "runtime_profile_missing":
		return "未接收：运行时缺少所需配置，请补齐配置。"
	case "target_unavailable":
		return "未接收：目标不可用，请检查智能体连接和状态。"
	case "request_rejected":
		return "未接收：任务、连接或成员权限已变化，请检查访问权限。"
	case "attribution_blocked":
		return "未接收：无法确定执行负责人，请管理员检查归属配置。"
	case "dispatch_unknown":
		return "评论已保存，但无法确认派发结果。请在任务中检查执行记录，避免重复执行。"
	case "comment_changed":
		return "原评论已修改或删除，不能重试。请发送新请求。"
	case "retry_not_allowed":
		return "无法重试：请由原发起人回复被拒绝的原消息发送 `/retry`。已接收或正在处理的请求不会重复执行。"
	default:
		return "未能完成请求，请稍后重试。"
	}
}
