// Package host provides host machine telemetry using gopsutil.
// It collects system-level information about the host machine.
package host

import (
	"context"
	"fmt"
	"runtime"

	"github.com/shirou/gopsutil/v3/cpu"
	hostinfo "github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
)

// HostInfo contains information about the host machine.
type HostInfo struct {
	// MachineID is a unique identifier for this machine
	// On Linux, this typically comes from /etc/machine-id
	// On Windows, this comes from the registry
	MachineID string `json:"machine_id"`

	// Hostname is the system hostname
	Hostname string `json:"hostname"`

	// OS is the operating system (e.g., "linux", "windows", "darwin")
	OS string `json:"os"`

	// Platform provides more specific OS information (e.g., "ubuntu", "debian")
	Platform string `json:"platform"`

	// PlatformVersion is the version of the platform (e.g., "22.04" for Ubuntu)
	PlatformVersion string `json:"platform_version"`

	// KernelVersion is the kernel/OS version
	KernelVersion string `json:"kernel_version"`

	// KernelArch is the kernel architecture (e.g., "x86_64", "aarch64")
	KernelArch string `json:"kernel_arch"`

	// TotalRAM is the total system memory in gigabytes
	TotalRAM float64 `json:"total_ram_gb"`

	// AvailableRAM is the available system memory in gigabytes
	AvailableRAM float64 `json:"available_ram_gb"`

	// UsedRAM is the used system memory in gigabytes
	UsedRAM float64 `json:"used_ram_gb"`

	// RAMUsagePercent is the percentage of RAM currently in use
	RAMUsagePercent float64 `json:"ram_usage_percent"`

	// CPUCores is the number of physical CPU cores
	CPUCores int `json:"cpu_cores"`

	// CPUThreads is the number of logical CPU threads (includes hyperthreading)
	CPUThreads int `json:"cpu_threads"`

	// CPUModel is the CPU model name (first CPU if multiple)
	CPUModel string `json:"cpu_model"`

	// Uptime is the system uptime in seconds
	Uptime uint64 `json:"uptime_seconds"`
}

// Collector is the interface for host telemetry collection.
// Using an interface allows for easy mocking in unit tests.
type Collector interface {
	// Collect gathers host machine telemetry.
	// It respects the provided context for cancellation/timeout.
	Collect(ctx context.Context) (*HostInfo, error)
}

// GopsutilCollector implements Collector using the gopsutil library.
type GopsutilCollector struct{}

// NewGopsutilCollector creates a new gopsutil-based host collector.
func NewGopsutilCollector() *GopsutilCollector {
	return &GopsutilCollector{}
}

// Collect implements the Collector interface.
// It uses gopsutil to gather comprehensive host information.
func (c *GopsutilCollector) Collect(ctx context.Context) (*HostInfo, error) {
	// Check context before starting
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("host collection cancelled: %w", ctx.Err())
	default:
	}

	info := &HostInfo{
		OS: runtime.GOOS, // Use Go's runtime for base OS info
	}

	// Get host information (hostname, machine ID, platform details)
	hostStat, err := hostinfo.InfoWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get host info: %w", err)
	}

	info.Hostname = hostStat.Hostname
	info.Platform = hostStat.Platform
	info.PlatformVersion = hostStat.PlatformVersion
	info.KernelVersion = hostStat.KernelVersion
	info.KernelArch = hostStat.KernelArch
	info.Uptime = hostStat.Uptime

	// Get machine ID
	// The HostID from gopsutil provides a unique machine identifier
	info.MachineID = hostStat.HostID

	// Get memory information
	memStat, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get memory info: %w", err)
	}

	// Convert bytes to gigabytes for readability
	info.TotalRAM = float64(memStat.Total) / (1024 * 1024 * 1024)
	info.AvailableRAM = float64(memStat.Available) / (1024 * 1024 * 1024)
	info.UsedRAM = float64(memStat.Used) / (1024 * 1024 * 1024)
	info.RAMUsagePercent = memStat.UsedPercent

	// Get CPU information
	// Physical cores count
	physicalCores, err := cpu.CountsWithContext(ctx, false)
	if err != nil {
		// Non-fatal error - we can continue without this
		physicalCores = 0
	}
	info.CPUCores = physicalCores

	// Logical cores count (includes hyperthreading)
	logicalCores, err := cpu.CountsWithContext(ctx, true)
	if err != nil {
		// Fallback to runtime.NumCPU()
		logicalCores = runtime.NumCPU()
	}
	info.CPUThreads = logicalCores

	// Get CPU model name
	cpuInfos, err := cpu.InfoWithContext(ctx)
	if err == nil && len(cpuInfos) > 0 {
		info.CPUModel = cpuInfos[0].ModelName
	}

	return info, nil
}
