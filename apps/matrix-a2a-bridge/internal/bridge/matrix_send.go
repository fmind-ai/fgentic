package bridge

import (
	"context"
	"errors"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// sendMessageEvent never calls mautrix IntentAPI.SendMessageEvent: that convenience method invokes
// EnsureJoined and can turn an untrusted event or a membership race into ambient room authority.
// Callers must establish the appropriate bot or managed-ghost membership before reaching this
// boundary; Matrix rejects the direct client request if that membership has since been revoked.
func sendMessageEvent(
	ctx context.Context,
	intent *appservice.IntentAPI,
	roomID id.RoomID,
	eventType event.Type,
	content any,
	extra ...mautrix.ReqSendEvent,
) (*mautrix.RespSendEvent, error) {
	if intent == nil || intent.Client == nil {
		return nil, errors.New("send Matrix event: intent client is unavailable")
	}
	return intent.Client.SendMessageEvent(
		ctx,
		roomID,
		eventType,
		intent.AddDoublePuppetValue(content),
		extra...,
	)
}

func sendStateEvent(
	ctx context.Context,
	intent *appservice.IntentAPI,
	roomID id.RoomID,
	eventType event.Type,
	stateKey string,
	content any,
	extra ...mautrix.ReqSendEvent,
) (*mautrix.RespSendEvent, error) {
	if intent == nil || intent.Client == nil {
		return nil, errors.New("send Matrix state event: intent client is unavailable")
	}
	return intent.Client.SendStateEvent(
		ctx,
		roomID,
		eventType,
		stateKey,
		intent.AddDoublePuppetValue(content),
		extra...,
	)
}
