package lark

// Issue <-> Feishu group sync, inbound half (docs/lark-issue-group-sync.md).
//
// A group chat bound to an issue is a second surface of that issue's comment
// timeline. Every message a person posts there becomes a comment authored by
// them; a native @ of an agent's bot becomes a Multica agent mention, so it
// triggers the agent exactly like the same comment written on the web.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

const (
	issueMessageInbound     = "inbound"
	issueMessageOutbound    = "outbound"
	issueMessageKindComment = "comment"
	issueMessageKindIntro   = "intro"
	// issueMessageKindThreadRoot is the card a sub-issue topic hangs under.
	issueMessageKindThreadRoot = "thread_root"
)

// CommentChannelOriginKey is the comment:created payload key that marks a
// comment written from a channel message. The outbound sync skips such
// comments so they are never mirrored back into the chat they came from.
const CommentChannelOriginKey = "channel_origin"

// CommentChannelOrigin identifies the platform message a comment came from.
type CommentChannelOrigin struct {
	ChannelType    string `json:"channel_type"`
	InstallationID string `json:"installation_id"`
	MessageID      string `json:"message_id"`
}

// ChannelIssueCommentParams is a member comment written from a claimed inbound
// queue entry. BindingID, LeaseToken and Origin identify that entry for the
// writer's atomic source mapping and acknowledgement.
type ChannelIssueCommentParams struct {
	WorkspaceID     pgtype.UUID
	IssueID         pgtype.UUID
	AuthorUserID    pgtype.UUID
	ParentCommentID pgtype.UUID
	Content         string
	Origin          CommentChannelOrigin
	BindingID       pgtype.UUID
	LeaseToken      pgtype.UUID
	Attachments     []ChannelIssueCommentAttachment
	SyncState       []byte
}

type ChannelIssueCommentAttachment struct {
	ID pgtype.UUID
	channel.MediaRef
}

// IssueCommentWriter creates a member comment through the same path as the
// comment HTTP endpoint, mention triggers included. The handler layer
// implements it; ErrIssueCommentRejected marks a refusal requiring a user
// to restore access or correct the request before an explicit retry.
type IssueCommentWriter interface {
	CreateChannelIssueComment(ctx context.Context, p ChannelIssueCommentParams) (ChannelIssueCommentResult, error)
	RetryChannelIssueComment(ctx context.Context, job db.ChannelIssueCommentPending) (ChannelIssueCommentResult, error)
}

// ErrIssueCommentRejected is returned by an IssueCommentWriter for a
// refusal of the comment.
var ErrIssueCommentRejected = errors.New("lark: issue comment rejected")

// FeishuIssueChatRouter implements engine.IssueChatRouter for Feishu.
type FeishuIssueChatRouter struct {
	store  *ChannelStore
	writer IssueCommentWriter
	api    APIClient
	creds  CredentialsResolver
	logger *slog.Logger
	media  engine.MediaResolver
}

var _ engine.IssueChatRouter = (*FeishuIssueChatRouter)(nil)

// NewFeishuIssueChatRouter builds the router. api and creds may be nil; the
// only verified union_id matches can then resolve.
func NewFeishuIssueChatRouter(store *ChannelStore, writer IssueCommentWriter, api APIClient, creds CredentialsResolver, logger *slog.Logger) *FeishuIssueChatRouter {
	if logger == nil {
		logger = slog.Default()
	}
	return &FeishuIssueChatRouter{
		store: store, writer: writer, api: api, creds: creds, logger: logger,
	}
}

// SetMediaResolver wires the existing Feishu resource downloader before Run.
func (r *FeishuIssueChatRouter) SetMediaResolver(media engine.MediaResolver) {
	r.media = media
}

// LookupIssueChat routes a message inside a topic by the topic's root
// message (a sub-issue topic), falling back to the chat-level binding so a
// topic someone opens under an ordinary message still belongs to the
// chat's issue.
func (r *FeishuIssueChatRouter) LookupIssueChat(ctx context.Context, msg channel.InboundMessage) (engine.IssueChatRoute, bool, error) {
	if msg.Source.ChatID == "" {
		return engine.IssueChatRoute{}, false, nil
	}
	threadRoot := ""
	if msg.Source.ThreadID != "" && msg.ReplyTo != nil {
		threadRoot = msg.ReplyTo.RootID
	}
	row, err := r.route(ctx, msg.Source.ChatID, threadRoot)
	if errors.Is(err, pgx.ErrNoRows) && threadRoot != "" {
		row, err = r.route(ctx, msg.Source.ChatID, "")
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return engine.IssueChatRoute{}, false, nil
	}
	if err != nil {
		return engine.IssueChatRoute{}, false, err
	}
	return engine.IssueChatRoute{
		BindingID:             row.ID,
		WorkspaceID:           row.WorkspaceID,
		IssueID:               row.IssueID,
		StewardInstallationID: row.StewardInstallationID,
	}, true, nil
}

func (r *FeishuIssueChatRouter) route(ctx context.Context, chatID, threadRoot string) (db.ChannelIssueChat, error) {
	return r.store.GetChannelIssueChatByRoute(ctx, db.GetChannelIssueChatByRouteParams{
		ChannelType:         channelTypeFeishu,
		ChatID:              chatID,
		ThreadRootMessageID: threadRoot,
	})
}

// SentByBot treats an undecodable message as bot-sent: dropping a message is
// recoverable, looping a mirrored comment back into the issue is not.
func (r *FeishuIssueChatRouter) SentByBot(msg channel.InboundMessage) bool {
	lm, err := larkMsgFromRaw(msg)
	return err != nil || lm.SenderType == "app"
}

func (r *FeishuIssueChatRouter) RecordIssueComment(ctx context.Context, inst engine.ResolvedInstallation, identity engine.ResolvedIdentity, route engine.IssueChatRoute, msg channel.InboundMessage) error {
	if msg.MessageID == "" {
		return nil
	}
	// A crash between the comment and the dedup mark replays the event; the
	// message map written with the comment makes the replay a no-op.
	if _, err := r.store.GetChannelIssueMessageByChannelID(ctx, msg.MessageID); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return r.store.QueueChannelIssueComment(ctx, db.QueueChannelIssueCommentParams{
		ID: dbid.NewV7(), WorkspaceID: route.WorkspaceID, IssueID: route.IssueID,
		BindingID: route.BindingID, InstallationID: inst.ID, AuthorUserID: identity.UserID,
		ChannelMessageID: msg.MessageID, Message: msg.Raw,
	})
}

// parentComment maps a quote reply (or a reply inside a topic rooted at a
// mirrored comment) to that comment, so the reply lands in its thread.
func (r *FeishuIssueChatRouter) parentComment(ctx context.Context, issueID pgtype.UUID, lm InboundMessage) (pgtype.UUID, error) {
	for _, id := range []string{lm.ParentID, lm.RootID} {
		if id == "" {
			continue
		}
		row, err := r.store.GetChannelIssueMessageByChannelID(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return pgtype.UUID{}, err
		}
		if row.IssueID == issueID && row.CommentID.Valid {
			return row.CommentID, nil
		}
	}
	return pgtype.UUID{}, nil
}

// renderComment turns the user's message into comment Markdown. Mentions are
// resolved by union_id, the only identity consistent across the bots in one
// chat: a bot becomes an agent mention, a bound person a member mention.
func (r *FeishuIssueChatRouter) renderComment(ctx context.Context, workspaceID pgtype.UUID, lm InboundMessage, text string) (string, error) {
	text = strings.ReplaceAll(text, "mention://", "mention-disabled://")
	if (lm.MessageType != "text" && lm.MessageType != "post") || len(lm.Mentions) == 0 {
		return text, nil
	}
	insts, err := r.store.ListLarkInstallationsByWorkspace(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	agentByBot := make(map[string]pgtype.UUID, len(insts))
	for _, inst := range insts {
		if InstallationStatus(inst.Status) == InstallationActive && inst.BotUnionID.String != "" {
			agentByBot[inst.BotUnionID.String] = inst.AgentID
		}
	}
	// open_id belongs to the receiving app; it must never be compared with
	// another installation's open_id or with a display name.
	var receiving Installation
	for _, inst := range insts {
		if inst.AppID == lm.AppID {
			receiving = inst
			break
		}
	}
	replacements := make(map[string]string, len(lm.Mentions))
	unknownBots, lookedUpBots := false, false
	resolveUnknownBots := func() error {
		if lookedUpBots {
			return nil
		}
		lookedUpBots = true
		for _, inst := range insts {
			if InstallationStatus(inst.Status) != InstallationActive || inst.BotUnionID.String != "" {
				continue
			}
			if r.api == nil || r.creds == nil {
				unknownBots = true
				continue
			}
			creds, err := installationCredentialsFor(inst, r.creds)
			if err != nil {
				return err
			}
			info, err := r.api.GetBotInfo(ctx, creds)
			if err != nil || info.UnionID == "" {
				unknownBots = true
				continue
			}
			agentByBot[info.UnionID] = inst.AgentID
		}
		return nil
	}
	for _, m := range lm.Mentions {
		if m.Key == "" {
			continue
		}
		label := mentionLabel(m.Name)
		if label == "" {
			label = strings.TrimPrefix(m.Key, "@")
		}
		replacements[m.Key] = "@" + label
		if m.Key == "@_all" || m.OpenID == "all" {
			continue
		}
		unionID := m.UnionID
		if unionID == "" && m.OpenID != "" && receiving.ID.Valid && r.creds != nil {
			if resolver, ok := r.api.(MentionIdentityResolver); ok {
				creds, err := installationCredentialsFor(receiving, r.creds)
				if err != nil {
					return "", err
				}
				unionID, err = resolver.ResolveMentionUnionID(ctx, creds, m.OpenID)
				if err != nil {
					return "", fmt.Errorf("%w: %v", ErrMentionIdentityUnresolved, err)
				}
			}
		}
		if unionID == "" {
			return "", ErrMentionIdentityUnresolved
		}
		if agentID, ok := agentByBot[unionID]; ok {
			replacements[m.Key] = multicaMention(label, "agent", agentID)
			continue
		}
		binding, err := r.store.FindChannelUserBindingByUnionID(ctx, db.FindChannelUserBindingByUnionIDParams{
			WorkspaceID: workspaceID, ChannelType: channelTypeFeishu, UnionID: unionID,
		})
		if err == nil {
			replacements[m.Key] = multicaMention(label, "member", binding.MulticaUserID)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		} else {
			if err := resolveUnknownBots(); err != nil {
				return "", err
			}
			if agentID, ok := agentByBot[unionID]; ok {
				replacements[m.Key] = multicaMention(label, "agent", agentID)
			} else if unknownBots {
				return "", ErrMentionIdentityUnresolved
			}
		}
	}
	return replaceMentionKeys(text, replacements), nil
}

var ErrMentionIdentityUnresolved = errors.New("lark: mention identity could not be verified")

// MentionIdentityResolver resolves IDs only in the app that received them.
type MentionIdentityResolver interface {
	ResolveMentionUnionID(context.Context, InstallationCredentials, string) (string, error)
}

func multicaMention(label, kind string, id pgtype.UUID) string {
	return fmt.Sprintf("[@%s](mention://%s/%s)", label, kind, uuidString(id))
}

// mentionLabel keeps a display name from breaking the mention link syntax.
func mentionLabel(name string) string {
	return strings.TrimSpace(strings.NewReplacer("\n", " ", "\r", " ", "](", " (").Replace(name))
}

// replaceMentionKeys substitutes Lark's @_user_N placeholders in one pass,
// longest key first so @_user_10 is never mistaken for @_user_1.
func replaceMentionKeys(text string, replacements map[string]string) string {
	if len(replacements) == 0 {
		return text
	}
	keys := make([]string, 0, len(replacements))
	for k := range replacements {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	var out strings.Builder
	for i := 0; i < len(text); {
		matched := ""
		for _, k := range keys {
			if strings.HasPrefix(text[i:], k) {
				matched = k
				break
			}
		}
		if matched == "" {
			out.WriteByte(text[i])
			i++
			continue
		}
		out.WriteString(replacements[matched])
		i += len(matched)
	}
	return out.String()
}
