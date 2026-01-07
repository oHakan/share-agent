// Package limits provides thread-safe storage for container resource limits.
// Limits are received from the orchestrator and enforced when creating Docker containers.
package limits

import (
	"sync"

	pb "github.com/depin-agent/agent/proto"
)

// Default limits when orchestrator doesn't send any
const (
	DefaultMaxCPU      = 1.0 // 1 CPU core
	DefaultMaxRAMGB    = 0.5 // 512 MB
	DefaultMaxVolumeGB = 10  // 10 GB
)

// Store holds the current resource limits for this node.
// All methods are thread-safe.
type Store struct {
	MaxCPU      float64 // Max CPU cores (e.g., 2.0 for 2 cores)
	MaxRAMGB    float64 // Max RAM in GB
	MaxVolumeGB int64   // Max disk space in GB
	mu          sync.RWMutex
}

// NewStore creates a new limits store with default values.
func NewStore() *Store {
	return &Store{
		MaxCPU:      DefaultMaxCPU,
		MaxRAMGB:    DefaultMaxRAMGB,
		MaxVolumeGB: DefaultMaxVolumeGB,
	}
}

// Update updates the limits from a proto ResourceLimits message.
// If the provided limits are nil, this is a no-op.
func (s *Store) Update(limits *pb.ResourceLimits) {
	if limits == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Only update if values are positive (0 means not set)
	if limits.MaxCpu > 0 {
		s.MaxCPU = limits.MaxCpu
	}
	if limits.MaxRamGb > 0 {
		s.MaxRAMGB = limits.MaxRamGb
	}
	if limits.MaxVolumeGb > 0 {
		s.MaxVolumeGB = limits.MaxVolumeGb
	}
}

// Get returns the current limits.
func (s *Store) Get() (maxCPU float64, maxRAMGB float64, maxVolumeGB int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.MaxCPU, s.MaxRAMGB, s.MaxVolumeGB
}

// ClampCPU clamps the requested CPU to the maximum allowed.
// Returns the clamped value.
func (s *Store) ClampCPU(requestedCPU float64) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if requestedCPU <= 0 || requestedCPU > s.MaxCPU {
		return s.MaxCPU
	}
	return requestedCPU
}

// ClampMemoryMB clamps the requested memory (in MB) to the maximum allowed.
// Returns the clamped value in MB.
func (s *Store) ClampMemoryMB(requestedMB int64) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	maxMB := int64(s.MaxRAMGB * 1024)
	if requestedMB <= 0 || requestedMB > maxMB {
		return maxMB
	}
	return requestedMB
}

// Clamp clamps both CPU and memory to their maximum allowed values.
// Memory is provided and returned in MB.
func (s *Store) Clamp(requestedCPU float64, requestedMemoryMB int64) (cpu float64, memoryMB int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Clamp CPU
	if requestedCPU <= 0 || requestedCPU > s.MaxCPU {
		cpu = s.MaxCPU
	} else {
		cpu = requestedCPU
	}

	// Clamp Memory
	maxMB := int64(s.MaxRAMGB * 1024)
	if requestedMemoryMB <= 0 || requestedMemoryMB > maxMB {
		memoryMB = maxMB
	} else {
		memoryMB = requestedMemoryMB
	}

	return cpu, memoryMB
}
