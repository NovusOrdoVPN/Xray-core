package metrics

import (
	"context"
	"encoding/json"
	"expvar"
	"net/http"
	_ "net/http/pprof"
	"strings"
	"time"

	"github.com/xtls/xray-core/app/observatory"
	"github.com/xtls/xray-core/app/stats"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/signal/done"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/extension"
	"github.com/xtls/xray-core/features/outbound"
	feature_stats "github.com/xtls/xray-core/features/stats"
)

type MetricsHandler struct {
	ohm          outbound.Manager
	statsManager feature_stats.Manager
	observatory  extension.Observatory
	tag          string
	listen       string
	tcpListener  net.Listener
}

type onlineUsersResponse struct {
	AsOfMs int64            `json:"asOfMs"`
	Direct map[string]int64 `json:"direct"`
	Proxy  map[string]int64 `json:"proxy"`
}

type onlineUserPresence struct {
	mode       string
	lastSeenMs int64
}

func inboundTagFromOnlineMapName(name string) string {
	parts := strings.Split(name, ">>>")
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

func classifyOnlineMode(tag string) string {
	if strings.Contains(strings.ToLower(tag), "proxy") {
		return "proxy"
	}
	return "direct"
}

func buildOnlineUsersResponse(manager *stats.Manager) onlineUsersResponse {
	rawByMode := map[string]map[string]int64{
		"direct": {},
		"proxy":  {},
	}

	manager.VisitOnlineMaps(func(name string, om *stats.OnlineMap) bool {
		if !strings.HasPrefix(name, "inbound>>>") {
			return true
		}

		tag := inboundTagFromOnlineMapName(name)
		if tag == "" {
			return true
		}

		mode := classifyOnlineMode(tag)
		for userID, lastSeen := range om.IpTimeMap() {
			lastSeenMs := lastSeen.UnixMilli()
			if current, found := rawByMode[mode][userID]; !found || lastSeenMs > current {
				rawByMode[mode][userID] = lastSeenMs
			}
		}

		return true
	})

	winners := make(map[string]onlineUserPresence)
	for _, mode := range []string{"direct", "proxy"} {
		for userID, lastSeenMs := range rawByMode[mode] {
			if existing, found := winners[userID]; !found || lastSeenMs > existing.lastSeenMs {
				winners[userID] = onlineUserPresence{
					mode:       mode,
					lastSeenMs: lastSeenMs,
				}
			}
		}
	}

	resp := onlineUsersResponse{
		AsOfMs: time.Now().UnixMilli(),
		Direct: map[string]int64{},
		Proxy:  map[string]int64{},
	}
	for userID, presence := range winners {
		if presence.mode == "proxy" {
			resp.Proxy[userID] = presence.lastSeenMs
		} else {
			resp.Direct[userID] = presence.lastSeenMs
		}
	}

	return resp
}

// NewMetricsHandler creates a new MetricsHandler based on the given config.
func NewMetricsHandler(ctx context.Context, config *Config) (*MetricsHandler, error) {
	c := &MetricsHandler{
		tag:    config.Tag,
		listen: config.Listen,
	}
	common.Must(core.RequireFeatures(ctx, func(om outbound.Manager, sm feature_stats.Manager) {
		c.statsManager = sm
		c.ohm = om
	}))
	expvar.Publish("stats", expvar.Func(func() interface{} {
		manager, ok := c.statsManager.(*stats.Manager)
		if !ok {
			return nil
		}
		resp := map[string]map[string]map[string]int64{
			"inbound":  {},
			"outbound": {},
			"user":     {},
		}
		manager.VisitCounters(func(name string, counter feature_stats.Counter) bool {
			nameSplit := strings.Split(name, ">>>")
			typeName, tagOrUser, direction := nameSplit[0], nameSplit[1], nameSplit[3]
			if item, found := resp[typeName][tagOrUser]; found {
				item[direction] = counter.Value()
			} else {
				resp[typeName][tagOrUser] = map[string]int64{
					direction: counter.Value(),
				}
			}
			return true
		})
		return resp
	}))
	expvar.Publish("online", expvar.Func(func() interface{} {
		manager, ok := c.statsManager.(*stats.Manager)
		if !ok {
			return nil
		}
		resp := map[string]int{}
		manager.VisitOnlineMaps(func(name string, om *stats.OnlineMap) bool {
			if strings.HasPrefix(name, "inbound>>>") {
				resp[name] = om.Count()
			}
			return true
		})
		return resp
	}))
	expvar.Publish("observatory", expvar.Func(func() interface{} {
		if c.observatory == nil {
			common.Must(core.RequireFeatures(ctx, func(observatory extension.Observatory) error {
				c.observatory = observatory
				return nil
			}))
			if c.observatory == nil {
				return nil
			}
		}
		resp := map[string]*observatory.OutboundStatus{}
		if o, err := c.observatory.GetObservation(context.Background()); err != nil {
			return err
		} else {
			for _, x := range o.(*observatory.ObservationResult).GetStatus() {
				resp[x.OutboundTag] = x
			}
		}
		return resp
	}))
	http.HandleFunc("/online", func(w http.ResponseWriter, r *http.Request) {
		manager, ok := c.statsManager.(*stats.Manager)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		resp := map[string]int{}
		manager.VisitOnlineMaps(func(name string, om *stats.OnlineMap) bool {
			if strings.HasPrefix(name, "inbound>>>") {
				// Extract tag from "inbound>>>tag>>>online"
				parts := strings.Split(name, ">>>")
				if len(parts) >= 2 {
					resp[parts[1]] = om.Count()
				}
			}
			return true
		})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	http.HandleFunc("/online-users", func(w http.ResponseWriter, r *http.Request) {
		manager, ok := c.statsManager.(*stats.Manager)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(buildOnlineUsersResponse(manager))
	})

	return c, nil
}

func (p *MetricsHandler) Type() interface{} {
	return (*MetricsHandler)(nil)
}

func (p *MetricsHandler) Start() error {

	// direct listen a port if listen is set
	if p.listen != "" {
		TCPlistener, err := net.Listen("tcp", p.listen)
		if err != nil {
			return err
		}
		p.tcpListener = TCPlistener
		errors.LogInfo(context.Background(), "Metrics server listening on ", p.listen)

		go func() {
			if err := http.Serve(TCPlistener, http.DefaultServeMux); err != nil {
				errors.LogErrorInner(context.Background(), err, "failed to start metrics server")
			}
		}()
	}

	listener := &OutboundListener{
		buffer: make(chan net.Conn, 4),
		done:   done.New(),
	}

	go func() {
		if err := http.Serve(listener, http.DefaultServeMux); err != nil {
			errors.LogErrorInner(context.Background(), err, "failed to start metrics server")
		}
	}()

	if err := p.ohm.RemoveHandler(context.Background(), p.tag); err != nil {
		errors.LogInfo(context.Background(), "failed to remove existing handler")
	}

	return p.ohm.AddHandler(context.Background(), &Outbound{
		tag:      p.tag,
		listener: listener,
	})
}

func (p *MetricsHandler) Close() error {
	return nil
}

func init() {
	common.Must(common.RegisterConfig((*Config)(nil), func(ctx context.Context, cfg interface{}) (interface{}, error) {
		return NewMetricsHandler(ctx, cfg.(*Config))
	}))
}
