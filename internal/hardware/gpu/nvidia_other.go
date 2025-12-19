//go:build !linux && !windows

// Package gpu provides NVIDIA GPU discovery functionality.
// This file is built on platforms other than Linux and Windows (e.g., macOS)
// where NVML is not available.
package gpu

import (
	"context"
	"runtime"
	"sync"
)

// NVMLDiscoverer implements Discoverer for unsupported platforms.
type NVMLDiscoverer struct {
	mu sync.Mutex
}

// NewNVMLDiscoverer creates a new GPU discoverer.
func NewNVMLDiscoverer() *NVMLDiscoverer {
	return &NVMLDiscoverer{}
}

// Discover implements the Discoverer interface.
// On unsupported platforms, it returns CPU-only mode.
func (d *NVMLDiscoverer) Discover(ctx context.Context) (*DiscoveryResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	return &DiscoveryResult{
		GPUs:        make([]GPUInfo, 0),
		CPUOnlyMode: true,
		Error:       "NVIDIA GPU detection not supported on " + runtime.GOOS + " (running in CPU-only mode)",
	}, nil
}

// Close releases any resources. No-op on unsupported platforms.
func (d *NVMLDiscoverer) Close() error {
	return nil
}
