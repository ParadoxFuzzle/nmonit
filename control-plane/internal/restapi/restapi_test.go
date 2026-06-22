package restapi

import (
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
