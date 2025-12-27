// Package gpu provides NVIDIA GPU discovery functionality.
// It defines interfaces and types for GPU discovery that can be implemented
// differently based on the target platform.
package gpu

import (
	"context"
)

// GPUInfo contains information about a single NVIDIA GPU.
type GPUInfo struct {
	// Index is the GPU index as reported by NVML (0-based)
	Index int `json:"index"`

	// Name is the product name of the GPU (e.g., "NVIDIA GeForce RTX 4090")
	Name string `json:"name"`

	// TotalVRAM is the total video memory in gigabytes
	TotalVRAM float64 `json:"total_vram_gb"`

	// FreeVRAM is the currently available video memory in gigabytes
	FreeVRAM float64 `json:"free_vram_gb"`

	// UsedVRAM is the currently used video memory in gigabytes
	UsedVRAM float64 `json:"used_vram_gb"`

	// DriverVersion is the installed NVIDIA driver version
	DriverVersion string `json:"driver_version"`

	// ComputeCapability is the CUDA compute capability (e.g., "8.9")
	ComputeCapability string `json:"compute_capability"`

	// UUID is the unique identifier for this GPU
	UUID string `json:"uuid"`

	// Temperature is the current GPU temperature in Celsius
	Temperature int `json:"temperature_celsius"`

	// PowerUsage is the current power consumption in watts
	PowerUsage float64 `json:"power_usage_watts"`
}

// DiscoveryResult contains the result of GPU discovery.
type DiscoveryResult struct {
	// GPUs is the list of discovered GPUs
	GPUs []GPUInfo `json:"gpus"`

	// DriverVersion is the system-wide NVIDIA driver version
	DriverVersion string `json:"driver_version"`

	// NVMLVersion is the NVML library version
	NVMLVersion string `json:"nvml_version"`

	// Error contains any error message if discovery failed
	Error string `json:"error,omitempty"`

	// CPUOnlyMode indicates whether the system is running without GPU
	CPUOnlyMode bool `json:"cpu_only_mode"`
}

// Discoverer is the interface for GPU discovery implementations.
// Using an interface allows for easy mocking in unit tests.
type Discoverer interface {
	// Discover detects all available GPUs and returns their information.
	// It respects the provided context for cancellation/timeout.
	Discover(ctx context.Context) (*DiscoveryResult, error)

	// GetUsageStats returns real-time usage statistics for all GPUs.
	GetUsageStats(ctx context.Context) ([]GPUUsageStats, error)

	// Close releases any resources held by the discoverer.
	Close() error
}

// GPUUsageStats contains real-time usage metrics for a GPU.
type GPUUsageStats struct {
	Index       int     `json:"index"`
	Name        string  `json:"name"`
	Utilization float64 `json:"utilization_percent"` // GPU Utilization (0-100)
	UsedVRAM    uint64  `json:"used_vram_bytes"`
	TotalVRAM   uint64  `json:"total_vram_bytes"`
	FreeVRAM    uint64  `json:"free_vram_bytes"`
}
