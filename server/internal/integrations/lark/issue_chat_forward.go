package lark

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/channelmedia"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// ForwardedCommentState preserves the source tree and successfully bound
// attachments, so retrying a partial sync never re-downloads successful files.
// It is stored only on the inbound source mapping, not in public API responses.
type ForwardedCommentState struct {
	Message    InboundMessage
	Root       ForwardedCommentMessage
	Media      map[string]ChannelIssueCommentAttachment
	Content    string
	Incomplete bool
}

type ForwardedCommentMessage struct {
	Message  LarkMessage
	Children []ForwardedCommentMessage
	Loaded   bool
	Error    string
	Sender   string
}

type ForwardedCommentResult struct {
	Content     string
	Attachments []ChannelIssueCommentAttachment
	SyncState   []byte
}

// PrepareForwardedComment runs on the durable worker (or an explicit retry),
// never on the WebSocket ACK path. Historical mentions remain plain text.
func (r *FeishuIssueChatRouter) PrepareForwardedComment(ctx context.Context, inst engine.ResolvedInstallation, lm InboundMessage, previous []byte) (ForwardedCommentResult, error) {
	// Leave the caller's remaining time for the atomic comment/attachment write.
	ctx, cancel := context.WithTimeout(ctx, engine.DefaultMediaTimeout)
	defer cancel()
	state := ForwardedCommentState{Message: lm, Root: ForwardedCommentMessage{Message: LarkMessage{MessageID: lm.MessageID, MessageType: larkMsgTypeMergeForward}}, Media: map[string]ChannelIssueCommentAttachment{}}
	if len(previous) > 0 {
		if err := json.Unmarshal(previous, &state); err != nil {
			return ForwardedCommentResult{}, err
		}
		if state.Message.MessageID != lm.MessageID {
			return ForwardedCommentResult{}, errors.New("forwarded comment source mismatch")
		}
	}
	if state.Media == nil {
		state.Media = map[string]ChannelIssueCommentAttachment{}
	}
	installation, ok := inst.Platform.(Installation)
	if !ok {
		return ForwardedCommentResult{}, errors.New("forwarded comment installation unavailable")
	}
	creds, err := installationCredentialsFor(installation, r.creds)
	if err != nil {
		return ForwardedCommentResult{}, err
	}
	count := 0
	r.loadForwardedMessages(ctx, creds, &state.Root, 0, &count, map[string]bool{})
	ids := []string{}
	seenSenders := map[string]bool{}
	walkForwarded(&state.Root, func(n *ForwardedCommentMessage) {
		if n.Message.SenderID != "" && n.Sender == "" && n.Message.SenderType != "app" && !seenSenders[n.Message.SenderID] {
			ids = append(ids, n.Message.SenderID)
			seenSenders[n.Message.SenderID] = true
		}
	})
	for start := 0; r.api != nil && start < len(ids); start += larkBatchGetUsersMaxIDs {
		end := min(start+larkBatchGetUsersMaxIDs, len(ids))
		names, nameErr := r.api.BatchGetUsers(ctx, creds, ids[start:end])
		if nameErr == nil {
			walkForwarded(&state.Root, func(n *ForwardedCommentMessage) {
				if name := names[n.Message.SenderID]; name != "" {
					n.Sender = name
				}
			})
		}
	}
	result := ForwardedCommentResult{}
	mediaID := dbid.NewV7()
	links := map[string]string{}
	failures := map[string]string{}
	state.Incomplete = false
	walkForwarded(&state.Root, func(n *ForwardedCommentMessage) {
		if n.Error != "" {
			state.Incomplete = true
		}
		if n.Message.Deleted {
			return
		}
		child := inboundForwardedMessage(n.Message)
		for _, res := range mediaResourcesFromMessage(child) {
			sourceKey := fmt.Sprintf("%x", sha256.Sum256([]byte(res.messageID+"\x00"+res.fetchType+"\x00"+res.key)))
			if saved, ok := state.Media[sourceKey]; ok {
				links[mediaObjectKey(inst, mediaID, res)] = channelmedia.Link(uuidString(saved.ID), saved.Filename, saved.Type == channel.MsgTypeImage)
				continue
			}
			// Resolve one source resource at a time. A mixed post may already have
			// successful attachments from a prior attempt; those must not be re-uploaded.
			payload, _ := json.Marshal(map[string]any{"image_key": res.key, "file_key": res.key, "file_name": res.filename, "mime_type": res.mimeType, "size_bytes": res.sizeBytes})
			resourceMessage := InboundMessage{MessageID: child.MessageID, ResourceMessageID: lm.MessageID, MessageType: string(res.kind), Content: string(payload)}
			if res.kind == channel.MsgTypeVideo {
				resourceMessage.MessageType = "media"
			}
			resolved := channelMessageFromLark(resourceMessage)
			if r.media != nil && ctx.Err() == nil {
				resolved = r.media.ResolveMedia(ctx, inst, engine.ResolvedIdentity{}, mediaID, mediaID, resolved)
			}
			storageKey := mediaObjectKey(inst, mediaID, res)
			if len(resolved.MediaRefs) == 0 {
				reason := resolved.MediaErrors[storageKey]
				if ctx.Err() != nil {
					reason = "Sync timed out; retry sync"
				}
				if reason == "" {
					reason = "Attachment sync is unavailable"
				}
				label := res.filename
				if label == "" {
					label = string(res.kind)
				}
				failures[storageKey] = "[" + label + ": " + reason + "]"
				state.Incomplete = true
				continue
			}
			attachment := ChannelIssueCommentAttachment{ID: dbid.NewV7(), MediaRef: resolved.MediaRefs[0]}
			state.Media[sourceKey] = attachment
			result.Attachments = append(result.Attachments, attachment)
			links[storageKey] = channelmedia.Link(uuidString(attachment.ID), attachment.Filename, attachment.Type == channel.MsgTypeImage)
		}
	})
	result.Content = renderForwardedComment(&state.Root, func(m LarkMessage) string {
		child := inboundForwardedMessage(m)
		// Use the same span-aware renderer as ordinary comments, preserving image
		// positions even when a preceding attachment failed to download.
		perMessage := map[string]string{}
		for _, res := range mediaResourcesFromMessage(child) {
			key := mediaObjectKey(inst, mediaID, res)
			perMessage[key] = links[key]
			if perMessage[key] == "" {
				perMessage[key] = failures[key]
			}
		}
		text := issueCommentMediaMarkdown(child, inst, mediaID, perMessage)
		if text == "" {
			text = "[Unsupported message type: " + m.MessageType + "]"
		}
		text = resolveMentions(text, restMentionsToEvent(m.Mentions), "", "")
		// A copied Multica mention URL is historical too; never let it start a run.
		return strings.ReplaceAll(text, "mention://", "forwarded-mention:")
	})
	if state.Incomplete {
		result.Content += fmt.Sprintf("\n\n[Retry sync](/api/channel-messages/%s/retry)", url.PathEscape(lm.MessageID))
	}
	state.Content = result.Content
	result.SyncState, err = json.Marshal(state)
	return result, err
}

func inboundForwardedMessage(m LarkMessage) InboundMessage {
	if m.MessageType == "interactive" {
		if content, ok := forwardedCardPost(m.Content); ok {
			return InboundMessage{MessageID: m.MessageID, MessageType: "post", Content: content}
		}
	}
	return InboundMessage{MessageID: m.MessageID, MessageType: m.MessageType, Content: m.Content}
}

func walkForwarded(n *ForwardedCommentMessage, fn func(*ForwardedCommentMessage)) {
	fn(n)
	if n.Message.Deleted {
		return
	}
	for i := range n.Children {
		walkForwarded(&n.Children[i], fn)
	}
}

func (r *FeishuIssueChatRouter) loadForwardedMessages(ctx context.Context, creds InstallationCredentials, node *ForwardedCommentMessage, depth int, count *int, path map[string]bool) {
	if node.Message.MessageType != larkMsgTypeMergeForward || node.Message.Deleted {
		return
	}
	id := node.Message.MessageID
	// Bound recursive message trees; hitting a limit remains visible and retryable.
	if depth >= 10 || *count >= 1000 || path[id] {
		node.Error = "Forwarded messages exceed the sync limit"
		return
	}
	path[id] = true
	defer delete(path, id)
	if !node.Loaded {
		if r.api == nil {
			node.Error = "Feishu message reading is unavailable"
			return
		}
		items, err := r.api.GetMessage(ctx, creds, id)
		if err != nil {
			node.Error = "Could not read forwarded messages from Feishu: " + syncReadFailureReason(err)
			return
		}
		node.Children = forwardedChildren(items, id)
		node.Loaded = len(node.Children) > 0
		if !node.Loaded {
			node.Error = "No forwarded messages are available"
			return
		}
	}
	node.Error = ""
	*count += len(node.Children)
	for i := range node.Children {
		r.loadForwardedMessages(ctx, creds, &node.Children[i], depth+1, count, path)
	}
}

func syncReadFailureReason(err error) string {
	switch classifyRecentContextFetchError(err).category {
	case recentContextFailurePermissionDenied:
		return "permission denied"
	case recentContextFailureMessageDeleted:
		return "message deleted or no longer visible"
	case recentContextFailureTimeout:
		return "request timed out"
	case recentContextFailureRateLimited:
		return "Feishu rate limit reached"
	case recentContextFailureTokenExpired:
		return "Feishu authorization expired"
	default:
		return "service unavailable"
	}
}

// Some responses include grandchildren in the same items array. UpperMessageID
// owns the tree; without it, items belong directly to the requested forward.
func forwardedChildren(items []LarkMessage, parent string) []ForwardedCommentMessage {
	children := []ForwardedCommentMessage{}
	seen := map[string]bool{}
	for _, item := range items {
		if item.MessageID == parent || seen[item.MessageID] || (item.UpperMessageID != "" && item.UpperMessageID != parent) {
			continue
		}
		seen[item.MessageID] = true
		child := ForwardedCommentMessage{Message: item, Sender: item.SenderName}
		if item.MessageType == larkMsgTypeMergeForward {
			child.Children = forwardedChildrenForNested(items, item.MessageID, map[string]bool{parent: true})
			child.Loaded = len(child.Children) > 0
		}
		children = append(children, child)
	}
	sort.SliceStable(children, func(i, j int) bool {
		return parseLarkMillis(children[i].Message.CreateTime) < parseLarkMillis(children[j].Message.CreateTime)
	})
	return children
}

func forwardedChildrenForNested(items []LarkMessage, parent string, path map[string]bool) []ForwardedCommentMessage {
	if path[parent] || len(path) >= 10 {
		return nil
	}
	path[parent] = true
	defer delete(path, parent)
	out := []ForwardedCommentMessage{}
	seen := map[string]bool{}
	for _, item := range items {
		if item.UpperMessageID != parent || item.MessageID == parent || seen[item.MessageID] {
			continue
		}
		seen[item.MessageID] = true
		child := ForwardedCommentMessage{Message: item, Sender: item.SenderName}
		if item.MessageType == larkMsgTypeMergeForward {
			child.Children = forwardedChildrenForNested(items, item.MessageID, path)
			child.Loaded = len(child.Children) > 0
		}
		out = append(out, child)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return parseLarkMillis(out[i].Message.CreateTime) < parseLarkMillis(out[j].Message.CreateTime)
	})
	return out
}

func renderForwardedComment(root *ForwardedCommentMessage, render func(LarkMessage) string) string {
	var b strings.Builder
	b.WriteString("<details>\n<summary>Forwarded messages</summary>\n\n")
	if root.Error != "" {
		fmt.Fprintf(&b, "> %s\n\n", root.Error)
	}
	for i := range root.Children {
		child := &root.Children[i]
		sender := child.Sender
		if sender == "" {
			sender = child.Message.SenderID
		}
		if sender == "" {
			sender = "Unknown sender"
		}
		stamp := ""
		if ms, err := strconv.ParseInt(child.Message.CreateTime, 10, 64); err == nil {
			stamp = " · " + time.UnixMilli(ms).UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "**%s%s**\n\n", escapeForwardedSender(sender), stamp)
		switch {
		case child.Message.Deleted:
			b.WriteString("> [Deleted message]\n\n")
		case child.Message.MessageType == larkMsgTypeMergeForward:
			b.WriteString(renderForwardedComment(child, render))
			b.WriteString("\n\n")
		default:
			// Blockquote every line, including blank ones, so code fences, commands and
			// Markdown lists remain visually inside their historical message.
			text := render(child.Message)
			for _, line := range strings.Split(text, "\n") {
				b.WriteString("> " + line + "\n")
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("</details>")
	return b.String()
}

func escapeForwardedSender(sender string) string {
	return strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)", "`", "\\`", "\n", " ", "\r", " ").Replace(html.EscapeString(sender))
}
