package inbound

import (
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/uuid"
	"github.com/xtls/xray-core/proxy/vless"
)

// Ensure relayValidator implements the Validator interface and the custom
// MetaValidator interface (so the type assertion in encoding.go succeeds).
var (
	_ vless.Validator     = (*relayValidator)(nil)
	_ vless.MetaValidator = (*relayValidator)(nil)
)

// relayValidator accepts any UUID without authentication.
// Used on relay servers where auth is deferred to the exit node.
type relayValidator struct{}

func (v *relayValidator) Get(id uuid.UUID) *protocol.MemoryUser {
	return v.syntheticUser(id)
}

func (v *relayValidator) GetWithMeta(id uuid.UUID, clientVersion string) *protocol.MemoryUser {
	return v.syntheticUser(id)
}

func (v *relayValidator) Add(u *protocol.MemoryUser) error             { return nil }
func (v *relayValidator) Del(email string) error                       { return nil }
func (v *relayValidator) GetByEmail(email string) *protocol.MemoryUser { return nil }
func (v *relayValidator) GetAll() []*protocol.MemoryUser               { return nil }
func (v *relayValidator) GetCount() int64                              { return 0 }

func (v *relayValidator) syntheticUser(id uuid.UUID) *protocol.MemoryUser {
	return &protocol.MemoryUser{
		Account: &vless.MemoryAccount{ID: protocol.NewID(id)},
		Level:   0,
	}
}
