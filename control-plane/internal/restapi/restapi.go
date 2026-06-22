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
	"time"

	computev1 "github.com/ParadoxFuzzle/control-plane/gen/compute/v1"
	"github.com/ParadoxFuzzle/control-plane/internal/registry"
)

// ============================================================================
// Public response shapes
// ============================================================================

// NodeResponse is the JSON-shaped view of one registered agent node.
type NodeResponse struct {
	NodeID   string        `json:"node_id"`
	Hostname string        `json:"hostname"`
	Address  string        `json:"address"`
	CPU      CPUSummary    `json:"cpu"`
	Memory   MemorySummary `json:"memory"`
	GPUs     []GPUSummary  `json:"gpus"`
	LastBeat string        `json:"last_beat"`
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

func memorySummary(m *computev1.MemoryResources) MemorySummary {
	if m == nil {
		return MemorySummary{}
	}
	return MemorySummary{
		TotalGB: float64(m.TotalBytes) / (1024 * 1024 * 1024),
	}
}

func gpuSummary(gs []*computev1.GpuResources) []GPUSummary {
	if len(gs) == 0 {
		return []GPUSummary{}
	}
	out := make([]GPUSummary, len(gs))
	for i, g := range gs {
		out[i] = GPUSummary{
			Index:  int(g.Index),
			Vendor: g.Vendor,
			Model:  g.Model,
			VRamGB: float64(g.VramBytes) / (1024 * 1024 * 1024),
		}
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

// RequireAPIKey is the X-API-Key middleware. Accepts the key from either the
// X-API-Key header or the api_key query parameter, and compares against the
// configured key with constant-time equality. If no key is configured, all
// requests are allowed.
func (a *APIServer) RequireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.apiKey != "" {
			key := r.Header.Get("X-API-Key")
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
			NodeID:   n.NodeID,
			Hostname: n.Hostname,
			Address:  n.Address,
			CPU:      cpuSummary(n.CPU),
			Memory:   memorySummary(n.Memory),
			GPUs:     gpuSummary(n.GPUs),
			LastBeat: n.LastBeat.Format(time.RFC3339),
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
