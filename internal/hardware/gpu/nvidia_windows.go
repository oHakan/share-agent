//go:build windows

// Package gpu provides NVIDIA GPU discovery functionality for Windows.
// On Windows, we use nvidia-smi CLI tool which comes with NVIDIA drivers.
package gpu

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// NVMLDiscoverer implements Discoverer for Windows using nvidia-smi CLI.
type NVMLDiscoverer struct {
	mu sync.Mutex
}

// NewNVMLDiscoverer creates a new GPU discoverer for Windows.
func NewNVMLDiscoverer() *NVMLDiscoverer {
	return &NVMLDiscoverer{}
}

// Discover implements the Discoverer interface for Windows.
// It uses nvidia-smi CLI to query GPU information.
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

	// Try to run nvidia-smi with CSV query
	// Command: nvidia-smi --query-gpu=name,memory.total,driver_version --format=csv,noheader,nounits
	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=name,memory.total,driver_version,compute_cap",
		"--format=csv,noheader,nounits")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		// If nvidia-smi fails, we check for dev mode or just return CPU-only mode
		// Ideally we would check config here, but we'll infer based on environment or just fallback
		isDevCtx := os.Getenv("DEPIN_DEV_MODE") == "true"

		if isDevCtx {
			// Mock GPU for development
			return &DiscoveryResult{
				GPUs: []GPUInfo{
					{
						Index:             0,
						Name:              "Mock NVIDIA GeForce RTX 4090",
						UUID:              "GPU-MOCK-1234-5678-90AB-CDEF",
						TotalVRAM:         24.0,
						UsedVRAM:          2.5,
						FreeVRAM:          21.5,
						Temperature:       45,
						PowerUsage:        150.0,
						DriverVersion:     "535.104",
						ComputeCapability: "8.9",
					},
				},
				DriverVersion: "535.104",
				NVMLVersion:   "Mock NVML",
				CPUOnlyMode:   false,
			}, nil
		}

		// Real failure - return CPU only mode
		result.CPUOnlyMode = true
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.Error = fmt.Sprintf("nvidia-smi failed (exit %d): %s (running in CPU-only mode)",
				exitErr.ExitCode(), strings.TrimSpace(stderr.String()))
		} else if strings.Contains(err.Error(), "executable file not found") {
			result.Error = "nvidia-smi not found - NVIDIA drivers may not be installed (running in CPU-only mode)"
		} else {
			result.Error = fmt.Sprintf("nvidia-smi error: %v (running in CPU-only mode)", err)
		}
		return result, nil
	}

	// Parse CSV output
	reader := csv.NewReader(&stdout)
	records, err := reader.ReadAll()
	if err != nil {
		result.CPUOnlyMode = true
		result.Error = fmt.Sprintf("failed to parse nvidia-smi output: %v", err)
		return result, nil
	}

	if len(records) == 0 {
		result.CPUOnlyMode = true
		result.Error = "No NVIDIA GPUs found (running in CPU-only mode)"
		return result, nil
	}

	// Parse each GPU record
	for i, record := range records {
		if len(record) < 3 {
			continue // Skip malformed lines
		}

		gpuInfo := GPUInfo{
			Index: i,
			Name:  strings.TrimSpace(record[0]),
		}

		// Parse Total Memory (MB) -> GB
		totalMemMB, err := strconv.ParseFloat(strings.TrimSpace(record[1]), 64)
		if err == nil {
			gpuInfo.TotalVRAM = totalMemMB / 1024.0
			// We only get Total from this query.
			// To get Used/Free we'd need more fields or separate query.
			// For registration, Total is most important.
			// Let's assume Free = Total for initial registration if we can't get it easily without more parsing complexity
			gpuInfo.FreeVRAM = gpuInfo.TotalVRAM
			gpuInfo.UsedVRAM = 0
		}

		// Driver Version
		if len(record) >= 3 {
			result.DriverVersion = strings.TrimSpace(record[2])
			gpuInfo.DriverVersion = result.DriverVersion
		}

		// Compute Cap
		if len(record) >= 4 {
			gpuInfo.ComputeCapability = strings.TrimSpace(record[3])
		}

		// Generate a pseudo-UUID since this query doesn't give it easily without -q -x
		// or we could add uuid to the query
		gpuInfo.UUID = fmt.Sprintf("GPU-%d-%s", i, gpuInfo.Name)

		result.GPUs = append(result.GPUs, gpuInfo)
	}

	// If we successfully parsed GPUs
	if len(result.GPUs) > 0 {
		result.CPUOnlyMode = false
	} else {
		result.CPUOnlyMode = true
		result.Error = "No valid GPUs parsed from output"
	}

	return result, nil
}

// GetUsageStats returns real-time usage statistics for all GPUs using nvidia-smi.
func (d *NVMLDiscoverer) GetUsageStats(ctx context.Context) ([]GPUUsageStats, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Check context before starting
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	// Command: nvidia-smi --query-gpu=index,name,utilization.gpu,memory.used,memory.total,memory.free --format=csv,noheader,nounits
	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=index,name,utilization.gpu,memory.used,memory.total,memory.free",
		"--format=csv,noheader,nounits")

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("nvidia-smi failed: %w", err)
	}

	reader := csv.NewReader(&stdout)
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("failed to parse nvidia-smi output: %w", err)
	}

	var stats []GPUUsageStats
	for _, record := range records {
		if len(record) < 6 {
			continue
		}

		s := GPUUsageStats{}

		// Parse Index
		if idx, err := strconv.Atoi(strings.TrimSpace(record[0])); err == nil {
			s.Index = idx
		}

		// Name
		s.Name = strings.TrimSpace(record[1])

		// Utilization (0-100)
		if util, err := strconv.ParseFloat(strings.TrimSpace(record[2]), 64); err == nil {
			s.Utilization = util
		}

		// memory.used (MB) -> bytes
		if used, err := strconv.ParseFloat(strings.TrimSpace(record[3]), 64); err == nil {
			s.UsedVRAM = uint64(used * 1024 * 1024)
		}

		// memory.total (MB) -> bytes
		if total, err := strconv.ParseFloat(strings.TrimSpace(record[4]), 64); err == nil {
			s.TotalVRAM = uint64(total * 1024 * 1024)
		}

		// memory.free (MB) -> bytes
		if free, err := strconv.ParseFloat(strings.TrimSpace(record[5]), 64); err == nil {
			s.FreeVRAM = uint64(free * 1024 * 1024)
		}

		stats = append(stats, s)
	}

	return stats, nil
}

// Close releases any resources. No-op as nvidia-smi is run on-demand.
func (d *NVMLDiscoverer) Close() error {
	return nil
}
