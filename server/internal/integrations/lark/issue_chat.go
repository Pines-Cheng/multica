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
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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

// ChannelIssueCommentParams is a member comment written from a channel.
type ChannelIssueCommentParams struct {
	WorkspaceID     pgtype.UUID
	IssueID         pgtype.UUID
	AuthorUserID    pgtype.UUID
	ParentCommentID pgtype.UUID
	Content         string
	Origin          CommentChannelOrigin
}

// IssueCommentWriter creates a member comment through the same path as the
// comment HTTP endpoint, mention triggers included. The handler layer
// implements it; ErrIssueCommentRejected marks a refusal that a retry of the
// same message cannot fix (issue gone, no access, invalid content).
type IssueCommentWriter interface {
	CreateChannelIssueComment(ctx context.Context, p ChannelIssueCommentParams) (pgtype.UUID, error)
}

// ErrIssueCommentRejected is returned by an IssueCommentWriter for a
// permanent refusal of the comment.
var ErrIssueCommentRejected = errors.New("lark: issue comment rejected")

// FeishuIssueChatRouter implements engine.IssueChatRouter for Feishu.
type FeishuIssueChatRouter struct {
	store  *ChannelStore
	writer IssueCommentWriter
	api    APIClient
	creds  CredentialsResolver
	logger *slog.Logger

	namesMu sync.Mutex
	names   map[pgtype.UUID]cachedBotName
}

// cachedBotName is a bot display name read from Lark, kept briefly so a busy
// chat does not call /bot/v3/info for every mention.
type cachedBotName struct {
	name    string
	fetched time.Time
}

const botNameTTL = 10 * time.Minute

var _ engine.IssueChatRouter = (*FeishuIssueChatRouter)(nil)

// NewFeishuIssueChatRouter builds the router. api and creds may be nil; the
// bot-name mention fallback is then off and only union_id matches resolve.
func NewFeishuIssueChatRouter(store *ChannelStore, writer IssueCommentWriter, api APIClient, creds CredentialsResolver, logger *slog.Logger) *FeishuIssueChatRouter {
	if logger == nil {
		logger = slog.Default()
	}
	return &FeishuIssueChatRouter{
		store: store, writer: writer, api: api, creds: creds, logger: logger,
		names: make(map[pgtype.UUID]cachedBotName),
	}
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
	lm, err := larkMsgFromRaw(msg)
	if err != nil {
		return err
	}
	body, err := r.renderComment(ctx, route.WorkspaceID, lm)
	if err != nil {
		return err
	}
	if strings.TrimSpace(body) == "" {
		return nil
	}
	parent, err := r.parentComment(ctx, route.IssueID, lm)
	if err != nil {
		return err
	}
	commentID, err := r.writer.CreateChannelIssueComment(ctx, ChannelIssueCommentParams{
		WorkspaceID:     route.WorkspaceID,
		IssueID:         route.IssueID,
		AuthorUserID:    identity.UserID,
		ParentCommentID: parent,
		Content:         body,
		Origin: CommentChannelOrigin{
			ChannelType:    channelTypeFeishu,
			InstallationID: uuidString(inst.ID),
			MessageID:      msg.MessageID,
		},
	})
	if errors.Is(err, ErrIssueCommentRejected) {
		r.logger.Info("lark issue chat: comment rejected",
			"issue_id", uuidString(route.IssueID), "message_id", msg.MessageID, "error", err)
		return nil
	}
	if err != nil {
		return err
	}
	return r.store.RecordChannelIssueMessage(ctx, db.RecordChannelIssueMessageParams{
		WorkspaceID:      route.WorkspaceID,
		IssueID:          route.IssueID,
		BindingID:        route.BindingID,
		InstallationID:   inst.ID,
		ChannelMessageID: msg.MessageID,
		Direction:        issueMessageInbound,
		Kind:             issueMessageKindComment,
		CommentID:        commentID,
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
func (r *FeishuIssueChatRouter) renderComment(ctx context.Context, workspaceID pgtype.UUID, lm InboundMessage) (string, error) {
	text := flattenContent(lm.MessageType, lm.Content)
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
	var agentByName map[string]pgtype.UUID
	replacements := make(map[string]string, len(lm.Mentions))
	for _, m := range lm.Mentions {
		if m.Key == "" {
			continue
		}
		label := mentionLabel(m.Name)
		if label == "" {
			label = strings.TrimPrefix(m.Key, "@")
		}
		replacements[m.Key] = "@" + label
		if m.UnionID == "" {
			continue
		}
		if agentID, ok := agentByBot[m.UnionID]; ok {
			replacements[m.Key] = multicaMention(label, "agent", agentID)
			continue
		}
		binding, err := r.store.FindChannelUserBindingByUnionID(ctx, db.FindChannelUserBindingByUnionIDParams{
			WorkspaceID: workspaceID,
			ChannelType: channelTypeFeishu,
			UnionID:     m.UnionID,
		})
		if err == nil {
			replacements[m.Key] = multicaMention(label, "member", binding.MulticaUserID)
			continue
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
		// Bots whose union_id could not be resolved at install (the contact
		// lookup needs a contact range that covers the bot) are matched by
		// their display name. A name shared by two bots matches neither.
		if agentByName == nil {
			agentByName = r.agentsByBotName(ctx, insts, agentByBot)
		}
		if agentID, ok := agentByName[strings.TrimSpace(m.Name)]; ok {
			replacements[m.Key] = multicaMention(label, "agent", agentID)
		}
	}
	return replaceMentionKeys(text, replacements), nil
}

// agentsByBotName maps the display name of every active bot without a known
// union_id to its agent. Ambiguous names are left out.
func (r *FeishuIssueChatRouter) agentsByBotName(ctx context.Context, insts []Installation, known map[string]pgtype.UUID) map[string]pgtype.UUID {
	out := map[string]pgtype.UUID{}
	if r.api == nil || r.creds == nil {
		return out
	}
	ambiguous := map[string]bool{}
	for _, inst := range insts {
		if InstallationStatus(inst.Status) != InstallationActive {
			continue
		}
		if _, ok := known[inst.BotUnionID.String]; ok && inst.BotUnionID.String != "" {
			continue
		}
		name := strings.TrimSpace(r.botName(ctx, inst))
		if name == "" {
			continue
		}
		if _, dup := out[name]; dup || ambiguous[name] {
			delete(out, name)
			ambiguous[name] = true
			continue
		}
		out[name] = inst.AgentID
	}
	return out
}

func (r *FeishuIssueChatRouter) botName(ctx context.Context, inst Installation) string {
	r.namesMu.Lock()
	cached, ok := r.names[inst.ID]
	r.namesMu.Unlock()
	if ok && time.Since(cached.fetched) < botNameTTL {
		return cached.name
	}
	creds, err := installationCredentialsFor(inst, r.creds)
	if err != nil {
		return ""
	}
	info, err := r.api.GetBotInfo(ctx, creds)
	if err != nil {
		r.logger.Warn("lark issue chat: bot name lookup failed", "installation_id", uuidString(inst.ID), "error", err)
		return ""
	}
	r.namesMu.Lock()
	r.names[inst.ID] = cachedBotName{name: info.Name, fetched: time.Now()}
	r.namesMu.Unlock()
	return info.Name
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
