package splithttp

import (
	"context"
	"crypto/rand"
	"math"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/common/errors"
)

type XmuxConn interface {
	IsClosed() bool
}

type XmuxClient struct {
	XmuxConn     XmuxConn
	OpenUsage    atomic.Int32
	leftUsage    int32
	LeftRequests atomic.Int32
	UnreusableAt time.Time
}

type XmuxManager struct {
	xmuxConfig  XmuxConfig
	concurrency int32
	connections int32
	newConnFunc func() XmuxConn
	xmuxClients []*XmuxClient
}

func NewXmuxManager(xmuxConfig XmuxConfig, newConnFunc func() XmuxConn) *XmuxManager {
	return &XmuxManager{
		xmuxConfig:  xmuxConfig,
		concurrency: xmuxConfig.GetNormalizedMaxConcurrency().rand(),
		connections: xmuxConfig.GetNormalizedMaxConnections().rand(),
		newConnFunc: newConnFunc,
		xmuxClients: make([]*XmuxClient, 0),
	}
}

func (m *XmuxManager) newXmuxClient() *XmuxClient {
	xmuxClient := &XmuxClient{
		XmuxConn:  m.newConnFunc(),
		leftUsage: -1,
	}
	if x := m.xmuxConfig.GetNormalizedCMaxReuseTimes().rand(); x > 0 {
		xmuxClient.leftUsage = x - 1
	}
	xmuxClient.LeftRequests.Store(math.MaxInt32)
	if x := m.xmuxConfig.GetNormalizedHMaxRequestTimes().rand(); x > 0 {
		xmuxClient.LeftRequests.Store(x)
	}
	if x := m.xmuxConfig.GetNormalizedHMaxReusableSecs().rand(); x > 0 {
		xmuxClient.UnreusableAt = time.Now().Add(time.Duration(x) * time.Second)
	}
	m.xmuxClients = append(m.xmuxClients, xmuxClient)
	return xmuxClient
}

// CUSTOM: closeXmuxConn tears down the underlying HTTP transport of an XmuxClient
// if it supports CloseTransport (DefaultDialerClient does). Without this, removing
// the XmuxClient from the slice just releases our reference — Go's GC doesn't close
// the http.Client's connection pool promptly, leaving goroutines/TLS state/idle
// sockets alive. Matters on iOS NetworkExtension (50MB limit).
func closeXmuxConn(conn XmuxConn) {
	if closer, ok := conn.(interface{ CloseTransport() }); ok {
		closer.CloseTransport()
	}
}

// CUSTOM: CleanupIdleClients removes expired/closed clients that have no active
// streams and closes their HTTP transports. Returns true if any were removed.
// Called by the periodic/lazy cleanup path in dialer.go.
func (m *XmuxManager) CleanupIdleClients() bool {
	cleaned := false
	for i := 0; i < len(m.xmuxClients); {
		client := m.xmuxClients[i]
		expired := client.XmuxConn.IsClosed() ||
			client.leftUsage == 0 ||
			client.LeftRequests.Load() <= 0 ||
			(client.UnreusableAt != time.Time{} && time.Now().After(client.UnreusableAt))

		if expired && client.OpenUsage.Load() <= 0 {
			closeXmuxConn(client.XmuxConn)
			m.xmuxClients = append(m.xmuxClients[:i], m.xmuxClients[i+1:]...)
			cleaned = true
		} else {
			i++
		}
	}
	return cleaned
}

// CUSTOM: IsEmpty returns true when the manager holds no active clients.
// Used by the global dialer map cleanup to decide when to drop a manager.
func (m *XmuxManager) IsEmpty() bool {
	return len(m.xmuxClients) == 0
}

func (m *XmuxManager) GetXmuxClient(ctx context.Context) *XmuxClient { // when locking
	for i := 0; i < len(m.xmuxClients); {
		xmuxClient := m.xmuxClients[i]
		if xmuxClient.XmuxConn.IsClosed() ||
			xmuxClient.leftUsage == 0 ||
			xmuxClient.LeftRequests.Load() <= 0 ||
			(xmuxClient.UnreusableAt != time.Time{} && time.Now().After(xmuxClient.UnreusableAt)) {
			errors.LogDebug(ctx, "XMUX: removing xmuxClient, IsClosed() = ", xmuxClient.XmuxConn.IsClosed(),
				", OpenUsage = ", xmuxClient.OpenUsage.Load(),
				", leftUsage = ", xmuxClient.leftUsage,
				", LeftRequests = ", xmuxClient.LeftRequests.Load(),
				", UnreusableAt = ", xmuxClient.UnreusableAt)
			// CUSTOM: on mobile, actively close the transport when discarding a
			// client with no active streams (see cleanup_mobile.go). No-op on
			// desktop/server (see cleanup_desktop.go) — falls through to
			// upstream's behavior of just dropping the client from the slice.
			if xmuxClient.OpenUsage.Load() <= 0 {
				maybeCloseOnDiscardXmux(xmuxClient.XmuxConn)
			}
			m.xmuxClients = append(m.xmuxClients[:i], m.xmuxClients[i+1:]...)
		} else {
			i++
		}
	}

	if len(m.xmuxClients) == 0 {
		errors.LogDebug(ctx, "XMUX: creating xmuxClient because xmuxClients is empty")
		return m.newXmuxClient()
	}

	if m.connections > 0 && len(m.xmuxClients) < int(m.connections) {
		errors.LogDebug(ctx, "XMUX: creating xmuxClient because maxConnections was not hit, xmuxClients = ", len(m.xmuxClients))
		return m.newXmuxClient()
	}

	xmuxClients := make([]*XmuxClient, 0)
	if m.concurrency > 0 {
		for _, xmuxClient := range m.xmuxClients {
			if xmuxClient.OpenUsage.Load() < m.concurrency {
				xmuxClients = append(xmuxClients, xmuxClient)
			}
		}
	} else {
		xmuxClients = m.xmuxClients
	}

	if len(xmuxClients) == 0 {
		errors.LogDebug(ctx, "XMUX: creating xmuxClient because maxConcurrency was hit, xmuxClients = ", len(m.xmuxClients))
		return m.newXmuxClient()
	}

	i, _ := rand.Int(rand.Reader, big.NewInt(int64(len(xmuxClients))))
	xmuxClient := xmuxClients[i.Int64()]
	if xmuxClient.leftUsage > 0 {
		xmuxClient.leftUsage -= 1
	}
	return xmuxClient
}
