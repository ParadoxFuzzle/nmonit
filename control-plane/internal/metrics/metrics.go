// Package metrics provides Prometheus instrumentation for the control plane.
//
// Includes:
//   - gRPC server-side interceptors (unary + stream) for request count,
//     latency, and status codes per method.
//   - Custom application-level metrics (node count, heartbeats, registrations).
//   - A promhttp.Handler() for the /metrics HTTP endpoint.
package metrics

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// =========================================================================
// gRPC metrics
// =========================================================================

var (
	grpcServerRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nmonit_grpc_server_requests_total",
			Help: "Total gRPC requests handled by the server, grouped by method and status code.",
		},
		[]string{"grpc_method", "grpc_type", "grpc_code"},
	)

	grpcServerLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "nmonit_grpc_server_latency_seconds",
			Help:    "Server-side gRPC request latency in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"grpc_method", "grpc_type"},
	)

	grpcActiveStreams = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "nmonit_grpc_server_streams_active",
			Help: "Number of currently open gRPC streams.",
		},
		[]string{"grpc_method"},
	)
)

// =========================================================================
// Application-level metrics
// =========================================================================

var (
	// RegisteredNodes tracks the current number of registered agents.
	RegisteredNodes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "nmonit_nodes_registered_total",
		Help: "Current number of agent nodes registered with the control plane.",
	})

	// RegistrationsTotal counts every successful Register call.
	RegistrationsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "nmonit_registrations_total",
		Help: "Total number of successful agent registrations (including re-registrations).",
	})

	// HeartbeatsTotal counts every heartbeat received from agents.
	HeartbeatsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "nmonit_heartbeats_total",
		Help: "Total number of heartbeats received from all agents.",
	})

	// StaleNodesRemoved counts nodes removed by the stale cleanup routine.
	StaleNodesRemoved = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "nmonit_nodes_stale_removed_total",
		Help: "Total number of nodes removed due to missed heartbeats (stale cleanup).",
	})

	// TasksRunning tracks the current number of running tasks across all nodes.
	TasksRunning = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "nmonit_tasks_running_total",
		Help: "Current number of running tasks across all agent nodes.",
	})

	// TLSCertLoadTotal counts TLS certificate load/reload attempts.
	TLSCertLoadTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nmonit_tls_cert_load_total",
			Help: "Total TLS certificate load attempts (initial and reload), labeled by result (success or failure).",
		},
		[]string{"result"},
	)
)

func init() {
	prometheus.MustRegister(grpcServerRequests)
	prometheus.MustRegister(grpcServerLatency)
	prometheus.MustRegister(grpcActiveStreams)
	prometheus.MustRegister(RegisteredNodes)
	prometheus.MustRegister(RegistrationsTotal)
	prometheus.MustRegister(HeartbeatsTotal)
	prometheus.MustRegister(StaleNodesRemoved)
	prometheus.MustRegister(TasksRunning)
	prometheus.MustRegister(TLSCertLoadTotal)

	// Pre-populate known method cache so sanitizeMethod avoids runtime parsing.
	for _, m := range knownServiceMethods {
		knownMethods.Store(strings.ReplaceAll(m, "/", "."), struct{}{})
	}
}

// =========================================================================
// HTTP handler
// =========================================================================

// Handler returns an http.Handler that serves Prometheus metrics in the
// standard text-based exposition format.
func Handler() http.Handler {
	return promhttp.Handler()
}

// =========================================================================
// Interceptors
// =========================================================================

// UnaryServerInterceptor returns a gRPC unary interceptor that records
// request count, latency, and status code for every unary RPC.
func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		elapsed := time.Since(start).Seconds()

		method := sanitizeMethod(info.FullMethod)
		code := status.Code(err).String()

		grpcServerRequests.WithLabelValues(method, "unary", code).Inc()
		grpcServerLatency.WithLabelValues(method, "unary").Observe(elapsed)

		return resp, err
	}
}

// StreamServerInterceptor returns a gRPC stream interceptor that records
// request count, latency, and active streams for every streaming RPC.
func StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		method := sanitizeMethod(info.FullMethod)
		grpcActiveStreams.WithLabelValues(method).Inc()
		defer grpcActiveStreams.WithLabelValues(method).Dec()

		start := time.Now()
		err := handler(srv, ss)
		elapsed := time.Since(start).Seconds()

		code := status.Code(err).String()
		grpcServerRequests.WithLabelValues(method, "stream", code).Inc()
		grpcServerLatency.WithLabelValues(method, "stream").Observe(elapsed)

		return err
	}
}

// =========================================================================
// Helpers
// =========================================================================

// sanitizeMethod strips the leading '/' from FullMethod and replaces
// remaining '/' with '.' to create a clean label value.
//
// To prevent unbounded metric cardinality from malicious clients sending
// randomized method names, unknown methods are collapsed into a single
// "unknown_method" label. Known methods are tracked in a sync.Map.
func sanitizeMethod(full string) string {
	full = strings.TrimPrefix(full, "/")
	method := strings.ReplaceAll(full, "/", ".")

	if _, ok := knownMethods.Load(method); ok {
		return method
	}

	// Check if it's a known pattern (e.g. "compute.v1.AgentService/Register")
	// Known services: AgentService, MemoryService, ControlService
	parts := strings.SplitN(method, ".", 4)
	if len(parts) >= 3 {
		service := parts[len(parts)-2] + "/" + parts[len(parts)-1]
		for _, known := range knownServiceMethods {
			if service == known {
				knownMethods.Store(method, struct{}{})
				return method
			}
		}
	}

	// Unknown — collapse to avoid cardinality explosion
	return "unknown_method"
}

// knownMethods caches valid method names to avoid repeated string ops.
var knownMethods sync.Map

// knownServiceMethods lists all valid gRPC methods served by the control plane.
var knownServiceMethods = []string{
	"AgentService/Register",
	"AgentService/Heartbeat",
	"AgentService/ExecuteTask",
	"AgentService/AllocateMemory",
	"AgentService/AllocateGPUMemory",
	"AgentService/FreeMemory",
	"AgentService/FreeGPUMemory",
	"MemoryService/AllocateDistributed",
	"MemoryService/MigrateRegion",
	"MemoryService/GetRegion",
	"ControlService/GetClusterState",
	"ControlService/JoinCluster",
}
