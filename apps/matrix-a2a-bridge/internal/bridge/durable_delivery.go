package bridge

import (
	"context"
	"fmt"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

// sendDurableNotice sends a reply or placeholder with a caller-persisted transaction ID. A retry
// after a lost Matrix response uses the same ID and therefore resolves to the same logical event.
func (b *Bridge) sendDurableNotice(
	ctx context.Context,
	intent *appservice.IntentAPI,
	evt *event.Event,
	text, transactionID string,
	meta *resultMetadata,
) (id.EventID, error) {
	content := &event.MessageEventContent{MsgType: event.MsgNotice, Body: text}
	content.SetReply(evt)
	response, err := sendMessageEvent(
		ctx,
		intent,
		evt.RoomID,
		event.EventMessage,
		automatedResultContent(content, meta),
		mautrix.ReqSendEvent{TransactionID: transactionID},
	)
	if err != nil {
		return "", fmt.Errorf("send durable Matrix notice in room %s: %w", evt.RoomID, err)
	}
	if response == nil || response.EventID == "" {
		return "", fmt.Errorf("send durable Matrix notice in room %s: empty event ID", evt.RoomID)
	}
	return response.EventID, nil
}

// editDurableNotice replaces a known placeholder using a separate deterministic transaction ID.
func (b *Bridge) editDurableNotice(
	ctx context.Context,
	intent *appservice.IntentAPI,
	roomID id.RoomID,
	target id.EventID,
	text, transactionID string,
	meta *resultMetadata,
) (id.EventID, error) {
	if target == "" {
		return "", fmt.Errorf("edit durable Matrix notice in room %s: empty placeholder", roomID)
	}
	content := &event.MessageEventContent{MsgType: event.MsgNotice, Body: text}
	content.SetEdit(target)
	response, err := sendMessageEvent(
		ctx,
		intent,
		roomID,
		event.EventMessage,
		automatedResultContent(content, meta),
		mautrix.ReqSendEvent{TransactionID: transactionID},
	)
	if err != nil {
		return "", fmt.Errorf("edit durable Matrix notice in room %s: %w", roomID, err)
	}
	if response == nil || response.EventID == "" {
		return "", fmt.Errorf("edit durable Matrix notice in room %s: empty event ID", roomID)
	}
	return response.EventID, nil
}

// deliverDurableReply projects an acknowledged A2A result with deterministic IDs for the primary
// reply/edit and every attachment event. Content uploads may be repeated after a crash, but Matrix
// event creation remains idempotent and no generated event is duplicated in the room timeline.
func (b *Bridge) deliverDurableReply(
	ctx context.Context,
	intent *appservice.IntentAPI,
	evt *event.Event,
	job state.Job,
	ref *AgentRef,
	res a2aclient.Result,
	meta *resultMetadata,
) (primaryEventID id.EventID, edit bool, out, rejected int, err error) {
	text, uploads, rejected := b.prepareReply(ctx, intent, job.GhostLocalpart, ref, res)
	if job.MatrixPlaceholderEventID == "" {
		primaryEventID, err = b.sendDurableNotice(ctx, intent, evt, text, job.MatrixReplyTxnID, meta)
	} else {
		edit = true
		primaryEventID, err = b.editDurableNotice(
			ctx,
			intent,
			evt.RoomID,
			id.EventID(job.MatrixPlaceholderEventID),
			text,
			job.MatrixEditTxnID,
			meta,
		)
	}
	if err != nil {
		return "", edit, 0, rejected, err
	}
	for index, upload := range uploads {
		transactionID := state.MatrixTransactionIDFor(job.JobID, fmt.Sprintf("media-%d", index))
		if err := b.postDurableMediaFile(ctx, intent, evt, upload, transactionID); err != nil {
			return "", edit, index, rejected, err
		}
	}
	return primaryEventID, edit, len(uploads), rejected, nil
}

func (b *Bridge) postDurableMediaFile(
	ctx context.Context,
	intent *appservice.IntentAPI,
	evt *event.Event,
	upload uploadedFile,
	transactionID string,
) error {
	msgType := event.MsgFile
	if strings.HasPrefix(upload.mime, "image/") {
		msgType = event.MsgImage
	}
	content := &event.MessageEventContent{
		MsgType:  msgType,
		Body:     upload.name,
		FileName: upload.name,
		URL:      upload.uri,
		Info:     &event.FileInfo{MimeType: upload.mime, Size: upload.size},
	}
	content.SetReply(evt)
	response, err := sendMessageEvent(
		ctx,
		intent,
		evt.RoomID,
		event.EventMessage,
		automatedContent(content),
		mautrix.ReqSendEvent{TransactionID: transactionID},
	)
	if err != nil {
		return fmt.Errorf("send durable Matrix artifact in room %s: %w", evt.RoomID, err)
	}
	if response == nil || response.EventID == "" {
		return fmt.Errorf("send durable Matrix artifact in room %s: empty event ID", evt.RoomID)
	}
	return nil
}
