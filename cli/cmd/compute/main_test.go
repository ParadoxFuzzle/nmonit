package main

import "testing"

func TestFormatCPUAndMemory(t *testing.T) {
	n := node{
		CPU:    cpuResources{PhysicalCores: 8, LogicalCores: 16},
		Memory: memoryResources{TotalGB: 32},
		Utilization: &nodeUtilization{
			CPU:    &cpuUtilization{OverallPercent: 12.5},
			Memory: &memoryUtilization{UsedGB: 8, TotalGB: 32},
		},
	}

	if got := formatCPU(n); got != "12.5% (8/16)" {
		t.Errorf("unexpected CPU format: %q", got)
	}
	if got := formatMemory(n); got != "8.0/32.0 GB" {
		t.Errorf("unexpected memory format: %q", got)
	}
}

func TestFormatGPUs(t *testing.T) {
	n := node{
		GPUs: []gpuResources{{Index: 0}},
		Utilization: &nodeUtilization{
			GPUs: []gpuUtilization{{
				Index:       0,
				GPUPercent:  50,
				VRAMUsedGB:  6,
				VRAMTotalGB: 24,
			}},
		},
	}

	if got := formatGPUs(n); got != "0: 50% 6.0/24.0 GB" {
		t.Errorf("unexpected GPU format: %q", got)
	}
}

func TestFormatGPUsWithoutUtilization(t *testing.T) {
	n := node{GPUs: []gpuResources{{Index: 0}, {Index: 1}}}
	if got := formatGPUs(n); got != "2" {
		t.Errorf("expected static GPU count, got %q", got)
	}
}
