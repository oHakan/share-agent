//go:build windows

// Package gpu provides NVIDIA GPU discovery functionality for Windows.
// On Windows, we use nvidia-smi CLI tool which comes with NVIDIA drivers.
package gpu

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
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

// nvidiaSmiOutput represents the XML output structure from nvidia-smi
type nvidiaSmiOutput struct {
	XMLName       xml.Name       `xml:"nvidia_smi_log"`
	DriverVersion string         `xml:"driver_version"`
	CUDAVersion   string         `xml:"cuda_version"`
	GPUs          []nvidiaSmiGPU `xml:"gpu"`
}

type nvidiaSmiGPU struct {
	ID                string   `xml:"id,attr"`
	ProductName       string   `xml:"product_name"`
	UUID              string   `xml:"uuid"`
	FBMemoryUsage     fbMemory `xml:"fb_memory_usage"`
	Temperature       gpuTemp  `xml:"temperature"`
	PowerReadings     power    `xml:"gpu_power_readings"`
	ComputeCapability string   `xml:"cuda_compute_capability"`
}

type fbMemory struct {
	Total string `xml:"total"`
	Used  string `xml:"used"`
	Free  string `xml:"free"`
}

type gpuTemp struct {
	GPUTemp string `xml:"gpu_temp"`
}

type power struct {
	PowerDraw string `xml:"power_draw"`
}

// Discover implements the Discoverer interface for Windows.
// It uses nvidia-smi CLI to query GPU information.
//
// nvidia-smi is installed with NVIDIA drivers and provides comprehensive
// GPU information in XML format when called with -q -x flags.
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

	// Try to run nvidia-smi with XML output
	// nvidia-smi -q -x gives us detailed XML output
	cmd := exec.CommandContext(ctx, "nvidia-smi", "-q", "-x")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		// nvidia-smi not found or failed - likely no NVIDIA GPU or drivers
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

	// Parse XML output
	var smiOutput nvidiaSmiOutput
	if err := xml.Unmarshal(stdout.Bytes(), &smiOutput); err != nil {
		result.CPUOnlyMode = true
		result.Error = fmt.Sprintf("failed to parse nvidia-smi output: %v (running in CPU-only mode)", err)
		return result, nil
	}

	// Set driver version
	result.DriverVersion = smiOutput.DriverVersion
	result.NVMLVersion = fmt.Sprintf("CUDA %s", smiOutput.CUDAVersion)

	// No GPUs found
	if len(smiOutput.GPUs) == 0 {
		result.CPUOnlyMode = true
		result.Error = "No NVIDIA GPUs found (running in CPU-only mode)"
		return result, nil
	}

	// Parse each GPU
	for i, smiGPU := range smiOutput.GPUs {
		gpuInfo := GPUInfo{
			Index:         i,
			Name:          smiGPU.ProductName,
			UUID:          smiGPU.UUID,
			DriverVersion: smiOutput.DriverVersion,
		}

		// Parse VRAM (format: "24576 MiB" or "24 GiB")
		gpuInfo.TotalVRAM = parseMemoryToGB(smiGPU.FBMemoryUsage.Total)
		gpuInfo.UsedVRAM = parseMemoryToGB(smiGPU.FBMemoryUsage.Used)
		gpuInfo.FreeVRAM = parseMemoryToGB(smiGPU.FBMemoryUsage.Free)

		// Parse temperature (format: "45 C")
		gpuInfo.Temperature = parseTemperature(smiGPU.Temperature.GPUTemp)

		// Parse power (format: "150.00 W")
		gpuInfo.PowerUsage = parsePower(smiGPU.PowerReadings.PowerDraw)

		// Compute capability (format: "8.9")
		gpuInfo.ComputeCapability = smiGPU.ComputeCapability

		result.GPUs = append(result.GPUs, gpuInfo)
	}

	return result, nil
}

// parseMemoryToGB converts memory strings like "24576 MiB" to GB
func parseMemoryToGB(mem string) float64 {
	mem = strings.TrimSpace(mem)
	if mem == "" || mem == "N/A" {
		return 0
	}

	parts := strings.Fields(mem)
	if len(parts) < 2 {
		return 0
	}

	value, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0
	}

	unit := strings.ToUpper(parts[1])
	switch unit {
	case "MIB", "MB":
		return value / 1024
	case "GIB", "GB":
		return value
	case "KIB", "KB":
		return value / (1024 * 1024)
	default:
		return value / 1024 // Assume MiB by default
	}
}

// parseTemperature converts temperature strings like "45 C" to int
func parseTemperature(temp string) int {
	temp = strings.TrimSpace(temp)
	if temp == "" || temp == "N/A" {
		return 0
	}

	parts := strings.Fields(temp)
	if len(parts) < 1 {
		return 0
	}

	value, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0
	}

	return value
}

// parsePower converts power strings like "150.00 W" to float64
func parsePower(power string) float64 {
	power = strings.TrimSpace(power)
	if power == "" || power == "N/A" {
		return 0
	}

	parts := strings.Fields(power)
	if len(parts) < 1 {
		return 0
	}

	value, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0
	}

	return value
}

// Close releases any resources. No-op as nvidia-smi is run on-demand.
func (d *NVMLDiscoverer) Close() error {
	return nil
}
