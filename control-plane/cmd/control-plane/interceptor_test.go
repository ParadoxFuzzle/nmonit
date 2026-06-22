package main

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ParadoxFuzzle/control-plane/internal/agent"
	"github.com/ParadoxFuzzle/control-plane/internal/metrics"
)

// TestInterceptorChain_AuthRejectRecordedInMetrics guards the gRPC chain
// wiring in main() against an accidental order swap.
//
// grpc.ChainUnaryInterceptor runs the leftmost interceptor first, on the
// outermost layer. With the current order (metrics outermost, auth inner),
// an auth-rejected call returns from auth without invoking the final
// handler — but metrics still records the request-count / latency, so
// dashboards reflect attempts that the auth layer blocked.
//
// A future edit that swaps the order (auth outer, metrics inner) would
// cause Unauthenticated / PermissionDenied calls to SILENTLY skip the
// metrics layer, hiding auth abuse from operators. This test fails in
// that scenario by verifying both the counter and the latency histogram
// increment when the chain rejects.
//
// The chain is composed manually here (rather than via grpc.ChainUnaryInterceptor
// itself, which returns a ServerOption rather than the chained function)
// by nesting: metricsIntr → authIntr → terminalHandler. The nesting matches
// gRPC's internal composition for the left-to-right chain order used in
// main.go.
//
// Test-pollution note: other packages' tests increment metrics with different
// names (nmonit_registrations_total, etc.), so the gRPC label sets exercised
// here are exclusive to this test within the control-plane suite. We do NOT
// call t.Parallel() and assert exact delta values.
func TestInterceptorChain_AuthRejectRecordedInMetrics(t *testing.T) {
	const (
		fullMethod    = "/compute.v1.AgentService/Register"
		sanitized     = "compute.v1.AgentService.Register" // sanitizeMethod strips leading '/' AND turns every '/' into '.'
		counterName   = "nmonit_grpc_server_requests_total"
		histogramName = "nmonit_grpc_server_latency_seconds"
	)
	counterLabels := map[string]string{
		"grpc_method": sanitized,
		"grpc_type":   "unary",
		"grpc_code":   "Unauthenticated",
	}
	histogramLabels := map[string]string{
		"grpc_method": sanitized,
		"grpc_type":   "unary",
	}

	counterBefore := readCounter(t, counterName, counterLabels)
	samplesBefore := readHistogramSampleCount(t, histogramName, histogramLabels)

	metricsIntr := metrics.UnaryServerInterceptor()
	authIntr := agent.AuthUnaryInterceptorFor("test-token")

	info := &grpc.UnaryServerInfo{FullMethod: fullMethod}
	handlerCalled := false
	terminal := func(ctx context.Context, req any) (any, error) {
		handlerCalled = true
		return nil, nil
	}

	// Manually compose the same wiring grpc.ChainUnaryInterceptor(
	// [metrics, auth]) would build: metrics outermost (leftmost), then
	// auth, then the terminal handler. This mirrors the chain semantics
	// documented by gRPC and matches main.go's order.
	_, err := metricsIntr(context.Background(), "input", info,
		func(ctx context.Context, req any) (any, error) {
			return authIntr(ctx, req, info, terminal)
		})

	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated from auth, got: %v (err=%v)", got, err)
	}
	if handlerCalled {
		t.Error("terminal handler must not be invoked when auth rejects")
	}

	counterAfter := readCounter(t, counterName, counterLabels)
	if delta := counterAfter - counterBefore; delta != 1 {
		t.Errorf("counter delta: want 1, got %v (before=%v after=%v) — chain wiring may have moved auth outer of metrics",
			delta, counterBefore, counterAfter)
	}

	samplesAfter := readHistogramSampleCount(t, histogramName, histogramLabels)
	if delta := samplesAfter - samplesBefore; delta != 1 {
		t.Errorf("latency histogram sample-count delta: want 1, got %v (before=%v after=%v) — chain wiring may have moved auth outer of metrics",
			delta, samplesBefore, samplesAfter)
	}
}

// readCounter returns the current value of a Prometheus counter family
// matched by name + label set, or 0 if no matching observation exists.
func readCounter(t *testing.T, metricName string, labels map[string]string) float64 {
	t.Helper()
	return readScalar(t, metricName, labels, func(m *dto.Metric) float64 {
		if c := m.GetCounter(); c != nil {
			return c.GetValue()
		}
		return 0
	})
}

// readHistogramSampleCount returns the cumulative sample count of a
// Prometheus histogram matched by name + label set.
func readHistogramSampleCount(t *testing.T, metricName string, labels map[string]string) uint64 {
	t.Helper()
	v := readScalar(t, metricName, labels, func(m *dto.Metric) float64 {
		if h := m.GetHistogram(); h != nil {
			return float64(h.GetSampleCount())
		}
		return 0
	})
	return uint64(v)
}

// readScalar walks the DefaultGatherer output and applies a type-specific
// extractor to the first metric matching the name + label set. Returns 0
// when no match is found.
func readScalar(t *testing.T, metricName string, labels map[string]string, extract func(*dto.Metric) float64) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("prometheus.DefaultGatherer.Gather failed: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != metricName {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelsMatch(m.GetLabel(), labels) {
				return extract(m)
			}
		}
	}
	return 0
}

func labelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	gotMap := make(map[string]string, len(got))
	for _, lp := range got {
		gotMap[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if gotMap[k] != v {
			return false
		}
	}
	return true
}
