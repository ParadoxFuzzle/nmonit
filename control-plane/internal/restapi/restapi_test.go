package restapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	computev1 "github.com/ParadoxFuzzle/control-plane/gen/compute/v1"
	"github.com/ParadoxFuzzle/control-plane/internal/registry"
)

// ============================================================================
// RequireAPIKey middleware
// ============================================================================

func TestRequireAPIKey_NoKeyRequired(t *testing.T) {
	api := NewAPIServer(registry.NewNodeRegistry(), "", false)
	called := false
	handler := api.RequireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if !called {
		t.Error("expected handler to be called when no API key required")
	}
}

func TestRequireAPIKey_MissingKey(t *testing.T) {
	api := NewAPIServer(registry.NewNodeRegistry(), "my-api-key", false)
	called := false
	handler := api.RequireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if called {
		t.Error("expected handler to be blocked when API key missing")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestRequireAPIKey_ValidHeader(t *testing.T) {
	api := NewAPIServer(registry.NewNodeRegistry(), "my-api-key", false)
	called := false
	handler := api.RequireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	req.Header.Set("X-API-Key", "my-api-key")
	w := httptest.NewRecorder()
	handler(w, req)

	if !called {
		t.Error("expected handler to be called with valid X-API-Key header")
	}
}

func TestRequireAPIKey_ValidQueryParam(t *testing.T) {
	api := NewAPIServer(registry.NewNodeRegistry(), "my-api-key", false)
	called := false
	handler := api.RequireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes?api_key=my-api-key", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if !called {
		t.Error("expected handler to be called with valid api_key query param")
	}
}

func TestRequireAPIKey_InvalidKey(t *testing.T) {
	api := NewAPIServer(registry.NewNodeRegistry(), "my-api-key", false)
	called := false
	handler := api.RequireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	req.Header.Set("X-API-Key", "wrong-key")
	w := httptest.NewRecorder()
	handler(w, req)

	if called {
		t.Error("expected handler to be blocked with wrong API key")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

// ============================================================================
// Summary helper mappings — signatures now take individual proto types.
// ============================================================================

func TestCPUSummary_Nil(t *testing.T) {
	if got := cpuSummary(nil); got.PhysicalCores != 0 {
		t.Error("expected zero CpuSummary for nil input")
	}
}

func TestCPUSummary_WithData(t *testing.T) {
	cpu := &computev1.CpuResources{
		PhysicalCores: 16,
		LogicalCores:  32,
		Architecture:  "aarch64",
		ModelName:     "Apple M2",
	}
	got := cpuSummary(cpu)
	if got.PhysicalCores != 16 {
		t.Errorf("expected 16 cores, got %d", got.PhysicalCores)
	}
	if got.LogicalCores != 32 {
		t.Errorf("expected 32 logical, got %d", got.LogicalCores)
	}
	if got.Architecture != "aarch64" {
		t.Errorf("expected aarch64, got %s", got.Architecture)
	}
	if got.Model != "Apple M2" {
		t.Errorf("expected Apple M2 model, got %s", got.Model)
	}
}

func TestMemorySummary_Nil(t *testing.T) {
	if got := memorySummary(nil); got.TotalGB != 0 {
		t.Error("expected zero MemorySummary for nil input")
	}
}

func TestMemorySummary(t *testing.T) {
	mem := &computev1.MemoryResources{
		TotalBytes: 16 * 1024 * 1024 * 1024, // 16 GB
	}
	if got := memorySummary(mem); got.TotalGB != 16.0 {
		t.Errorf("expected 16 GB, got %f", got.TotalGB)
	}
}

func TestGPUSummary(t *testing.T) {
	gpus := []*computev1.GpuResources{
		{Index: 0, Vendor: "nvidia", Model: "RTX 4090", VramBytes: 24 * 1024 * 1024 * 1024},
		{Index: 1, Vendor: "nvidia", Model: "RTX 4090", VramBytes: 24 * 1024 * 1024 * 1024},
	}
	got := gpuSummary(gpus)
	if len(got) != 2 {
		t.Fatalf("expected 2 GPUs, got %d", len(got))
	}
	if got[0].VRamGB != 24.0 {
		t.Errorf("expected 24 GB VRAM, got %f", got[0].VRamGB)
	}
	if got[1].Index != 1 {
		t.Errorf("expected index 1, got %d", got[1].Index)
	}
}

func TestUtilizationSummary_Nil(t *testing.T) {
	if got := utilizationSummary(nil); got != nil {
		t.Errorf("expected nil utilization summary, got %#v", got)
	}
}

func TestUtilizationSummary(t *testing.T) {
	got := utilizationSummary(&computev1.NodeUtilization{
		TimestampNs: 1_000_000_000,
		Cpu: &computev1.CpuUtilization{
			OverallPercent: 12.5,
			PerCorePercent: []float64{10, 15},
			Load_1M:        1.2,
			Load_5M:        1.1,
			Load_15M:       1.0,
		},
		Memory: &computev1.MemoryUtilization{
			UsedBytes:      4 * 1024 * 1024 * 1024,
			AvailableBytes: 12 * 1024 * 1024 * 1024,
		},
		Gpus: []*computev1.GpuUtilization{{
			Index:              0,
			GpuPercent:         50,
			MemoryPercent:      25,
			VramUsedBytes:      6 * 1024 * 1024 * 1024,
			VramTotalBytes:     24 * 1024 * 1024 * 1024,
			TemperatureCelsius: 60,
			PowerDrawWatts:     100,
			PowerLimitWatts:    300,
		}},
	})

	if got == nil {
		t.Fatal("expected non-nil utilization summary")
	}
	if got.CPU == nil || got.CPU.OverallPercent != 12.5 {
		t.Fatalf("unexpected CPU summary: %#v", got.CPU)
	}
	if got.CPU.Load1 != 1.2 || got.CPU.Load5 != 1.1 || got.CPU.Load15 != 1.0 {
		t.Errorf("unexpected load averages: %#v", got.CPU)
	}
	if got.Memory == nil || got.Memory.UsedGB != 4 || got.Memory.AvailableGB != 12 || got.Memory.TotalGB != 16 {
		t.Errorf("unexpected memory summary: %#v", got.Memory)
	}
	if len(got.GPUs) != 1 || got.GPUs[0].GPUPercent != 50 || got.GPUs[0].VRAMUsedGB != 6 {
		t.Errorf("unexpected GPU summary: %#v", got.GPUs)
	}
}

func TestNodesHandler_IncludesUtilization(t *testing.T) {
	reg := registry.NewNodeRegistry()
	reg.Register("node-1", "", &computev1.NodeResources{NodeId: "node-1", Hostname: "worker-1"})
	reg.UpdateHeartbeat("node-1", reg.ConnectionID("node-1"), &computev1.NodeUtilization{
		Cpu: &computev1.CpuUtilization{OverallPercent: 33.3},
	}, nil)

	api := NewAPIServer(reg, "", false)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	rec := httptest.NewRecorder()
	api.NodesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var got []NodeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 node, got %d", len(got))
	}
	if got[0].Utilization == nil || got[0].Utilization.CPU == nil {
		t.Fatalf("expected utilization in response, got %#v", got[0].Utilization)
	}
	if got[0].Utilization.CPU.OverallPercent != 33.3 {
		t.Errorf("expected CPU 33.3, got %f", got[0].Utilization.CPU.OverallPercent)
	}
}

func TestGPUSummary_Empty(t *testing.T) {
	if got := gpuSummary(nil); got == nil {
		t.Error("expected non-nil empty slice for nil input")
	} else if len(got) != 0 {
		t.Errorf("expected empty slice, got %d GPUs", len(got))
	}
	if got := gpuSummary([]*computev1.GpuResources{}); got == nil {
		t.Error("expected non-nil empty slice for empty input")
	} else if len(got) != 0 {
		t.Errorf("expected empty slice, got %d GPUs", len(got))
	}
}
