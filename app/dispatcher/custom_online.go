package dispatcher

// CUSTOM: per-inbound online user tracking.
//
// Upstream already tracks online IPs per user (user>>>email>>>online) via
// trackOnlineIP in default.go. This file adds a parallel map keyed by inbound
// tag (inbound>>>tag>>>online) that dedups using a UUID-based identity, so
// the admin portal can report concurrent user counts per inbound — including
// for email-less users such as the synthetic users returned by relayValidator.
//
// The call sites live in app/dispatcher/default.go (getLink and WrapLink)
// inside the `if p.Stats.UserOnline` branch; they are marked with
// CUSTOM-BEGIN/CUSTOM-END so future upstream syncs surface them clearly.

import (
	"context"

	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/features/stats"
	"github.com/xtls/xray-core/proxy/vless"
)

// userOnlineIdentity returns a stable per-user identifier for online tracking.
// For VLESS users it returns the UUID string (preferred — works even when email
// is empty, e.g. synthetic users from the remote or relay validators). Falls
// back to email for other protocols.
func userOnlineIdentity(user *protocol.MemoryUser) string {
	if user == nil {
		return ""
	}
	if account, ok := user.Account.(*vless.MemoryAccount); ok && account.ID != nil {
		return account.ID.String()
	}
	return user.Email
}

// trackInboundOnline registers the connected user (UUID or email) against the
// inbound tag. The admin portal consumes this via /online and /online-users
// to report concurrent users per inbound. RemoveIP is paired via
// context.AfterFunc so the refcount-based OnlineMap decrements on disconnect.
func trackInboundOnline(ctx context.Context, sm stats.Manager, inboundTag, identity string) {
	if inboundTag == "" || identity == "" {
		return
	}
	name := "inbound>>>" + inboundTag + ">>>online"
	if om, _ := stats.GetOrRegisterOnlineMap(sm, name); om != nil {
		om.AddIP(identity)
		context.AfterFunc(ctx, func() { om.RemoveIP(identity) })
	}
}
