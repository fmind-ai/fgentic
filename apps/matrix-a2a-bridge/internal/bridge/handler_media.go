package bridge

import (
	"context"
	"fmt"
	"io"
	"strings"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
)

// Bounds on the non-file content folded into a reply so a chatty agent cannot bloat a room message.
const (
	maxDataBlocks     = 8
	maxDataBlockChars = 4000
	maxLinks          = 8
	maxLinkChars      = 2000
)

// collectInboundMedia resolves the file a mention references — the media event itself, or the media
// event it replies to — applies the media policy and the remote allowMedia opt-in, downloads allowed
// files, and returns them as A2A inbound parts (#115). ok is false when the delegation must be
// refused: a referenced file failed policy (disallowed, oversized, encrypted, or a remote target
// without allowMedia), so the caller posts a bounded notice and never reaches A2A. When no file is
// referenced it returns (nil, 0, true) so an ordinary text mention proceeds unchanged.
func (b *Bridge) collectInboundMedia(
	ctx context.Context,
	intent *appservice.IntentAPI,
	evt *event.Event,
	ref *AgentRef,
) (files []a2aclient.InboundFile, rejected int, ok bool) {
	if !b.media.enabled() {
		return nil, 0, true
	}
	candidates := b.inboundMediaCandidates(ctx, intent, evt)
	if len(candidates) == 0 {
		return nil, 0, true
	}
	// A remote target receives file bytes only when its mapping explicitly opts in. Refuse before any
	// download so the room's bytes never even leave the cluster toward an un-opted-in partner.
	if ref.Target().IsRemote() && !ref.AllowsMedia() {
		return nil, len(candidates), false
	}
	budget := b.media.newBudget()
	for _, c := range candidates {
		// Encrypted attachments cannot be handled: the bridge deliberately does not wire the crypto
		// package (ADR 0008), so it cannot decrypt them and must fail closed rather than forward ciphertext.
		if c.File != nil {
			return nil, len(candidates), false
		}
		mime := ""
		var declared int64
		if c.Info != nil {
			mime = c.Info.MimeType
			declared = int64(c.Info.Size)
		}
		if !b.media.precheck(mime, declared) {
			return nil, len(candidates), false
		}
		mxc, err := c.URL.Parse()
		if err != nil {
			return nil, len(candidates), false
		}
		data, ok := b.downloadBounded(ctx, intent, evt.RoomID, mxc)
		if !ok {
			return nil, len(candidates), false
		}
		if _, admit := budget.admit(mime, int64(len(data))); !admit {
			return nil, len(candidates), false
		}
		if sniffsAsHTML(data) {
			// Bytes sniff as HTML despite a benign declared type — refuse the whole delegation.
			return nil, len(candidates), false
		}
		files = append(files, a2aclient.InboundFile{
			Name:     sanitizeFilename(c.GetFileName(), mime),
			MIMEType: normalizeMIME(mime),
			Bytes:    data,
		})
	}
	return files, 0, true
}

// downloadBounded fetches media through an io.LimitReader capped at the per-file byte limit + 1, so a
// blob whose real size exceeds MEDIA_MAX_BYTES is never fully buffered — the declared info.size is
// attacker-controlled and must not be trusted to size the read (#115). It rejects (ok=false) on a
// transport error, an advertised Content-Length over the cap, or an over-cap actual read. The extra
// byte lets a file exactly at the cap through while still detecting overflow.
func (b *Bridge) downloadBounded(ctx context.Context, intent *appservice.IntentAPI, roomID id.RoomID, mxc id.ContentURI) ([]byte, bool) {
	resp, err := intent.Download(ctx, mxc)
	if err != nil {
		b.log.Warn("download inbound media failed", "room", roomID, "err", err)
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ContentLength > b.media.maxBytes {
		b.log.Warn("inbound media advertises size over cap", "room", roomID, "content_length", resp.ContentLength)
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, b.media.maxBytes+1))
	if err != nil {
		b.log.Warn("read inbound media failed", "room", roomID, "err", err)
		return nil, false
	}
	if int64(len(data)) > b.media.maxBytes {
		b.log.Warn("inbound media exceeds cap on read", "room", roomID)
		return nil, false
	}
	return data, true
}

// inboundMediaCandidates returns the media message contents a mention references: the event itself
// when it is a media message, otherwise the single event it replies to when that is media. It never
// walks a whole thread — exactly one referenced file, resolved with at most one extra fetch.
func (b *Bridge) inboundMediaCandidates(
	ctx context.Context,
	intent *appservice.IntentAPI,
	evt *event.Event,
) []*event.MessageEventContent {
	msg := evt.Content.AsMessage()
	if msg == nil {
		return nil
	}
	if msg.MsgType.IsMedia() {
		return []*event.MessageEventContent{msg}
	}
	replyTo := msg.RelatesTo.GetReplyTo()
	if replyTo == "" {
		return nil
	}
	parent, err := intent.GetEvent(ctx, evt.RoomID, replyTo)
	if err != nil {
		b.log.Warn("fetch replied-to event for media", "room", evt.RoomID, "event", replyTo, "err", err)
		return nil
	}
	if err := parent.Content.ParseRaw(parent.Type); err != nil {
		return nil
	}
	pm := parent.Content.AsMessage()
	if pm == nil || !pm.MsgType.IsMedia() {
		return nil
	}
	return []*event.MessageEventContent{pm}
}

// deliverReply posts an agent's terminal reply into the room: the text summary as the ghost's
// m.notice (a fresh reply when placeholder is empty, otherwise an edit of the working placeholder),
// any structured-data and URL parts folded into that text, allowed artifact files as separate ghost
// media events, and a bounded content-free note for anything the media policy withheld (#115). It
// returns the reply event ID and the content-free media counts for the audit.
func (b *Bridge) deliverReply(
	ctx context.Context,
	intent *appservice.IntentAPI,
	evt *event.Event,
	placeholder id.EventID,
	localpart string,
	ref *AgentRef,
	res a2aclient.Result,
) (replyID id.EventID, out, rejected int) {
	text := res.Text
	text += renderDataBlocks(res.Data)
	text += renderLinks(res.Links)

	// Upload the allowed files first so the count of anything withheld is known before the text posts.
	uploads, rejects := b.uploadReplyFiles(ctx, intent, localpart, ref, res.Files)
	text += withheldNotice(rejects)

	// A file-only reply carries no summary text; give the notice a short caption rather than the
	// "returned no content" fallback, which would be wrong when attachments follow.
	if strings.TrimSpace(text) == "" {
		if len(uploads) > 0 {
			text = "📎 attached."
		} else {
			text = emptyReplyText
		}
	}

	if placeholder == "" {
		replyID = b.postReply(ctx, intent, evt, text)
	} else {
		b.editReply(ctx, intent, evt.RoomID, placeholder, text)
		replyID = placeholder
	}
	for _, u := range uploads {
		b.postMediaFile(ctx, intent, evt, u)
	}
	for _, n := range rejects {
		rejected += n
	}
	return replyID, len(uploads), rejected
}

// uploadedFile is one artifact file that passed policy and was stored in the content repository,
// ready to post as a Matrix media event.
type uploadedFile struct {
	name string
	mime string
	size int
	uri  id.ContentURIString
}

// uploadReplyFiles gates the agent's artifact files by the media policy (and, for a remote target,
// its allowMedia opt-in), uploads the survivors to the content repository as the ghost, and returns
// them alongside a content-free tally of what was withheld and why.
func (b *Bridge) uploadReplyFiles(
	ctx context.Context,
	intent *appservice.IntentAPI,
	localpart string,
	ref *AgentRef,
	files []a2aclient.ResultFile,
) ([]uploadedFile, map[mediaReject]int) {
	rejects := map[mediaReject]int{}
	if len(files) == 0 {
		return nil, rejects
	}
	// A remote agent's bytes reach the room only when the mapping opted in — the boundary is closed to
	// files in both directions by default.
	if ref.Target().IsRemote() && !ref.AllowsMedia() {
		for range files {
			rejects[mediaRejectRemoteOptOut]++
		}
		return nil, rejects
	}
	budget := b.media.newBudget()
	var uploads []uploadedFile
	for _, f := range files {
		if reason, ok := budget.admit(f.MIMEType, int64(len(f.Bytes))); !ok {
			rejects[reason]++
			continue
		}
		if sniffsAsHTML(f.Bytes) {
			// Content sniffs as HTML despite a benign declared type — withhold rather than post it.
			rejects[mediaRejectDisallowedType]++
			continue
		}
		name := sanitizeFilename(f.Name, f.MIMEType)
		resp, err := intent.UploadBytesWithName(ctx, f.Bytes, normalizeMIME(f.MIMEType), name)
		if err != nil {
			b.log.Error("upload agent artifact", "ghost", localpart, "err", err)
			rejects[mediaRejectDisallowedType]++ // best-effort content-free: surfaced as withheld, never dropped silently
			continue
		}
		uploads = append(uploads, uploadedFile{
			name: name, mime: normalizeMIME(f.MIMEType), size: len(f.Bytes), uri: id.ContentURIString(resp.ContentURI.String()),
		})
	}
	return uploads, rejects
}

// postMediaFile posts one uploaded artifact as the ghost, as an m.image for images and m.file
// otherwise, replying to the original mention and carrying the m.automated marker so automation still
// treats agent output as untrusted (docs/security.md).
func (b *Bridge) postMediaFile(ctx context.Context, intent *appservice.IntentAPI, evt *event.Event, u uploadedFile) {
	msgType := event.MsgFile
	if strings.HasPrefix(u.mime, "image/") {
		msgType = event.MsgImage
	}
	content := &event.MessageEventContent{
		MsgType:  msgType,
		Body:     u.name,
		FileName: u.name,
		URL:      u.uri,
		Info:     &event.FileInfo{MimeType: u.mime, Size: u.size},
	}
	content.SetReply(evt)
	if _, err := intent.SendMessageEvent(ctx, evt.RoomID, event.EventMessage, automatedContent(content)); err != nil {
		b.log.Error("post agent artifact", "room", evt.RoomID, "err", err)
	}
}

// renderDataBlocks folds structured-data parts into fenced JSON code blocks, bounded in count and
// size so a large data payload cannot bloat the reply.
func renderDataBlocks(data []string) string {
	var b strings.Builder
	for i, block := range data {
		if i >= maxDataBlocks {
			break
		}
		if len([]rune(block)) > maxDataBlockChars {
			block = string([]rune(block)[:maxDataBlockChars]) + " …(truncated)"
		}
		fmt.Fprintf(&b, "\n\n```json\n%s\n```", block)
	}
	return b.String()
}

// renderLinks surfaces URL parts as labeled, untrusted, bridge-never-fetched links. URLs are shown as
// plain text (not markdown auto-links) and bounded so an agent cannot flood or overflow the reply.
func renderLinks(links []a2aclient.ResultLink) string {
	var b strings.Builder
	for i, link := range links {
		if i >= maxLinks {
			break
		}
		url := sanitizeInline(link.URL, maxLinkChars)
		if url == "" {
			continue
		}
		if label := sanitizeInline(link.Label, maxFilenameRunes); label != "" {
			fmt.Fprintf(&b, "\n\n🔗 untrusted link (not fetched) — %s: %s", label, url)
		} else {
			fmt.Fprintf(&b, "\n\n🔗 untrusted link (not fetched): %s", url)
		}
	}
	return b.String()
}

// sanitizeInline strips control characters and bounds the length of a single-line untrusted string
// bound for a room message, so it cannot inject newlines or overflow the reply.
func sanitizeInline(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if runes := []rune(s); len(runes) > max {
		s = string(runes[:max]) + "…"
	}
	return s
}
