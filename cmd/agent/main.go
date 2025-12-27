// Package main is the entry point for the DePIN GPU Agent.
// It initializes all components, runs discovery, registers with orchestrator, and maintains connection.

// go run .\cmd\agent\ --owner=user_377268BpQY4RydCUqvTLu2R0wH3
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/depin-agent/agent/internal/client"
	"github.com/depin-agent/agent/internal/config"
	"github.com/depin-agent/agent/internal/hardware/gpu"
	"github.com/depin-agent/agent/internal/hardware/host"
	"github.com/depin-agent/agent/internal/runtime/docker"
	"github.com/depin-agent/agent/internal/telemetry"
	"github.com/depin-agent/agent/pkg/logger"
	pb "github.com/depin-agent/agent/proto"
)

// NodeCapacity aggregates all discovered capabilities of this node.
// This is the main data structure that will be reported to the marketplace.
type NodeCapacity struct {
	// Host contains host machine information
	Host *host.HostInfo `json:"host"`

	// GPUs contains information about all discovered NVIDIA GPUs
	GPUs *gpu.DiscoveryResult `json:"gpu_discovery"`

	// Docker contains Docker runtime information
	Docker *docker.DockerInfo `json:"docker"`

	// AgentVersion is the version of this agent
	AgentVersion string `json:"agent_version"`

	// DiscoveryTime is when this discovery was performed
	DiscoveryTime time.Time `json:"discovery_time"`

	// DiscoveryDuration is how long the discovery took
	DiscoveryDuration time.Duration `json:"discovery_duration_ms"`

	// Errors contains any non-fatal errors encountered during discovery
	Errors []string `json:"errors,omitempty"`
}

func main() {
	// Load configuration first
	cfg, err := config.Load()
	if err != nil {
		// Can't use logger yet, so use fmt
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	// Initialize logger based on configuration
	log, err := logger.New(cfg.DevMode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync(log)

	log.Info("Starting DePIN GPU Agent",
		zap.String("version", cfg.AgentVersion),
		zap.Bool("dev_mode", cfg.DevMode),
		zap.String("orchestrator", cfg.OrchestratorAddress),
	)

	// Setup graceful shutdown
	// This creates a context that will be cancelled on SIGINT or SIGTERM
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start shutdown listener in background
	go func() {
		sig := <-sigChan
		log.Info("Received shutdown signal",
			zap.String("signal", sig.String()),
		)
		cancel()
	}()

	// Run discovery
	capacity, err := runDiscovery(ctx, cfg, log)
	if err != nil {
		log.Fatal("Discovery failed",
			zap.Error(err),
		)
	}

	// Output results as pretty JSON
	outputJSON, err := json.MarshalIndent(capacity, "", "  ")
	if err != nil {
		log.Fatal("Failed to marshal results",
			zap.Error(err),
		)
	}

	// Log the complete node capacity
	log.Info("Node capacity discovery complete",
		zap.String("result", string(outputJSON)),
	)

	// Also print to stdout for easy viewing/piping
	fmt.Println("\n=== Node Capacity ===")
	fmt.Println(string(outputJSON))

	// --- Create Docker Executor for job execution ---
	executor, err := docker.NewExecutor(log)
	if err != nil {
		log.Error("Failed to create Docker executor",
			zap.Error(err),
		)
		// Continue without executor - we won't be able to run jobs
	}
	if executor != nil {
		defer executor.Close()
	}

	// --- Connect to Orchestrator and Register ---
	if err := registerAndStream(ctx, cfg, capacity, executor, log); err != nil {
		log.Error("Failed to register/stream with orchestrator",
			zap.Error(err),
		)
		// Don't exit - agent can still function in standalone mode
	}

	// Keep agent running until shutdown signal
	log.Info("Agent is running. Press Ctrl+C to stop.")
	<-ctx.Done()
	log.Info("Agent shutting down")
}

// registerAndStream connects to the orchestrator, registers this node,
// and starts the bidirectional event stream for job processing.
func registerAndStream(ctx context.Context, cfg *config.Config, capacity *NodeCapacity, executor *docker.Executor, log *zap.Logger) error {
	log.Info("Connecting to orchestrator",
		zap.String("address", cfg.OrchestratorAddress),
	)

	// Create gRPC client
	clientConfig := &client.ClientConfig{
		OrchestratorAddress: cfg.OrchestratorAddress,
		MaxRetries:          cfg.MaxRetries,
		InitialBackoff:      cfg.RetryBackoff,
		MaxBackoff:          30 * time.Second,
		ConnectionTimeout:   10 * time.Second,
		CallTimeout:         10 * time.Second,
	}

	grpcClient := client.NewClient(clientConfig, log)

	// Connect with retry
	if err := grpcClient.Connect(ctx); err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}

	// Convert discovered capacity to proto NodeInfo
	nodeInfo := capacityToNodeInfo(capacity, cfg, log)

	// Register with orchestrator
	resp, err := grpcClient.Register(ctx, nodeInfo)
	if err != nil {
		grpcClient.Close()
		return fmt.Errorf("registration failed: %w", err)
	}

	log.Info("Registration successful",
		zap.String("status", resp.Status),
		zap.String("message", resp.Message),
	)

	// Create job handler that uses the Docker executor
	jobHandler := createJobHandler(executor, log)

	// Initialize telemetry collector
	// Create a persistent GPU discoverer for telemetry
	gpuDiscoverer := gpu.NewNVMLDiscoverer()
	defer gpuDiscoverer.Close()

	telemetryCollector := telemetry.NewGopsutilCollector(gpuDiscoverer)

	// Create telemetry provider for heartbeats
	telemetryProvider := func() *pb.Heartbeat {
		// Collect real-time stats
		// Use a short timeout to not block heartbeat
		tCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		defer cancel()

		stats, err := telemetryCollector.Collect(tCtx)
		if err != nil {
			log.Warn("Failed to collect telemetry", zap.Error(err))
			// Return minimal heartbeat on error
			return &pb.Heartbeat{
				NodeId:    nodeInfo.Id,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			}
		}

		return &pb.Heartbeat{
			NodeId:      nodeInfo.Id,
			CpuPercent:  stats.CPUPercent,
			RamPercent:  stats.RAMPercent,
			Uptime:      stats.Uptime,
			GpuPercent:  stats.GPUPercent,
			VramPercent: stats.VRAMPercent,
			VramTotal:   stats.VRAMTotal,
			VramFree:    stats.VRAMFree,
			GpuModel:    stats.GPUModel,
			Timestamp:   time.Now().UTC().Format(time.RFC3339),
		}
	}

	// Start the bidirectional event stream in background
	// This handles both heartbeats and job execution
	go func() {
		heartbeatInterval := 3 * time.Second // 3 seconds heartbeat for live graphs

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			err := grpcClient.StreamEvents(ctx, nodeInfo, jobHandler, telemetryProvider, heartbeatInterval)
			if err != nil {
				if ctx.Err() != nil {
					// Context cancelled, shutting down
					return
				}
				log.Error("Stream error, reconnecting...",
					zap.Error(err),
				)
				time.Sleep(5 * time.Second)
				continue
			}
		}
	}()

	return nil
}

// createJobHandler creates a job handler function that uses the Docker executor.
func createJobHandler(executor *docker.Executor, log *zap.Logger) client.JobHandler {
	return func(ctx context.Context, req *pb.JobRequest, logSender func(string)) *pb.JobResult {
		result := &pb.JobResult{
			JobId: req.JobId,
		}

		// Check if we have an executor
		if executor == nil {
			result.Status = "failed"
			result.OutputLog = "Docker executor not available"
			log.Error("Job failed - no executor",
				zap.String("job_id", req.JobId),
			)
			return result
		}

		// Validate that we have script content to execute
		if req.ScriptContent == "" {
			result.Status = "failed"
			result.OutputLog = "No script content provided"
			log.Error("Job failed - no script content",
				zap.String("job_id", req.JobId),
			)
			return result
		}

		log.Info("Executing job with in-memory script injection",
			zap.String("job_id", req.JobId),
			zap.String("image", req.Image),
			zap.Int("script_length", len(req.ScriptContent)),
			zap.Strings("requirements", req.Requirements),
		)

		// Build container config with resource limits and script content
		// Script is injected into container via tar archive (zero disk footprint)
		containerConfig := docker.ContainerConfig{
			MemoryLimitMB:  req.MemoryLimitMb,     // Memory limit in MB
			CPULimit:       float64(req.CpuLimit), // CPU limit in cores
			TimeoutSeconds: req.TimeoutSeconds,    // Execution timeout in seconds
			Requirements:   req.Requirements,      // Python dependencies to install
			Script:         req.ScriptContent,     // Script content injected into container
		}

		// Run the container with log streaming callback and security sandbox
		startTime := time.Now()
		output, err := executor.RunContainer(ctx, req.Image, containerConfig, logSender)
		duration := time.Since(startTime)

		if err != nil {
			result.Status = "failed"
			result.OutputLog = fmt.Sprintf("Execution error: %v", err)
			log.Error("Job execution failed",
				zap.String("job_id", req.JobId),
				zap.Error(err),
			)
			return result
		}

		result.Status = "completed"
		result.OutputLog = output

		// Add execution stats
		// Note: Peak CPU implementation would require continuous monitoring during execution
		// For now we just send the duration and 0 for peak CPU or we could sample once
		result.Stats = &pb.JobStats{
			DurationMs: duration.Milliseconds(),
			// PeakCpuPercent: 0, // Placeholder until container monitoring is implemented
		}

		log.Info("Job completed successfully",
			zap.String("job_id", req.JobId),
			zap.Int("output_length", len(output)),
			zap.Int64("duration_ms", duration.Milliseconds()),
		)

		return result
	}
}

// capacityToNodeInfo converts discovered NodeCapacity to proto NodeInfo.
// Only uses fields that exist in the proto definition.
func capacityToNodeInfo(capacity *NodeCapacity, cfg *config.Config, log *zap.Logger) *pb.NodeInfo {
	nodeInfo := &pb.NodeInfo{}

	// Fill host info - only Id is available from host
	if capacity.Host != nil {
		nodeInfo.Id = capacity.Host.MachineID

		// New Hardware Specs
		nodeInfo.CpuModel = capacity.Host.CPUModel
		nodeInfo.CpuCores = int32(capacity.Host.CPUCores)
		nodeInfo.RamTotalMb = int64(capacity.Host.TotalRAM * 1024)
		nodeInfo.DiskTotalGb = capacity.Host.DiskTotalGB
		nodeInfo.OsInfo = capacity.Host.OSInfo
	}

	// Fill GPU info (use first GPU if available)
	if capacity.GPUs != nil && len(capacity.GPUs.GPUs) > 0 {
		firstGPU := capacity.GPUs.GPUs[0]
		nodeInfo.GpuModel = firstGPU.Name
		nodeInfo.VramTotal = uint64(firstGPU.TotalVRAM * 1024 * 1024 * 1024) // GB to bytes
		nodeInfo.VramFree = uint64(firstGPU.FreeVRAM * 1024 * 1024 * 1024)

		// Map all GPUs
		for _, g := range capacity.GPUs.GPUs {
			nodeInfo.GpuInfo = append(nodeInfo.GpuInfo, &pb.GPUInfo{
				Name:          g.Name,
				VramMb:        int64(g.TotalVRAM * 1024),
				DriverVersion: g.DriverVersion,
				CudaVersion:   g.ComputeCapability, // Using Compute Capability as proxy or we could add field
			})
		}
	}

	// Fill Docker info
	if capacity.Docker != nil && capacity.Docker.Available {
		nodeInfo.DockerVersion = capacity.Docker.ServerVersion
	}

	// Fill Owner ID from configuration
	if cfg.OwnerID != "" {
		nodeInfo.OwnerId = cfg.OwnerID
		log.Info("Owner ID configured",
			zap.String("owner_id", cfg.OwnerID),
		)
	} else {
		panic("Owner ID not configured. Set via --owner flag or NEXUS_OWNER_ID env var")
	}

	return nodeInfo
}

// runDiscovery performs all discovery operations concurrently.
// It uses WaitGroups and channels to coordinate parallel operations.
func runDiscovery(ctx context.Context, cfg *config.Config, log *zap.Logger) (*NodeCapacity, error) {
	startTime := time.Now()

	capacity := &NodeCapacity{
		AgentVersion:  cfg.AgentVersion,
		DiscoveryTime: startTime,
		Errors:        make([]string, 0),
	}

	// Use WaitGroup to coordinate concurrent discovery
	var wg sync.WaitGroup
	var mu sync.Mutex // Protects capacity.Errors

	// Error helper function
	addError := func(err string) {
		mu.Lock()
		capacity.Errors = append(capacity.Errors, err)
		mu.Unlock()
	}

	// --- Host Discovery ---
	wg.Add(1)
	go func() {
		defer wg.Done()

		log.Debug("Starting host telemetry collection")
		collector := host.NewGopsutilCollector()

		hostInfo, err := collector.Collect(ctx)
		if err != nil {
			log.Error("Host collection failed",
				zap.Error(err),
			)
			addError(fmt.Sprintf("host: %v", err))
			return
		}

		capacity.Host = hostInfo
		log.Debug("Host telemetry collection complete",
			zap.String("hostname", hostInfo.Hostname),
			zap.String("os", hostInfo.OS),
			zap.Float64("ram_gb", hostInfo.TotalRAM),
		)
	}()

	// --- GPU Discovery ---
	wg.Add(1)
	go func() {
		defer wg.Done()

		log.Debug("Starting GPU discovery")

		// Create context with timeout for GPU operations
		gpuCtx, gpuCancel := context.WithTimeout(ctx, cfg.GPUTimeout)
		defer gpuCancel()

		discoverer := gpu.NewNVMLDiscoverer()
		defer func() {
			if err := discoverer.Close(); err != nil {
				log.Warn("Failed to close GPU discoverer",
					zap.Error(err),
				)
			}
		}()

		gpuResult, err := discoverer.Discover(gpuCtx)
		if err != nil {
			log.Error("GPU discovery failed",
				zap.Error(err),
			)
			addError(fmt.Sprintf("gpu: %v", err))
			return
		}

		capacity.GPUs = gpuResult

		if gpuResult.CPUOnlyMode {
			log.Info("GPU discovery complete - CPU only mode",
				zap.String("reason", gpuResult.Error),
			)
		} else {
			log.Debug("GPU discovery complete",
				zap.Int("gpu_count", len(gpuResult.GPUs)),
				zap.String("driver_version", gpuResult.DriverVersion),
			)
		}
	}()

	// --- Docker Check ---
	wg.Add(1)
	go func() {
		defer wg.Done()

		log.Debug("Starting Docker connectivity check")

		// Create context with timeout for Docker operations
		dockerCtx, dockerCancel := context.WithTimeout(ctx, cfg.DockerTimeout)
		defer dockerCancel()

		checker, err := docker.NewDockerChecker()
		if err != nil {
			log.Error("Failed to create Docker checker",
				zap.Error(err),
			)
			addError(fmt.Sprintf("docker_init: %v", err))

			// Create a placeholder result
			capacity.Docker = &docker.DockerInfo{
				Available: false,
				Error:     err.Error(),
			}

			// Fail fast if Docker is required
			if cfg.DockerRequired {
				printDockerInstructions()
				log.Fatal("Docker is required but unavailable",
					zap.Error(err),
				)
			}
			return
		}
		defer func() {
			if err := checker.Close(); err != nil {
				log.Warn("Failed to close Docker checker",
					zap.Error(err),
				)
			}
		}()

		dockerInfo, err := checker.Check(dockerCtx)
		if err != nil {
			log.Error("Docker check failed",
				zap.Error(err),
			)
			addError(fmt.Sprintf("docker: %v", err))
			return
		}

		capacity.Docker = dockerInfo

		if dockerInfo.Available {
			log.Debug("Docker connectivity check complete",
				zap.String("api_version", dockerInfo.APIVersion),
				zap.String("server_version", dockerInfo.ServerVersion),
				zap.Int("running_containers", dockerInfo.ContainersRunning),
			)
		} else {
			log.Warn("Docker is not available",
				zap.String("error", dockerInfo.Error),
			)

			// Fail fast if Docker is required
			if cfg.DockerRequired {
				printDockerInstructions()
				log.Fatal("Docker is required but not available",
					zap.String("error", dockerInfo.Error),
				)
			}
		}
	}()

	// Wait for all discovery operations to complete
	// Also handle context cancellation
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All discoveries completed
		log.Debug("All discovery operations complete")
	case <-ctx.Done():
		return nil, fmt.Errorf("discovery cancelled: %w", ctx.Err())
	}

	// Calculate discovery duration
	capacity.DiscoveryDuration = time.Since(startTime)

	return capacity, nil
}

// printDockerInstructions prints user-friendly instructions for installing Docker
func printDockerInstructions() {
	fmt.Println("\n" +
		"╔══════════════════════════════════════════════════════════════╗\n" +
		"║                  DOCKER IS REQUIRED                          ║\n" +
		"╠══════════════════════════════════════════════════════════════╣\n" +
		"║  The Agent requires Docker to execute jobs.                  ║\n" +
		"║  Please install and start Docker Desktop to continue.        ║\n" +
		"║                                                              ║")

	if runtime.GOOS == "windows" {
		fmt.Println("" +
			"║  Windows Installation:                                       ║\n" +
			"║  1. Download: docs.docker.com/desktop/install/windows-install/ ║\n" +
			"║  2. Run the installer                                        ║\n" +
			"║  3. Start Docker Desktop                                     ║")
	} else if runtime.GOOS == "darwin" {
		fmt.Println("" +
			"║  Mac Installation:                                           ║\n" +
			"║  1. Download: docs.docker.com/desktop/install/mac-install/     ║\n" +
			"║  2. Drag to Applications                                     ║\n" +
			"║  3. Start Docker Desktop                                     ║")
	} else {
		fmt.Println("" +
			"║  Linux Installation:                                         ║\n" +
			"║  1. Follow guide: docs.docker.com/engine/install/            ║\n" +
			"║  2. Ensure service is running: sudo systemctl start docker   ║\n" +
			"║  3. Add user to group: sudo usermod -aG docker $USER         ║")
	}
	fmt.Println("" +
		"╚══════════════════════════════════════════════════════════════╝\n")
}
