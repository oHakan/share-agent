//go:build linux

// Package gpu provides NVIDIA GPU discovery functionality using NVML.
// It detects all available NVIDIA GPUs and returns their specifications.
// This file is only built on Linux where NVML is fully supported.
package gpu

import (
	"context"
	"fmt"
	"sync"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// NVMLDiscoverer implements Discoverer using the NVML library.
type NVMLDiscoverer struct {
	// initialized tracks whether NVML has been initialized
	initialized bool

	// mu protects the initialized state
	mu sync.Mutex
}

// NewNVMLDiscoverer creates a new NVML-based GPU discoverer.
func NewNVMLDiscoverer() *NVMLDiscoverer {
	return &NVMLDiscoverer{}
}

// Discover implements the Discoverer interface.
// It initializes NVML, enumerates all GPUs, and collects their properties.
//
// IMPORTANT: NVML operations involve C library calls with pointer handling.
// The go-nvml library wraps these safely, but we still need to:
// 1. Always initialize NVML before any operations
// 2. Always shutdown NVML when done
// 3. Handle errors from each NVML call individually
func (d *NVMLDiscoverer) Discover(ctx context.Context) (*DiscoveryResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	result := &DiscoveryResult{
		GPUs:        make([]GPUInfo, 0),
		CPUOnlyMode: false,
	}

	// Check context before starting
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("gpu discovery cancelled: %w", ctx.Err())
	default:
	}

	// Initialize NVML library
	// This must be called before any other NVML operations
	// It loads the NVIDIA driver and establishes communication
	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		// If NVML fails to initialize, we're in CPU-only mode
		// This is not necessarily an error - the machine may not have NVIDIA GPUs
		errMsg := nvml.ErrorString(ret)
		result.CPUOnlyMode = true
		result.Error = fmt.Sprintf("NVML initialization failed: %s (running in CPU-only mode)", errMsg)
		return result, nil // Return result, not error - this is a valid state
	}
	d.initialized = true

	// Get NVML version for diagnostics
	nvmlVersion, ret := nvml.SystemGetNVMLVersion()
	if ret == nvml.SUCCESS {
		result.NVMLVersion = nvmlVersion
	}

	// Get driver version (system-wide, not per-GPU)
	driverVersion, ret := nvml.SystemGetDriverVersion()
	if ret == nvml.SUCCESS {
		result.DriverVersion = driverVersion
	}

	// Get the number of GPU devices
	// This includes all NVIDIA GPUs visible to NVML
	deviceCount, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to get device count: %s", nvml.ErrorString(ret))
	}

	if deviceCount == 0 {
		result.CPUOnlyMode = true
		result.Error = "No NVIDIA GPUs found (running in CPU-only mode)"
		return result, nil
	}

	// Enumerate and collect information for each GPU
	for i := 0; i < deviceCount; i++ {
		// Check context periodically for long-running operations
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("gpu discovery cancelled during enumeration: %w", ctx.Err())
		default:
		}

		gpuInfo, err := d.getDeviceInfo(i)
		if err != nil {
			// Log individual device errors but continue with other GPUs
			gpuInfo = &GPUInfo{
				Index: i,
				Name:  fmt.Sprintf("GPU %d (error: %s)", i, err.Error()),
			}
		}
		gpuInfo.DriverVersion = driverVersion
		result.GPUs = append(result.GPUs, *gpuInfo)
	}

	return result, nil
}

// getDeviceInfo retrieves detailed information for a single GPU device.
func (d *NVMLDiscoverer) getDeviceInfo(index int) (*GPUInfo, error) {
	// Get device handle by index
	// The handle is an opaque pointer used for subsequent operations
	device, ret := nvml.DeviceGetHandleByIndex(index)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to get device handle: %s", nvml.ErrorString(ret))
	}

	info := &GPUInfo{
		Index: index,
	}

	// Get device name (e.g., "NVIDIA GeForce RTX 4090")
	name, ret := device.GetName()
	if ret == nvml.SUCCESS {
		info.Name = name
	}

	// Get UUID (unique identifier for this specific GPU)
	uuid, ret := device.GetUUID()
	if ret == nvml.SUCCESS {
		info.UUID = uuid
	}

	// Get memory information
	// memInfo contains total and free memory in bytes
	memInfo, ret := device.GetMemoryInfo()
	if ret == nvml.SUCCESS {
		// Convert bytes to gigabytes for readability
		info.TotalVRAM = float64(memInfo.Total) / (1024 * 1024 * 1024)
		info.FreeVRAM = float64(memInfo.Free) / (1024 * 1024 * 1024)
		info.UsedVRAM = float64(memInfo.Used) / (1024 * 1024 * 1024)
	}

	// Get compute capability (CUDA version support indicator)
	// This returns major and minor version (e.g., 8, 9 for Compute Capability 8.9)
	major, minor, ret := device.GetCudaComputeCapability()
	if ret == nvml.SUCCESS {
		info.ComputeCapability = fmt.Sprintf("%d.%d", major, minor)
	}

	// Get current GPU temperature
	temp, ret := device.GetTemperature(nvml.TEMPERATURE_GPU)
	if ret == nvml.SUCCESS {
		info.Temperature = int(temp)
	}

	// Get current power usage in milliwatts
	power, ret := device.GetPowerUsage()
	if ret == nvml.SUCCESS {
		// Convert milliwatts to watts
		info.PowerUsage = float64(power) / 1000
	}

	return info, nil
}

// GetUsageStats returns real-time usage statistics.
// TODO: Implement using NVML similar to Discover
func (d *NVMLDiscoverer) GetUsageStats(ctx context.Context) ([]GPUUsageStats, error) {
	// For now return empty stats to satisfy interface
	return nil, nil
}

// Close shuts down NVML and releases all resources.
// This should be called when GPU discovery is no longer needed.
func (d *NVMLDiscoverer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.initialized {
		return nil
	}

	ret := nvml.Shutdown()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to shutdown NVML: %s", nvml.ErrorString(ret))
	}

	d.initialized = false
	return nil
}
