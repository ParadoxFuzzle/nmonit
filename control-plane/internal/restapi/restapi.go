// Package restapi serves the HTTP control-plane API:
//
//	GET  /health             — liveness probe, no auth, JSON status
//	GET  /api/v1/nodes       — registered-node snapshot, X-API-Key protected
//	GET  /metrics            — Prometheus handler, X-API-Key protected
//
// Response shapes are owned by this package so that future transports (CLI,
// GraphQL, …) can map registry.NodeInfo into whatever shape they need
// without dragging JSON tags into the core domain.
package restapi

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	computev1 "github.com/ParadoxFuzzle/control-plane/gen/compute/v1"
	"github.com/ParadoxFuzzle/control-plane/internal/registry"
)

// ============================================================================
// Public response shapes
// ============================================================================

// NodeResponse is the JSON-shaped view of one registered agent node.
type NodeResponse struct {
	NodeID      string              `json:"node_id"`
	Hostname    string              `json:"hostname"`
	Address     string              `json:"address"`
	CPU         CPUSummary          `json:"cpu"`
	Memory      MemorySummary       `json:"memory"`
	GPUs        []GPUSummary        `json:"gpus"`
	Utilization *UtilizationSummary `json:"utilization,omitempty"`
	LastBeat    string              `json:"last_beat"`
}

// CPUSummary is the REST-friendly form of a node's CPU resources.
type CPUSummary struct {
	PhysicalCores int    `json:"physical_cores"`
	LogicalCores  int    `json:"logical_cores"`
	Architecture  string `json:"architecture"`
	Model         string `json:"model"`
}

// MemorySummary is the REST-friendly form of a node's memory resources.
type MemorySummary struct {
	TotalGB float64 `json:"total_gb"`
}

// GPUSummary is the REST-friendly form of one GPU device.
type GPUSummary struct {
	Index  int     `json:"index"`
	Vendor string  `json:"vendor"`
	Model  string  `json:"model"`
	VRamGB float64 `json:"vram_gb"`
}

// UtilizationSummary is the live resource view reported by heartbeats.
type UtilizationSummary struct {
	Timestamp string                    `json:"timestamp,omitempty"`
	CPU       *CPUUtilizationSummary    `json:"cpu,omitempty"`
	Memory    *MemoryUtilizationSummary `json:"memory,omitempty"`
	GPUs      []GPUUtilizationSummary   `json:"gpus"`
}

// CPUUtilizationSummary is the REST-friendly form of live CPU usage.
type CPUUtilizationSummary struct {
	OverallPercent float64   `json:"overall_percent"`
	PerCorePercent []float64 `json:"per_core_percent,omitempty"`
	Load1          float64   `json:"load_1m"`
	Load5          float64   `json:"load_5m"`
	Load15         float64   `json:"load_15m"`
}

// MemoryUtilizationSummary is the REST-friendly form of live memory usage.
type MemoryUtilizationSummary struct {
	UsedGB      float64 `json:"used_gb"`
	AvailableGB float64 `json:"available_gb"`
	TotalGB     float64 `json:"total_gb"`
}

// GPUUtilizationSummary is the REST-friendly form of live GPU usage.
type GPUUtilizationSummary struct {
	Index          int     `json:"index"`
	GPUPercent     float64 `json:"gpu_percent"`
	MemoryPercent  float64 `json:"memory_percent"`
	VRAMUsedGB     float64 `json:"vram_used_gb"`
	VRAMTotalGB    float64 `json:"vram_total_gb"`
	TemperatureC   int     `json:"temperature_c"`
	PowerDrawW     int64   `json:"power_draw_w"`
	PowerLimitW    int64   `json:"power_limit_w"`
	ClockMHz       int64   `json:"clock_mhz"`
	MemoryClockMHz int64   `json:"memory_clock_mhz"`
}

// ============================================================================
// Mapping helpers
//
// These take individual proto fields rather than a full *NodeResources
// because callers (APIServer.NodesHandler) operate on NodeSummary values
// returned by registry.NodeRegistry.GetAll, which already deconstructed
// the resource into Hostname + CPU + Memory + GPUs.
// ============================================================================

func cpuSummary(c *computev1.CpuResources) CPUSummary {
	if c == nil {
		return CPUSummary{}
	}
	return CPUSummary{
		PhysicalCores: int(c.PhysicalCores),
		LogicalCores:  int(c.LogicalCores),
		Architecture:  c.Architecture,
		Model:         c.ModelName,
	}
}

func bytesToGB(bytes uint64) float64 {
	return float64(bytes) / (1024 * 1024 * 1024)
}

func memorySummary(m *computev1.MemoryResources) MemorySummary {
	if m == nil {
		return MemorySummary{}
	}
	return MemorySummary{
		TotalGB: bytesToGB(m.TotalBytes),
	}
}

func gpuSummary(gs []*computev1.GpuResources) []GPUSummary {
	if len(gs) == 0 {
		return []GPUSummary{}
	}
	out := make([]GPUSummary, 0, len(gs))
	for _, g := range gs {
		if g == nil {
			continue
		}
		out = append(out, GPUSummary{
			Index:  int(g.Index),
			Vendor: g.Vendor,
			Model:  g.Model,
			VRamGB: bytesToGB(g.VramBytes),
		})
	}
	return out
}

func utilizationSummary(u *computev1.NodeUtilization) *UtilizationSummary {
	if u == nil {
		return nil
	}
	out := &UtilizationSummary{
		GPUs: make([]GPUUtilizationSummary, 0, len(u.Gpus)),
	}
	if u.TimestampNs != 0 {
		out.Timestamp = time.Unix(0, int64(u.TimestampNs)).UTC().Format(time.RFC3339Nano)
	}
	if u.Cpu != nil {
		out.CPU = &CPUUtilizationSummary{
			OverallPercent: u.Cpu.OverallPercent,
			PerCorePercent: append([]float64(nil), u.Cpu.PerCorePercent...),
			Load1:          u.Cpu.Load_1M,
			Load5:          u.Cpu.Load_5M,
			Load15:         u.Cpu.Load_15M,
		}
	}
	if u.Memory != nil {
		total := u.Memory.UsedBytes + u.Memory.AvailableBytes
		out.Memory = &MemoryUtilizationSummary{
			UsedGB:      bytesToGB(u.Memory.UsedBytes),
			AvailableGB: bytesToGB(u.Memory.AvailableBytes),
			TotalGB:     bytesToGB(total),
		}
	}
	for _, g := range u.Gpus {
		if g == nil {
			continue
		}
		out.GPUs = append(out.GPUs, GPUUtilizationSummary{
			Index:          int(g.Index),
			GPUPercent:     g.GpuPercent,
			MemoryPercent:  g.MemoryPercent,
			VRAMUsedGB:     bytesToGB(g.VramUsedBytes),
			VRAMTotalGB:    bytesToGB(g.VramTotalBytes),
			TemperatureC:   int(g.TemperatureCelsius),
			PowerDrawW:     g.PowerDrawWatts,
			PowerLimitW:    g.PowerLimitWatts,
			ClockMHz:       g.ClockMhz,
			MemoryClockMHz: g.MemoryClockMhz,
		})
	}
	return out
}

// ============================================================================
// APIServer
// ============================================================================

// APIServer provides the REST API gateway.
type APIServer struct {
	registry *registry.NodeRegistry
	// apiKey for REST auth (empty = no auth).
	apiKey string
	// Whether TLS is enabled for the HTTP server (controls HSTS header).
	tlsEnabled bool
}

// NewAPIServer constructs an APIServer bound to a node registry.
func NewAPIServer(reg *registry.NodeRegistry, apiKey string, tlsEnabled bool) *APIServer {
	return &APIServer{
		registry:   reg,
		apiKey:     apiKey,
		tlsEnabled: tlsEnabled,
	}
}

// RequireAPIKey is the API-key middleware. It accepts the key from the
// X-API-Key header, an Authorization: Bearer header (used by Prometheus), or
// the legacy api_key query parameter, and compares it against the configured
// key with constant-time equality. If no key is configured, all requests are
// allowed.
func (a *APIServer) RequireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.apiKey != "" {
			key := r.Header.Get("X-API-Key")
			if key == "" {
				const bearerPrefix = "Bearer "
				auth := r.Header.Get("Authorization")
				if len(auth) > len(bearerPrefix) && strings.EqualFold(auth[:len(bearerPrefix)], bearerPrefix) {
					key = strings.TrimSpace(auth[len(bearerPrefix):])
				}
			}
			if key == "" {
				key = r.URL.Query().Get("api_key")
			}
			if subtle.ConstantTimeCompare([]byte(key), []byte(a.apiKey)) != 1 {
				setSecurityHeaders(w, a.tlsEnabled)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
				return
			}
		}
		next(w, r)
	}
}

// HealthHandler serves GET /health.
func (a *APIServer) HealthHandler(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w, a.tlsEnabled)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// NodesHandler serves GET /api/v1/nodes by mapping []registry.NodeSummary
// (returned by registry.NodeRegistry.GetAll) into []NodeResponse.
func (a *APIServer) NodesHandler(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w, a.tlsEnabled)
	w.Header().Set("Content-Type", "application/json")
	nodes := a.registry.GetAll()
	out := make([]NodeResponse, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, NodeResponse{
			NodeID:      n.NodeID,
			Hostname:    n.Hostname,
			Address:     n.Address,
			CPU:         cpuSummary(n.CPU),
			Memory:      memorySummary(n.Memory),
			GPUs:        gpuSummary(n.GPUs),
			Utilization: utilizationSummary(n.Utilization),
			LastBeat:    n.LastBeat.Format(time.RFC3339),
		})
	}
	_ = json.NewEncoder(w).Encode(out)
}

// setSecurityHeaders adds standard HTTP security headers to every response.
// When TLS is enabled, it also adds the Strict-Transport-Security (HSTS)
// header with a 1-year max-age to instruct browsers to always use HTTPS.
func setSecurityHeaders(w http.ResponseWriter, tlsEnabled bool) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	if tlsEnabled {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
}
