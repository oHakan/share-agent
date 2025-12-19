// Package docker provides Docker daemon connectivity checking.
// It verifies Docker availability and returns runtime information.
package docker

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// DockerInfo contains information about Docker availability and version.
type DockerInfo struct {
	// Available indicates whether Docker daemon is reachable
	Available bool `json:"available"`

	// APIVersion is the Docker API version (e.g., "1.43")
	APIVersion string `json:"api_version,omitempty"`

	// ServerVersion is the Docker Engine version (e.g., "24.0.7")
	ServerVersion string `json:"server_version,omitempty"`

	// OSType is the operating system type reported by Docker (e.g., "linux")
	OSType string `json:"os_type,omitempty"`

	// Architecture is the host architecture (e.g., "x86_64")
	Architecture string `json:"architecture,omitempty"`

	// ContainersRunning is the number of currently running containers
	ContainersRunning int `json:"containers_running,omitempty"`

	// ContainersPaused is the number of paused containers
	ContainersPaused int `json:"containers_paused,omitempty"`

	// ContainersStopped is the number of stopped containers
	ContainersStopped int `json:"containers_stopped,omitempty"`

	// Error contains error details if Docker is unavailable
	Error string `json:"error,omitempty"`

	// ResponseTime is how long the ping took (for health monitoring)
	ResponseTime time.Duration `json:"response_time_ms,omitempty"`
}

// Checker is the interface for Docker availability checking.
// Using an interface allows for easy mocking in unit tests.
type Checker interface {
	// Check verifies Docker daemon connectivity and returns runtime info.
	// It respects the provided context for cancellation/timeout.
	Check(ctx context.Context) (*DockerInfo, error)

	// Close releases any resources held by the checker.
	Close() error
}

// DockerChecker implements Checker using the official Docker client.
type DockerChecker struct {
	// client is the Docker API client
	client *client.Client
}

// NewDockerChecker creates a new Docker checker.
// It creates a client from environment variables (DOCKER_HOST, etc.)
// This doesn't actually connect to Docker - that happens on Check().
func NewDockerChecker() (*DockerChecker, error) {
	// Create Docker client from environment
	// This uses DOCKER_HOST, DOCKER_TLS_VERIFY, DOCKER_CERT_PATH
	// On Windows with Docker Desktop, this typically uses named pipes
	// On Linux, this typically uses unix:///var/run/docker.sock
	//
	// IMPORTANT: client.FromEnv() negotiates the API version automatically
	// which prevents "client version X is too new" errors
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	return &DockerChecker{
		client: cli,
	}, nil
}

// Check implements the Checker interface.
// It pings the Docker daemon and collects runtime information.
//
// IMPORTANT: Context usage here is critical for timeout handling.
// The context allows the caller to set a deadline for Docker operations,
// preventing hangs if the Docker daemon is unresponsive.
func (c *DockerChecker) Check(ctx context.Context) (*DockerInfo, error) {
	info := &DockerInfo{
		Available: false,
	}

	// Record start time for response time measurement
	startTime := time.Now()

	// Ping the Docker daemon
	// This is a lightweight operation that verifies connectivity
	_, err := c.client.Ping(ctx)
	if err != nil {
		info.Error = fmt.Sprintf("Docker ping failed: %v", err)
		return info, nil // Return info with error, not actual error
	}

	// Record response time
	info.ResponseTime = time.Since(startTime)
	info.Available = true

	// Get Docker API version from client
	info.APIVersion = c.client.ClientVersion()

	// Get more detailed system information
	// This provides server version, container counts, etc.
	systemInfo, err := c.client.Info(ctx)
	if err != nil {
		// Non-fatal - we already know Docker is available
		info.Error = fmt.Sprintf("Warning: could not get system info: %v", err)
	} else {
		info.ServerVersion = systemInfo.ServerVersion
		info.OSType = systemInfo.OSType
		info.Architecture = systemInfo.Architecture
		info.ContainersRunning = systemInfo.ContainersRunning
		info.ContainersPaused = systemInfo.ContainersPaused
		info.ContainersStopped = systemInfo.Containers - systemInfo.ContainersRunning - systemInfo.ContainersPaused
	}

	return info, nil
}

// CheckWithImages also retrieves information about available images.
// This is an extended check for more comprehensive runtime analysis.
func (c *DockerChecker) CheckWithImages(ctx context.Context) (*DockerInfo, []image.Summary, error) {
	// First, do the standard check
	info, err := c.Check(ctx)
	if err != nil || !info.Available {
		return info, nil, err
	}

	// List images using the image package types
	images, err := c.client.ImageList(ctx, image.ListOptions{
		All: false, // Only show images with containers or tags
	})
	if err != nil {
		return info, nil, fmt.Errorf("failed to list images: %w", err)
	}

	return info, images, nil
}

// Close releases Docker client resources.
func (c *DockerChecker) Close() error {
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}
