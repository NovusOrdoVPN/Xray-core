package vless

import (
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/uuid"
)

// CUSTOM: optional interface extending Validator with client-metadata-aware validation.
//
// Purpose: tower-backed validators (remote/relay) need the client's app version
// at validation time to enforce version policies or forward the value upstream.
// Upstream's Validator interface only exposes Get(id) without context, so we
// defined this optional interface in a custom file instead of modifying upstream.
//
// Call sites detect support via type assertion:
//
//	if mv, ok := validator.(MetaValidator); ok {
//	    user = mv.GetWithMeta(id, clientVersion)
//	} else {
//	    user = validator.Get(id)
//	}
//
// This keeps upstream's Validator interface and MemoryValidator untouched,
// so routine upstream syncs do not conflict with this feature.
type MetaValidator interface {
	// GetWithMeta validates the UUID and passes client metadata (e.g. app version)
	// to the validation backend. Returns nil if the user is unknown or rejected.
	GetWithMeta(id uuid.UUID, clientVersion string) *protocol.MemoryUser
}
