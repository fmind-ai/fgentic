package matrix

import (
	"context"
	"errors"
	"fmt"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Client adapts the reconciler's RoomManager to the mautrix/go client used across the platform. It
// wraps a NORMAL client logged in as the scoped access-manager identity; it never uses an admin API.
// This is the production adapter — the live-cluster invite/kick flow is the deferred acceptance path,
// while the reconciler's decision logic is proven offline against a fake RoomManager.
type Client struct {
	cli *mautrix.Client
}

// NewClient builds the access-manager Matrix client from its homeserver URL, MXID, and access token.
func NewClient(homeserverURL, accessManagerMXID, accessToken string) (*Client, error) {
	cli, err := mautrix.NewClient(homeserverURL, id.UserID(accessManagerMXID), accessToken)
	if err != nil {
		return nil, fmt.Errorf("build matrix client: %w", err)
	}
	return &Client{cli: cli}, nil
}

// ResolveAlias maps a room alias to its room ID.
func (c *Client) ResolveAlias(ctx context.Context, alias string) (string, error) {
	resp, err := c.cli.ResolveAlias(ctx, id.RoomAlias(alias))
	if err != nil {
		return "", fmt.Errorf("resolve alias %s: %w", alias, err)
	}
	return resp.RoomID.String(), nil
}

// RoomState reads the room's current authorization-relevant state from a single /state fetch.
func (c *Client) RoomState(ctx context.Context, roomID string) (RoomState, error) {
	stateMap, err := c.cli.State(ctx, id.RoomID(roomID))
	if err != nil {
		return RoomState{}, fmt.Errorf("read room state %s: %w", roomID, err)
	}
	out := RoomState{RoomID: roomID, Members: map[string]Membership{}}

	if create := stateEvent(stateMap, event.StateCreate); create != nil {
		// Room v12: the creator is the m.room.create sender. The deprecated content Creator field is
		// only consulted for older rooms, which the reconciler rejects as unmanaged anyway.
		out.Creator = create.Sender.String()
		if cc := create.Content.AsCreate(); cc != nil && cc.RoomVersion != "" {
			out.Version = string(cc.RoomVersion)
		}
	}
	if pl := stateEvent(stateMap, event.StatePowerLevels); pl != nil {
		if plc := pl.Content.AsPowerLevels(); plc != nil {
			out.Power = powerLevels(plc)
		}
	}
	for stateKey, ev := range stateMap[event.StateMember] {
		if mc := ev.Content.AsMember(); mc != nil {
			out.Members[stateKey] = Membership(mc.Membership)
		}
	}
	return out, nil
}

// AccountExists reports whether a local MXID is a provisioned account. A profile lookup returning
// M_NOT_FOUND means the account does not exist (fail closed: no grant); other errors propagate.
func (c *Client) AccountExists(ctx context.Context, mxid string) (bool, error) {
	_, err := c.cli.GetProfile(ctx, id.UserID(mxid))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, mautrix.MNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("profile lookup %s: %w", mxid, err)
}

// Invite grants access through the normal invite endpoint.
func (c *Client) Invite(ctx context.Context, roomID, mxid, reason string) error {
	if _, err := c.cli.InviteUser(ctx, id.RoomID(roomID), &mautrix.ReqInviteUser{UserID: id.UserID(mxid), Reason: reason}); err != nil {
		return fmt.Errorf("invite %s to %s: %w", mxid, roomID, err)
	}
	return nil
}

// Kick revokes access (withdraws a pending invite or removes a joined member) via the kick endpoint.
func (c *Client) Kick(ctx context.Context, roomID, mxid, reason string) error {
	if _, err := c.cli.KickUser(ctx, id.RoomID(roomID), &mautrix.ReqKickUser{UserID: id.UserID(mxid), Reason: reason}); err != nil {
		return fmt.Errorf("kick %s from %s: %w", mxid, roomID, err)
	}
	return nil
}

// Ban is the emergency-revocation escalation.
func (c *Client) Ban(ctx context.Context, roomID, mxid, reason string) error {
	if _, err := c.cli.BanUser(ctx, id.RoomID(roomID), &mautrix.ReqBanUser{UserID: id.UserID(mxid), Reason: reason}); err != nil {
		return fmt.Errorf("ban %s from %s: %w", mxid, roomID, err)
	}
	return nil
}

func stateEvent(stateMap mautrix.RoomStateMap, t event.Type) *event.Event {
	byKey, ok := stateMap[t]
	if !ok {
		return nil
	}
	return byKey[""]
}

func powerLevels(plc *event.PowerLevelsEventContent) PowerLevels {
	users := make(map[string]int, len(plc.Users))
	for uid, level := range plc.Users {
		users[uid.String()] = level
	}
	return PowerLevels{
		Users:        users,
		UsersDefault: plc.UsersDefault,
		Invite:       plc.Invite(),
		Kick:         plc.Kick(),
		Ban:          plc.Ban(),
		StateDefault: plc.StateDefault(),
	}
}
