package telemetry

import (
	"context"
	"fmt"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
)

// TelemetryData contains the collected system stats
type TelemetryData struct {
	CPUPercent float64
	RAMPercent float64
	Uptime     string
}

// Collector defines the interface for collecting telemetry data
type Collector interface {
	Collect(ctx context.Context) (*TelemetryData, error)
}

// GopsutilCollector implements Collector using gopsutil
type GopsutilCollector struct {
	// bootTimeCached stores boot time to avoid syscalls on every collection
	bootTimeCached uint64
}

// NewGopsutilCollector creates a new telemetry collector
func NewGopsutilCollector() *GopsutilCollector {
	return &GopsutilCollector{}
}

// Collect gathers the current system statistics
func (c *GopsutilCollector) Collect(ctx context.Context) (*TelemetryData, error) {
	data := &TelemetryData{}

	// 1. CPU Usage (total across all cores)
	// We use 0 as duration to get the instantaneous value since the last call,
	// or if it's the first call, it might return 0. Ideally, we should measure over a small window,
	// but for "instant" feel, 0 is often used with gopsutil, though it requires persistent state to be accurate.
	// Actually, gopsutil cpu.Percent(0, false) returns the usage since the last call.
	// However, calling it with 0 interval immediately after start might return 0.
	// For a 3s heartbeat, we can just call it.
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
	// We can try to get cached boot time or fetch it afresh
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
