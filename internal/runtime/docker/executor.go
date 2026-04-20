// Package docker provides Docker daemon connectivity and container execution.
// It supports checking Docker availability and running containers for job execution.
package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"go.uber.org/zap"
)

const (
	// isolatedNetworkName is the name of the Docker network used for all job containers.
	// This network allows internet egress but blocks access to private/internal subnets
	// (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16) and the Docker host.
	isolatedNetworkName = "vortix-isolated"

	// isolatedNetworkSubnet is the subnet for the isolated network.
	// Chosen from a narrow range to avoid conflicts with common LAN subnets.
	isolatedNetworkSubnet = "172.30.0.0/24"

	// isolatedNetworkGateway is the gateway for the isolated network.
	isolatedNetworkGateway = "172.30.0.1"
)

// Executor handles Docker container execution for job processing.
type Executor struct {
	client    *client.Client
	logger    *zap.Logger
	networkID string // ID of the vortix-isolated network
	gpuCount  int    // Number of GPUs available on this node (0 = CPU-only)
}

// NewExecutor creates a new Docker container executor.
// It creates a client from environment variables (DOCKER_HOST, etc.)
// and sets up an isolated Docker network that allows internet egress
// but blocks all access to private/internal subnets and the host.
//
// gpuCount indicates how many GPUs are available on this node.
// If > 0, containers will be created with NVIDIA GPU passthrough via DeviceRequests.
// If 0, containers run in CPU-only mode.
func NewExecutor(logger *zap.Logger, gpuCount int) (*Executor, error) {
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	// Validate NVIDIA Container Toolkit if GPUs were discovered.
	// If toolkit is not installed, fall back to CPU-only mode rather than failing
	// at container creation time with an unclear error.
	if gpuCount > 0 {
		if hasToolkit := checkNVIDIAToolkit(cli, logger); !hasToolkit {
			logger.Warn("NVIDIA Container Toolkit not detected - falling back to CPU-only mode",
				zap.Int("gpus_discovered", gpuCount),
			)
			fmt.Println("\n" +
				"  [!] NVIDIA Container Toolkit is not installed or not configured.\n" +
				"      GPUs were detected but cannot be passed to containers.\n" +
				"      Node will operate in CPU-only mode.\n" +
				"\n" +
				"      To enable GPU support, install the toolkit:\n" +
				"        https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html\n" +
				"\n" +
				"      Then configure Docker:\n" +
				"        sudo nvidia-ctk runtime configure --runtime=docker\n" +
				"        sudo systemctl restart docker\n")
			gpuCount = 0
		}
	}

	e := &Executor{
		client:   cli,
		logger:   logger,
		gpuCount: gpuCount,
	}

	if err := e.ensureIsolatedNetwork(context.Background()); err != nil {
		cli.Close()
		return nil, fmt.Errorf("failed to setup isolated network: %w", err)
	}

	logger.Info("Docker executor initialized",
		zap.Int("gpu_count", gpuCount),
	)

	return e, nil
}

// checkNVIDIAToolkit checks if the NVIDIA Container Toolkit is registered with Docker.
// It queries the Docker daemon info and looks for the "nvidia" runtime in the registered runtimes.
// This is a lightweight check (no container spin-up) that catches the common case of
// GPUs being present but the toolkit not installed/configured.
func checkNVIDIAToolkit(cli *client.Client, logger *zap.Logger) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	info, err := cli.Info(ctx)
	if err != nil {
		logger.Warn("Failed to query Docker info for NVIDIA toolkit check", zap.Error(err))
		return false
	}

	for name := range info.Runtimes {
		if name == "nvidia" {
			logger.Info("NVIDIA Container Toolkit detected",
				zap.String("runtime", "nvidia"),
			)
			return true
		}
	}

	return false
}

// ensureIsolatedNetwork creates (or reuses) the vortix-isolated Docker network.
// The network allows outbound internet access but blocks private subnets via
// iptables rules applied after network creation.
func (e *Executor) ensureIsolatedNetwork(ctx context.Context) error {
	// Check if network already exists (from a previous agent run)
	networks, err := e.client.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list networks: %w", err)
	}

	for _, n := range networks {
		if n.Name == isolatedNetworkName {
			e.networkID = n.ID
			e.logger.Info("Reusing existing isolated network",
				zap.String("network_id", n.ID),
			)
			return e.applyNetworkFirewallRules()
		}
	}

	// Create new network with ICC disabled (no inter-container communication)
	resp, err := e.client.NetworkCreate(ctx, isolatedNetworkName, network.CreateOptions{
		Driver: "bridge",
		IPAM: &network.IPAM{
			Config: []network.IPAMConfig{
				{
					Subnet:  isolatedNetworkSubnet,
					Gateway: isolatedNetworkGateway,
				},
			},
		},
		Options: map[string]string{
			"com.docker.network.bridge.enable_icc": "false", // Block container-to-container
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create isolated network: %w", err)
	}

	e.networkID = resp.ID
	e.logger.Info("Created isolated network",
		zap.String("network_id", resp.ID),
		zap.String("subnet", isolatedNetworkSubnet),
	)

	return e.applyNetworkFirewallRules()
}

// applyNetworkFirewallRules adds iptables rules to block traffic from the isolated network
// to all private/internal subnets. This ensures containers can reach the public internet
// but cannot access the host machine, LAN, or other internal services.
//
// Blocked ranges:
//   - 10.0.0.0/8      (Class A private)
//   - 172.16.0.0/12   (Class B private, includes Docker default bridge 172.17.0.0/16)
//   - 192.168.0.0/16  (Class C private, most home/office LANs)
//   - 169.254.0.0/16  (Link-local / APIPA)
//   - 127.0.0.0/8     (Loopback)
//
// Exception: traffic within the isolated network's own subnet (172.30.0.0/24) is allowed
// so containers can reach the gateway for DNS resolution and internet routing.
func (e *Executor) applyNetworkFirewallRules() error {
	blockedSubnets := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"127.0.0.0/8",
	}

	// First, allow traffic to the isolated network's own gateway.
	// This is needed because 172.30.0.0/24 falls within the 172.16.0.0/12 block.
	// Without this exception, containers couldn't reach the gateway for DNS and routing.
	if err := e.addIptablesAcceptRule(isolatedNetworkGateway); err != nil {
		return fmt.Errorf("failed to allow gateway access: %w", err)
	}

	for _, subnet := range blockedSubnets {
		if err := e.addIptablesRule(subnet); err != nil {
			return fmt.Errorf("failed to block subnet %s: %w", subnet, err)
		}
	}

	e.logger.Info("Network firewall rules applied",
		zap.Strings("blocked_subnets", blockedSubnets),
	)

	return nil
}

// addIptablesRule inserts an iptables DROP rule to block traffic from the isolated network
// to the specified destination subnet. Uses -C (check) first to avoid duplicate rules.
func (e *Executor) addIptablesRule(destSubnet string) error {
	return e.upsertIptablesRule(destSubnet, "DROP")
}

// addIptablesAcceptRule inserts an iptables ACCEPT rule to allow traffic from the isolated
// network to a specific destination (e.g., the gateway). Inserted before DROP rules.
func (e *Executor) addIptablesAcceptRule(destIP string) error {
	return e.upsertIptablesRule(destIP, "ACCEPT")
}

// upsertIptablesRule inserts an iptables rule if it doesn't already exist.
func (e *Executor) upsertIptablesRule(dest string, action string) error {
	chain := "DOCKER-USER"
	ruleArgs := []string{
		"-i", "br-" + e.networkID[:12],
		"-d", dest,
		"-j", action,
	}

	// Check if rule already exists
	checkArgs := append([]string{"iptables", "-C", chain}, ruleArgs...)
	exitCode, err := e.runHostCommand(checkArgs)
	if err == nil && exitCode == 0 {
		return nil // Rule already exists
	}

	// Insert rule at the top of the chain
	insertArgs := append([]string{"iptables", "-I", chain, "1"}, ruleArgs...)
	exitCode, err = e.runHostCommand(insertArgs)
	if err != nil {
		return fmt.Errorf("failed to run iptables: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("iptables exited with code %d", exitCode)
	}

	return nil
}

// runHostCommand executes a command on the host using a privileged helper container.
// This is needed because the agent process itself may not have root/iptables access,
// but it can create a privileged container with host network namespace to run iptables.
func (e *Executor) runHostCommand(cmd []string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := e.client.ContainerCreate(ctx, &container.Config{
		Image: "alpine:latest",
		Cmd:   cmd,
	}, &container.HostConfig{
		NetworkMode: "host",
		CapAdd:      []string{"NET_ADMIN"},
		AutoRemove:  true,
	}, nil, nil, "")
	if err != nil {
		return -1, fmt.Errorf("failed to create iptables helper: %w", err)
	}

	if err := e.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return -1, fmt.Errorf("failed to start iptables helper: %w", err)
	}

	statusCh, errCh := e.client.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return -1, err
		}
	case status := <-statusCh:
		return int(status.StatusCode), nil
	case <-ctx.Done():
		return -1, ctx.Err()
	}

	return -1, nil
}

// RuntimeType defines how the container should be executed.
const (
	RuntimeScript            = "SCRIPT"             // Inject script, run with interpreter (default)
	RuntimeDockerImage       = "DOCKER_IMAGE"       // Custom image + command, no script injection assumptions
	RuntimePersistentService = "PERSISTENT_SERVICE"  // Long-running container with health checks and port exposure
)

// PortMapping maps a container port to a host port.
type PortMapping struct {
	ContainerPort int    // Port inside the container
	HostPort      int    // Port on the host (0 = auto-assign)
	Protocol      string // "tcp" or "udp" (default: "tcp")
}

// HealthCheckConfig defines how to verify a persistent service is healthy.
type HealthCheckConfig struct {
	Endpoint           string // HTTP path (e.g., "/health")
	Port               int    // Port to check
	IntervalSeconds    int    // Check interval (default: 30)
	TimeoutSeconds     int    // Per-check timeout (default: 5)
	Retries            int    // Consecutive failures before unhealthy (default: 3)
	InitialDelaySeconds int   // Wait before first check (default: 10)
}

// ContainerConfig holds resource limits, timeout settings, and dependencies for container execution.
type ContainerConfig struct {
	MemoryLimitMB    int64             // Memory limit in megabytes (e.g., 512 for 512MB)
	CPULimit         float64           // CPU limit in cores (e.g., 0.5 for half core, 1.0 for one core)
	VolumeLimitGB    int64             // Volume/disk limit in GB (0 = no limit)
	TimeoutSeconds   int32             // Execution timeout in seconds (0 = no timeout for services)
	Requirements     []string          // Package dependencies to install before execution (e.g., ["numpy", "express"])
	Script           string            // Main script content (injected as Entrypoint filename)
	Files            map[string][]byte // Additional files to inject into /app (path -> content)
	Entrypoint       string            // Script to execute (default: "task.py")
	Command          []string          // Custom command override (e.g., ["node", "index.js"]). If set, Script/Entrypoint/Requirements are ignored.
	MaxArtifactBytes int64             // Max total artifact output size in bytes (0 = no artifact collection)
	RuntimeType      string            // Execution mode: SCRIPT, DOCKER_IMAGE, PERSISTENT_SERVICE
	Environment      map[string]string // User-defined environment variables
	PortMappings     []PortMapping     // Ports to expose (for PERSISTENT_SERVICE)
	HealthCheck      *HealthCheckConfig // Health check config (for PERSISTENT_SERVICE)
	GpuCount         int               // GPUs requested (0 = CPU only, -1 = all)
	ShmSizeMB        int64             // Override shared memory size (0 = auto-calculate)
	InstallCommand   string            // Custom dependency install command (e.g., "npm install")
}

// Artifact represents a file extracted from the container's /app/output/ directory.
type Artifact struct {
	Filename  string // Relative path (e.g., "model.pt", "results/metrics.csv")
	SizeBytes int64
	Content   []byte
}

// RunResult holds the output of a container execution.
type RunResult struct {
	Logs      string
	Artifacts []Artifact
}

// RunContainer executes a container with the specified image and script.
// After execution, it extracts artifact files from /app/output/ if MaxArtifactBytes > 0.
//
// Security controls enforced:
//   - TIMEOUT: Uses context.WithTimeout to enforce execution time limits
//   - MEMORY: Limits container memory via HostConfig.Resources.Memory
//   - CPU: Limits CPU via HostConfig.Resources.NanoCPUs
//   - NETWORK: Uses vortix-isolated network (internet egress allowed, private subnets blocked)
func (e *Executor) RunContainer(ctx context.Context, imageName string, config ContainerConfig, logCallback func(string)) (*RunResult, error) {
	e.logger.Debug("Starting container execution",
		zap.String("image", imageName),
		zap.Int64("memory_limit_mb", config.MemoryLimitMB),
		zap.Float64("cpu_limit", config.CPULimit),
		zap.Int64("volume_limit_gb", config.VolumeLimitGB),
		zap.Int32("timeout_seconds", config.TimeoutSeconds),
		zap.Strings("requirements", config.Requirements),
		zap.Int("script_size", len(config.Script)),
	)

	// Step 1: Create timeout context to enforce execution time limit
	// All container operations (create, start, wait) will respect this timeout
	var timeoutCtx context.Context
	var cancel context.CancelFunc
	if config.TimeoutSeconds > 0 {
		timeoutCtx, cancel = context.WithTimeout(ctx, time.Duration(config.TimeoutSeconds)*time.Second)
	} else {
		// Default to 5 minute timeout if not specified
		timeoutCtx, cancel = context.WithTimeout(ctx, 5*time.Minute)
	}
	defer cancel()

	// Step 2: Check if image exists locally, pull if not
	if err := e.ensureImage(timeoutCtx, imageName); err != nil {
		return nil, fmt.Errorf("failed to ensure image: %w", err)
	}

	// Step 3: Create container (do not start yet)
	containerID, err := e.createContainer(timeoutCtx, imageName, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create container: %w", err)
	}

	// Ensure container is removed after we're done (Force=true for cleanup even on timeout)
	defer func() {
		removeCtx := context.Background()
		if removeErr := e.client.ContainerRemove(removeCtx, containerID, container.RemoveOptions{
			Force: true,
		}); removeErr != nil {
			e.logger.Warn("Failed to remove container",
				zap.String("container_id", containerID),
				zap.Error(removeErr),
			)
		} else {
			e.logger.Debug("Container removed successfully",
				zap.String("container_id", containerID),
			)
		}
	}()

	// Step 4: Build file map and inject into container
	files := make(map[string][]byte)

	if config.RuntimeType != RuntimeDockerImage && config.RuntimeType != RuntimePersistentService {
		// SCRIPT mode: inject script file
		entrypoint := config.Entrypoint
		if entrypoint == "" {
			entrypoint = "task.py"
		}
		if config.Script != "" {
			files[entrypoint] = []byte(config.Script)
		}
	}

	// Inject additional files for all runtime types
	for path, content := range config.Files {
		files[path] = content
	}

	// Create output directory so users can write artifacts even if they don't mkdir
	files["output/.keep"] = []byte{}

	if len(files) > 0 {
		if err := e.injectFiles(timeoutCtx, containerID, files); err != nil {
			return nil, fmt.Errorf("failed to inject files: %w", err)
		}
	}

	// Step 5: Start the container
	if err := e.client.ContainerStart(timeoutCtx, containerID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	e.logger.Debug("Container started", zap.String("container_id", containerID))

	// Step 5.5: Volume monitoring goroutine
	var volumeExceeded bool
	var volumeExceededBytes int64
	volumeLimitBytes := config.VolumeLimitGB * 1024 * 1024 * 1024

	volumeCtx, volumeCancel := context.WithCancel(timeoutCtx)
	defer volumeCancel()

	if config.VolumeLimitGB > 0 {
		go func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-volumeCtx.Done():
					return
				case <-ticker.C:
					usedBytes, err := e.getContainerDiskUsage(volumeCtx, containerID)
					if err != nil {
						continue
					}
					if usedBytes > volumeLimitBytes {
						volumeExceeded = true
						volumeExceededBytes = usedBytes
						e.logger.Warn("Volume limit exceeded, killing container",
							zap.String("container_id", containerID),
							zap.Int64("used_bytes", usedBytes),
							zap.Int64("limit_bytes", volumeLimitBytes),
						)
						_ = e.client.ContainerKill(context.Background(), containerID, "SIGKILL")
						return
					}
				}
			}
		}()
	}

	// Helper for error returns with logs
	errResult := func(logs, msg string, err error) (*RunResult, error) {
		if logCallback != nil && msg != "" {
			logCallback(msg)
		}
		if msg != "" && logs != "" {
			logs = logs + "\n" + msg
		}
		return &RunResult{Logs: logs}, err
	}

	// Step 6: Stream logs in real-time
	logs, err := e.streamLogs(timeoutCtx, containerID, logCallback)
	if err != nil {
		if timeoutCtx.Err() == context.DeadlineExceeded {
			return errResult(logs, "Process killed due to timeout.", fmt.Errorf("execution timeout: %w", timeoutCtx.Err()))
		}
		return nil, fmt.Errorf("failed to stream logs: %w", err)
	}

	// Step 7: Wait for container to finish
	statusCh, errCh := e.client.ContainerWait(timeoutCtx, containerID, container.WaitConditionNotRunning)

	select {
	case err := <-errCh:
		if err != nil {
			if volumeExceeded {
				usedGB := float64(volumeExceededBytes) / (1024 * 1024 * 1024)
				return errResult(logs, fmt.Sprintf("Volume limit exceeded (%.2f GB / %d GB)", usedGB, config.VolumeLimitGB), fmt.Errorf("volume limit exceeded"))
			}
			if timeoutCtx.Err() == context.DeadlineExceeded {
				return errResult("", "Process killed due to timeout.", fmt.Errorf("execution timeout: %w", timeoutCtx.Err()))
			}
			return nil, fmt.Errorf("error waiting for container: %w", err)
		}
	case status := <-statusCh:
		if volumeExceeded {
			usedGB := float64(volumeExceededBytes) / (1024 * 1024 * 1024)
			return errResult(logs, fmt.Sprintf("Volume limit exceeded (%.2f GB / %d GB)", usedGB, config.VolumeLimitGB), fmt.Errorf("volume limit exceeded"))
		}
		if status.StatusCode == 137 {
			return errResult("", "Memory Limit Exceeded.", fmt.Errorf("memory limit exceeded"))
		}
		e.logger.Debug("Container finished",
			zap.String("container_id", containerID),
			zap.Int64("exit_code", status.StatusCode),
		)
	case <-timeoutCtx.Done():
		if volumeExceeded {
			usedGB := float64(volumeExceededBytes) / (1024 * 1024 * 1024)
			return errResult(logs, fmt.Sprintf("Volume limit exceeded (%.2f GB / %d GB)", usedGB, config.VolumeLimitGB), fmt.Errorf("volume limit exceeded"))
		}
		return errResult(logs, "Process killed due to timeout.", fmt.Errorf("execution timeout: %w", timeoutCtx.Err()))
	}

	// Step 8: Extract artifacts from /app/output/ if requested
	result := &RunResult{Logs: logs}

	if config.MaxArtifactBytes > 0 {
		artifacts, err := e.extractArtifacts(containerID, config.MaxArtifactBytes)
		if err != nil {
			e.logger.Warn("Failed to extract artifacts", zap.Error(err))
			// Non-fatal: job succeeded, artifact extraction failed
		} else {
			result.Artifacts = artifacts
		}
	}

	return result, nil
}

// extractArtifacts copies /app/output/ from the container and parses the tar archive.
// It enforces a total size limit and skips symlinks and paths with ".." to prevent escape.
func (e *Executor) extractArtifacts(containerID string, maxBytes int64) ([]Artifact, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reader, _, err := e.client.CopyFromContainer(ctx, containerID, "/app/output/")
	if err != nil {
		return nil, fmt.Errorf("failed to copy from container: %w", err)
	}
	defer reader.Close()

	tr := tar.NewReader(reader)
	var artifacts []Artifact
	var totalBytes int64
	const maxFiles = 1000

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return artifacts, fmt.Errorf("failed to read tar entry: %w", err)
		}

		// Skip directories, symlinks, and special files
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		// Security: reject paths that try to escape
		if strings.Contains(hdr.Name, "..") {
			e.logger.Warn("Skipping artifact with suspicious path", zap.String("path", hdr.Name))
			continue
		}

		// Strip the leading "output/" prefix from tar path
		name := strings.TrimPrefix(hdr.Name, "output/")
		if name == "" || name == ".keep" {
			continue
		}

		// Check limits
		if totalBytes+hdr.Size > maxBytes {
			e.logger.Warn("Artifact size limit reached, skipping remaining files",
				zap.Int64("total_bytes", totalBytes),
				zap.Int64("max_bytes", maxBytes),
			)
			break
		}
		if len(artifacts) >= maxFiles {
			e.logger.Warn("Artifact file count limit reached", zap.Int("max_files", maxFiles))
			break
		}

		content, err := io.ReadAll(io.LimitReader(tr, hdr.Size))
		if err != nil {
			return artifacts, fmt.Errorf("failed to read artifact %s: %w", name, err)
		}

		totalBytes += int64(len(content))
		artifacts = append(artifacts, Artifact{
			Filename:  name,
			SizeBytes: int64(len(content)),
			Content:   content,
		})
	}

	e.logger.Info("Artifacts extracted",
		zap.Int("count", len(artifacts)),
		zap.Int64("total_bytes", totalBytes),
	)

	return artifacts, nil
}

// getContainerDiskUsage returns the disk usage of a container's writable layer in bytes.
func (e *Executor) getContainerDiskUsage(ctx context.Context, containerID string) (int64, error) {
	// ContainerInspect with Size=true returns SizeRw (writable layer size)
	inspect, err := e.client.ContainerInspect(ctx, containerID)
	if err != nil {
		return 0, err
	}

	// SizeRw is the size of files that have been created or changed by this container
	if inspect.SizeRw != nil {
		return *inspect.SizeRw, nil
	}

	return 0, nil
}

// injectFiles creates a tar archive containing all workspace files in RAM and copies it
// into the container at /app. This achieves zero disk footprint on the host.
//
// The files map keys are relative paths (e.g., "task.py", "model/network.py").
// Directories are created automatically by the tar extraction.
func (e *Executor) injectFiles(ctx context.Context, containerID string, files map[string][]byte) error {
	if len(files) == 0 {
		return nil
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	for path, content := range files {
		hdr := &tar.Header{
			Name: path,
			Mode: 0644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("failed to write tar header for %s: %w", path, err)
		}
		if _, err := tw.Write(content); err != nil {
			return fmt.Errorf("failed to write %s to tar: %w", path, err)
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("failed to close tar writer: %w", err)
	}

	if err := e.client.CopyToContainer(ctx, containerID, "/app", &buf, container.CopyToContainerOptions{}); err != nil {
		return fmt.Errorf("failed to copy files to container: %w", err)
	}

	e.logger.Debug("Injected files into container",
		zap.Int("file_count", len(files)),
		zap.Int("tar_bytes", buf.Len()),
	)

	return nil
}

// ensureImage checks if the image exists locally and pulls it if not.
func (e *Executor) ensureImage(ctx context.Context, imageName string) error {
	// Check if image exists locally
	images, err := e.client.ImageList(ctx, image.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list images: %w", err)
	}

	// Check if our image is in the list
	imageExists := false
	for _, img := range images {
		for _, tag := range img.RepoTags {
			if tag == imageName || tag == imageName+":latest" {
				imageExists = true
				break
			}
		}
		if imageExists {
			break
		}
	}

	if imageExists {
		e.logger.Debug("Image already exists locally",
			zap.String("image", imageName),
		)
		return nil
	}

	// Pull the image
	e.logger.Info("Pulling image",
		zap.String("image", imageName),
	)

	reader, err := e.client.ImagePull(ctx, imageName, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}
	defer reader.Close()

	// Drain the reader to complete the pull
	// The pull won't complete until we read the response
	_, err = io.Copy(io.Discard, reader)
	if err != nil {
		return fmt.Errorf("failed to complete image pull: %w", err)
	}

	e.logger.Info("Image pulled successfully",
		zap.String("image", imageName),
	)

	return nil
}

// createContainer creates a new container with the specified image and resource limits.
// The container is created but NOT started - this allows us to inject the script first.
// Resource limits, network isolation, and security hardening are applied via HostConfig.
//
// Security Hardening (Principle of Least Privilege):
//   - CapDrop: ALL - Drops all Linux capabilities to minimize attack surface
//   - SecurityOpt: no-new-privileges - Prevents privilege escalation via setuid/setgid binaries
//   - Privileged: false - Explicitly disables privileged mode to prevent host access
//   - User: 1000:1000 - Runs as non-root user to limit damage from container escape
//   - Working directory set to /app where the script is injected
func (e *Executor) createContainer(ctx context.Context, imageName string, config ContainerConfig) (string, error) {
	// Build the container command based on runtime type.
	var finalCmd []string

	switch config.RuntimeType {
	case RuntimeDockerImage:
		// DOCKER_IMAGE: User provides command, or use image's default CMD
		if len(config.Command) > 0 {
			finalCmd = config.Command
		}
		// If no command, finalCmd stays nil -> Docker uses image's default CMD/ENTRYPOINT
		e.logger.Debug("Runtime: DOCKER_IMAGE", zap.Strings("command", finalCmd))

	case RuntimePersistentService:
		// PERSISTENT_SERVICE: Same as DOCKER_IMAGE but with health checks (handled in RunContainer)
		if len(config.Command) > 0 {
			finalCmd = config.Command
		}
		e.logger.Debug("Runtime: PERSISTENT_SERVICE", zap.Strings("command", finalCmd))

	default:
		// SCRIPT mode (default, backward compatible):
		//   1. Custom command override -> use it directly
		//   2. Install command + entrypoint -> install deps then run
		//   3. Requirements (legacy pip) + entrypoint -> pip install then run
		//   4. Just entrypoint -> run with python
		if len(config.Command) > 0 {
			finalCmd = config.Command
			e.logger.Debug("Script mode with custom command", zap.Strings("command", finalCmd))
		} else {
			entrypoint := config.Entrypoint
			if entrypoint == "" {
				entrypoint = "task.py"
			}

			if config.InstallCommand != "" {
				// Custom install command (e.g., "npm install", "pip install -r requirements.txt")
				combinedCmd := config.InstallCommand + " && exec " + strings.Join(guessInterpreter(entrypoint), " ") + " " + entrypoint
				finalCmd = []string{"/bin/sh", "-c", combinedCmd}
				e.logger.Debug("Script mode with custom install", zap.String("install", config.InstallCommand))
			} else if len(config.Requirements) > 0 {
				// Legacy pip requirements (backward compatible)
				e.logger.Info("Installing dependencies before script execution",
					zap.Strings("requirements", config.Requirements),
				)
				pipInstallCmd := "pip install --user --no-cache-dir " + strings.Join(config.Requirements, " ")
				interpreter := strings.Join(guessInterpreter(entrypoint), " ")
				combinedCmd := pipInstallCmd + " && exec " + interpreter + " " + entrypoint
				finalCmd = []string{"/bin/sh", "-c", combinedCmd}
			} else {
				interpreter := guessInterpreter(entrypoint)
				finalCmd = append(interpreter, entrypoint)
			}
		}
	}

	// Build environment variables
	envVars := []string{
		"HOME=/tmp",
		"PATH=/tmp/.local/bin:/usr/local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"VORTIX_OUTPUT_DIR=/app/output",  // Convention: write output files here for artifact collection
		"VORTIX_RUNTIME=" + config.RuntimeType,
	}

	// Add user-defined environment variables
	for key, val := range config.Environment {
		// Block overriding critical system vars
		switch strings.ToUpper(key) {
		case "PATH", "HOME":
			e.logger.Warn("Blocked override of system env var", zap.String("key", key))
			continue
		}
		envVars = append(envVars, key+"="+val)
	}

	// Determine GPU count for this container
	gpuCount := e.gpuCount
	if config.GpuCount > 0 && config.GpuCount < gpuCount {
		gpuCount = config.GpuCount
	} else if config.GpuCount == 0 {
		gpuCount = 0 // Explicitly CPU-only
	}
	// config.GpuCount == -1 means all available (keep e.gpuCount)

	// Add NVIDIA env vars when GPUs are available so the container runtime
	// knows which driver capabilities to expose and which devices to mount.
	if gpuCount > 0 {
		envVars = append(envVars,
			"NVIDIA_VISIBLE_DEVICES=all",
			"NVIDIA_DRIVER_CAPABILITIES=compute,utility",
		)
	}

	containerConfig := &container.Config{
		Image: imageName,
		Cmd:   finalCmd,
		Tty:   false,
		Env:   envVars,
		// WorkingDir: Set to /app where the script is injected.
		// Scripts will execute in this directory.
		WorkingDir: "/app",

		// User: Run as non-root user (UID:GID = 1000:1000).
		// This limits the impact of container escape vulnerabilities.
		// Note: The numeric ID is used instead of username to ensure compatibility
		// with any Docker image regardless of whether the user exists in /etc/passwd.
		User: "1000:1000",
	}

	// Calculate resource limits
	memoryBytes := config.MemoryLimitMB * 1024 * 1024
	nanoCPUs := int64(config.CPULimit * 1_000_000_000)

	// Calculate SHM size: user override, or 25% of memory limit (minimum 256MB).
	// PyTorch DataLoader with num_workers>0 and NCCL multi-GPU communication
	// require significantly more than Docker's default 64MB.
	var shmBytes int64
	if config.ShmSizeMB > 0 {
		shmBytes = config.ShmSizeMB * 1024 * 1024
	} else {
		const minShmBytes int64 = 256 * 1024 * 1024 // 256 MB
		shmBytes = memoryBytes / 4
		if shmBytes < minShmBytes {
			shmBytes = minShmBytes
		}
	}

	resources := container.Resources{
		Memory:     memoryBytes,
		MemorySwap: memoryBytes,
		NanoCPUs:   nanoCPUs,
	}

	// Add GPU passthrough when GPUs are requested.
	// DeviceRequests tells the NVIDIA Container Toolkit to mount GPU devices
	// and driver libraries into the container.
	if gpuCount > 0 {
		deviceCount := -1 // All available GPUs
		if config.GpuCount > 0 {
			deviceCount = config.GpuCount // Specific count requested
		}
		resources.DeviceRequests = []container.DeviceRequest{
			{
				Driver:       "nvidia",
				Count:        deviceCount,
				Capabilities: [][]string{{"gpu"}},
			},
		}
	}

	hostConfig := &container.HostConfig{
		AutoRemove: false,
		Resources:  resources,
		ShmSize:    shmBytes,
		// NetworkMode: All containers use the vortix-isolated network.
		// Internet egress is allowed; private/internal subnets are blocked by iptables.
		NetworkMode: container.NetworkMode(isolatedNetworkName),

		// ============================================================
		// SECURITY HARDENING: Principle of Least Privilege
		// ============================================================

		// CapDrop: Drop ALL Linux capabilities.
		// Capabilities are privilege units that can be independently enabled/disabled.
		// By dropping all, we ensure the container has no special privileges like:
		// - CAP_NET_ADMIN (network configuration)
		// - CAP_SYS_ADMIN (system administration)
		// - CAP_DAC_OVERRIDE (bypass file permission checks)
		// This is the foundation of the principle of least privilege for containers.
		CapDrop: []string{"ALL"},

		// SecurityOpt: Prevent privilege escalation via setuid/setgid binaries.
		// The "no-new-privileges" flag prevents the container from gaining additional
		// privileges through setuid/setgid executables, even if such binaries exist.
		// This blocks attacks where a malicious script might try to execute a
		// setuid binary to escalate from the non-root user to root.
		SecurityOpt: []string{"no-new-privileges"},

		// Privileged: Explicitly disable privileged mode.
		// Privileged containers have full access to host devices and can
		// escape containment. This flag ensures privileged mode is disabled
		// even if it might be enabled by default in some configurations.
		Privileged: false,

		// Note: ReadonlyRootfs is NOT enabled here because we use CopyToContainer
		// to inject the script, which requires a writable filesystem.
		// The script is injected before the container starts, so this is safe.
	}

	e.logger.Debug("Creating container with resource limits and security hardening",
		zap.String("image", imageName),
		zap.Int64("memory_bytes", memoryBytes),
		zap.Int64("shm_bytes", shmBytes),
		zap.Int64("nano_cpus", nanoCPUs),
		zap.Int("gpu_count", e.gpuCount),
		zap.String("network", isolatedNetworkName),
		zap.String("user", containerConfig.User),
	)

	resp, err := e.client.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, "")
	if err != nil {
		return "", err
	}

	return resp.ID, nil
}

// streamLogs attaches to container logs and streams output line-by-line.
// It uses stdcopy to properly demultiplex stdout/stderr streams.
// The logCallback is invoked for each line; if nil, lines are only collected.
func (e *Executor) streamLogs(ctx context.Context, containerID string, logCallback func(string)) (string, error) {
	options := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true, // Stream logs as they come
		Timestamps: false,
	}

	reader, err := e.client.ContainerLogs(ctx, containerID, options)
	if err != nil {
		return "", err
	}
	defer reader.Close()

	// Buffer to collect all logs for final return
	var allLogs bytes.Buffer

	// Create a writer that processes lines and invokes the callback
	lineWriter := &lineCallbackWriter{
		callback: logCallback,
		allLogs:  &allLogs,
	}

	// Use stdcopy to demux Docker's multiplexed stdout/stderr stream
	// Docker adds an 8-byte header to each frame indicating stream type and size
	_, err = stdcopy.StdCopy(lineWriter, lineWriter, reader)
	if err != nil && err != io.EOF {
		return allLogs.String(), fmt.Errorf("failed to read logs: %w", err)
	}

	// Flush any remaining content in the buffer
	lineWriter.Flush()

	return allLogs.String(), nil
}

// lineCallbackWriter is an io.Writer that buffers input, extracts complete lines,
// and invokes a callback for each line while also collecting all output.
type lineCallbackWriter struct {
	callback func(string)
	allLogs  *bytes.Buffer
	buf      bytes.Buffer
}

// Write implements io.Writer. It buffers data, extracts complete lines,
// and invokes the callback for each line.
func (w *lineCallbackWriter) Write(p []byte) (n int, err error) {
	n = len(p)

	// Also write to the collected logs buffer
	w.allLogs.Write(p)

	// Write to our internal buffer
	w.buf.Write(p)

	// Process complete lines
	w.extractAndSendLines()

	return n, nil
}

// extractAndSendLines reads complete lines from the buffer and sends them to the callback.
func (w *lineCallbackWriter) extractAndSendLines() {
	if w.callback == nil {
		return
	}

	for {
		// Find the next newline in the buffer
		data := w.buf.Bytes()
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			// No complete line yet, keep buffering
			break
		}

		// Extract the line (without the newline)
		line := string(data[:idx])
		// Remove \r if present (Windows line endings)
		line = strings.TrimSuffix(line, "\r")

		// Call the callback with this line
		w.callback(line)

		// Advance the buffer past this line (including the newline)
		w.buf.Next(idx + 1)
	}
}

// Flush sends any remaining buffered content as a final line.
func (w *lineCallbackWriter) Flush() {
	if w.callback == nil || w.buf.Len() == 0 {
		return
	}

	line := strings.TrimRight(w.buf.String(), "\r\n")
	if line != "" {
		w.callback(line)
	}
	w.buf.Reset()
}

// Close cleans up firewall rules and releases Docker client resources.
// The network itself is kept for reuse on next agent startup.
func (e *Executor) Close() error {
	if e.client != nil {
		if e.networkID != "" {
			e.cleanupFirewallRules()
		}
		return e.client.Close()
	}
	return nil
}

// cleanupFirewallRules removes all iptables rules added by applyNetworkFirewallRules.
func (e *Executor) cleanupFirewallRules() {
	iface := "br-" + e.networkID[:12]

	// Remove DROP rules
	for _, subnet := range []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"127.0.0.0/8",
	} {
		e.deleteIptablesRule(iface, subnet, "DROP")
	}

	// Remove gateway ACCEPT rule
	e.deleteIptablesRule(iface, isolatedNetworkGateway, "ACCEPT")

	e.logger.Info("Firewall rules cleaned up")
}

// guessInterpreter returns the interpreter command for a script based on file extension.
// This enables runtime-agnostic script execution in SCRIPT mode.
func guessInterpreter(filename string) []string {
	switch {
	case strings.HasSuffix(filename, ".py"):
		return []string{"python"}
	case strings.HasSuffix(filename, ".js"):
		return []string{"node"}
	case strings.HasSuffix(filename, ".ts"):
		return []string{"npx", "tsx"}
	case strings.HasSuffix(filename, ".rb"):
		return []string{"ruby"}
	case strings.HasSuffix(filename, ".sh"):
		return []string{"/bin/sh"}
	case strings.HasSuffix(filename, ".R") || strings.HasSuffix(filename, ".r"):
		return []string{"Rscript"}
	case strings.HasSuffix(filename, ".jl"):
		return []string{"julia"}
	case strings.HasSuffix(filename, ".pl"):
		return []string{"perl"}
	case strings.HasSuffix(filename, ".lua"):
		return []string{"lua"}
	default:
		// Default to python for backward compatibility
		return []string{"python"}
	}
}

func (e *Executor) deleteIptablesRule(iface, dest, action string) {
	args := []string{
		"iptables", "-D", "DOCKER-USER",
		"-i", iface,
		"-d", dest,
		"-j", action,
	}
	if _, err := e.runHostCommand(args); err != nil {
		e.logger.Warn("Failed to remove iptables rule",
			zap.String("dest", dest),
			zap.String("action", action),
			zap.Error(err),
		)
	}
}
