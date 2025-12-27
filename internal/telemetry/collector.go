package telemetry

import (
	"context"
	"fmt"
	"time"

	"github.com/depin-agent/agent/internal/hardware/gpu"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
)

// TelemetryData contains the collected system stats
type TelemetryData struct {
	CPUPercent  float64
	RAMPercent  float64
	Uptime      string
	GPUPercent  float64
	VRAMPercent float64
	VRAMTotal   uint64
	VRAMFree    uint64
	GPUModel    string
}

// Collector defines the interface for collecting telemetry data
type Collector interface {
	Collect(ctx context.Context) (*TelemetryData, error)
}

// GopsutilCollector implements Collector using gopsutil
type GopsutilCollector struct {
	// bootTimeCached stores boot time to avoid syscalls on every collection
	bootTimeCached uint64
	gpuProvider    gpu.Discoverer
}

// NewGopsutilCollector creates a new telemetry collector
func NewGopsutilCollector(gpuProvider gpu.Discoverer) *GopsutilCollector {
	return &GopsutilCollector{
		gpuProvider: gpuProvider,
	}
}

// Collect gathers the current system statistics
func (c *GopsutilCollector) Collect(ctx context.Context) (*TelemetryData, error) {
	data := &TelemetryData{}

	// 1. CPU Usage (total across all cores)
	cpuPers, err := cpu.PercentWithContext(ctx, 0, false)
	if err != nil {
		return nil, fmt.Errorf("failed to get cpu stats: %w", err)
	}
	if len(cpuPers) > 0 {
		data.CPUPercent = cpuPers[0]
	}

	// 2. RAM Usage
	vmStat, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get memory stats: %w", err)
	}
	data.RAMPercent = vmStat.UsedPercent

	// 3. Uptime
	bootTime := c.bootTimeCached
	if bootTime == 0 {
		bt, err := host.BootTimeWithContext(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get boot time: %w", err)
		}
		c.bootTimeCached = bt
		bootTime = bt
	}

	uptimeSeconds := uint64(time.Now().Unix()) - bootTime
	data.Uptime = formatUptime(uptimeSeconds)

	// 4. GPU Usage
	if c.gpuProvider != nil {
		stats, err := c.gpuProvider.GetUsageStats(ctx)
		if err == nil && len(stats) > 0 {
			// Report the first GPU's stats for now
			// The proto currently supports single GPU stats in Heartbeat
			gpuStat := stats[0]
			data.GPUPercent = gpuStat.Utilization
			data.GPUModel = gpuStat.Name
			data.VRAMTotal = gpuStat.TotalVRAM
			data.VRAMFree = gpuStat.FreeVRAM

			if gpuStat.TotalVRAM > 0 {
				data.VRAMPercent = (float64(gpuStat.UsedVRAM) / float64(gpuStat.TotalVRAM)) * 100
			}
		}
	}

	return data, nil
}

// formatUptime converts seconds to human readable string using standard library only
func formatUptime(seconds uint64) string {
	d := time.Duration(seconds) * time.Second

	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60

	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm %ds", days, hours, minutes, secs)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, secs)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, secs)
	}
	return fmt.Sprintf("%ds", secs)
}
