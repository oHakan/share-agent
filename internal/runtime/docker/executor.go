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
}

// NewExecutor creates a new Docker container executor.
// It creates a client from environment variables (DOCKER_HOST, etc.)
// and sets up an isolated Docker network that allows internet egress
// but blocks all access to private/internal subnets and the host.
func NewExecutor(logger *zap.Logger) (*Executor, error) {
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	e := &Executor{
		client: cli,
		logger: logger,
	}

	if err := e.ensureIsolatedNetwork(context.Background()); err != nil {
		cli.Close()
		return nil, fmt.Errorf("failed to setup isolated network: %w", err)
	}

	return e, nil
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

// ContainerConfig holds resource limits, timeout settings, and dependencies for container execution.
type ContainerConfig struct {
	MemoryLimitMB  int64    // Memory limit in megabytes (e.g., 512 for 512MB)
	CPULimit       float64  // CPU limit in cores (e.g., 0.5 for half core, 1.0 for one core)
	VolumeLimitGB  int64    // Volume/disk limit in GB (0 = no limit)
	TimeoutSeconds int32    // Execution timeout in seconds (e.g., 60)
	Requirements   []string // Python package dependencies to install before execution (e.g., ["numpy", "pandas==1.5.0"])
	Script         string   // Python script content to execute (injected into container as task.py)
}

// RunContainer executes a container with the specified image and script.
// It uses in-memory script injection (zero disk footprint on host) by:
//  1. Creating the container first (without starting it)
//  2. Preparing the script as a tar archive in RAM
//  3. Injecting the tar archive into the container using CopyToContainer
//  4. Starting the container to execute the script
//
// Security controls enforced:
//   - TIMEOUT: Uses context.WithTimeout to enforce execution time limits
//   - MEMORY: Limits container memory via HostConfig.Resources.Memory
//   - CPU: Limits CPU via HostConfig.Resources.NanoCPUs
//   - NETWORK: Uses vortix-isolated network (internet egress allowed, private subnets blocked)
//
// The logCallback is invoked for each log line as it's received.
// If logCallback is nil, logs are collected but not streamed.
//
// The function ensures proper cleanup by removing the container after execution.
func (e *Executor) RunContainer(ctx context.Context, imageName string, config ContainerConfig, logCallback func(string)) (string, error) {
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
		return "", fmt.Errorf("failed to ensure image: %w", err)
	}

	// Step 3: Create container (do not start yet)
	// We need to inject the script before starting
	containerID, err := e.createContainer(timeoutCtx, imageName, config)
	if err != nil {
		return "", fmt.Errorf("failed to create container: %w", err)
	}

	// Ensure container is removed after we're done (Force=true for cleanup even on timeout)
	defer func() {
		removeCtx := context.Background() // Use fresh context for cleanup to ensure removal
		if removeErr := e.client.ContainerRemove(removeCtx, containerID, container.RemoveOptions{
			Force: true, // Force removal even if container is still running (e.g., on timeout)
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

	// Step 4: Inject script into container using in-memory tar archive
	// This avoids writing any files to the host disk (zero disk footprint)
	if err := e.injectScript(timeoutCtx, containerID, config.Script); err != nil {
		return "", fmt.Errorf("failed to inject script: %w", err)
	}

	e.logger.Debug("Script injected into container",
		zap.String("container_id", containerID),
	)

	// Step 5: Start the container (script is now inside)
	if err := e.client.ContainerStart(timeoutCtx, containerID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("failed to start container: %w", err)
	}

	e.logger.Debug("Container started",
		zap.String("container_id", containerID),
	)

	// Step 5.5: Start volume monitoring goroutine if limit is set
	var volumeExceeded bool
	var volumeExceededBytes int64
	volumeLimitBytes := config.VolumeLimitGB * 1024 * 1024 * 1024 // Convert GB to bytes

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
					// Check container disk usage
					usedBytes, err := e.getContainerDiskUsage(volumeCtx, containerID)
					if err != nil {
						// Container might have stopped, ignore
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
						// Kill the container
						_ = e.client.ContainerKill(context.Background(), containerID, "SIGKILL")
						return
					}
				}
			}
		}()
	}

	// Step 6: Stream logs in real-time while container is running
	logs, err := e.streamLogs(timeoutCtx, containerID, logCallback)
	if err != nil {
		// Check if this was a timeout
		if timeoutCtx.Err() == context.DeadlineExceeded {
			timeoutMsg := "Process killed due to timeout."
			e.logger.Warn(timeoutMsg,
				zap.String("container_id", containerID),
				zap.Int32("timeout_seconds", config.TimeoutSeconds),
			)
			if logCallback != nil {
				logCallback(timeoutMsg)
			}
			return logs + "\n" + timeoutMsg, fmt.Errorf("execution timeout: %w", timeoutCtx.Err())
		}
		return "", fmt.Errorf("failed to stream logs: %w", err)
	}

	// Step 7: Wait for container to finish
	statusCh, errCh := e.client.ContainerWait(timeoutCtx, containerID, container.WaitConditionNotRunning)

	select {
	case err := <-errCh:
		if err != nil {
			// Check for volume limit exceeded first
			if volumeExceeded {
				usedGB := float64(volumeExceededBytes) / (1024 * 1024 * 1024)
				volumeMsg := fmt.Sprintf("Volume limit exceeded (%.2f GB / %d GB)", usedGB, config.VolumeLimitGB)
				e.logger.Error(volumeMsg,
					zap.String("container_id", containerID),
				)
				if logCallback != nil {
					logCallback(volumeMsg)
				}
				return logs + "\n" + volumeMsg, fmt.Errorf("volume limit exceeded")
			}
			// Check for timeout
			if timeoutCtx.Err() == context.DeadlineExceeded {
				timeoutMsg := "Process killed due to timeout."
				e.logger.Warn(timeoutMsg,
					zap.String("container_id", containerID),
					zap.Int32("timeout_seconds", config.TimeoutSeconds),
				)
				if logCallback != nil {
					logCallback(timeoutMsg)
				}
				return "", fmt.Errorf("execution timeout: %w", timeoutCtx.Err())
			}
			return "", fmt.Errorf("error waiting for container: %w", err)
		}
	case status := <-statusCh:
		// Check for volume limit exceeded
		if volumeExceeded {
			usedGB := float64(volumeExceededBytes) / (1024 * 1024 * 1024)
			volumeMsg := fmt.Sprintf("Volume limit exceeded (%.2f GB / %d GB)", usedGB, config.VolumeLimitGB)
			e.logger.Error(volumeMsg,
				zap.String("container_id", containerID),
			)
			if logCallback != nil {
				logCallback(volumeMsg)
			}
			return logs + "\n" + volumeMsg, fmt.Errorf("volume limit exceeded")
		}
		if status.StatusCode != 0 {
			if status.StatusCode == 137 {
				timeoutMsg := "Memory Limit Exceeded."
				e.logger.Error(timeoutMsg,
					zap.String("container_id", containerID),
					zap.Int32("timeout_seconds", config.TimeoutSeconds),
				)
				if logCallback != nil {
					logCallback(timeoutMsg)
				}
				return "", fmt.Errorf("memory limit exceeded")
			}
		}
		e.logger.Debug("Container finished",
			zap.String("container_id", containerID),
			zap.Int64("exit_code", status.StatusCode),
		)
	case <-timeoutCtx.Done():
		// Check for volume limit exceeded first
		if volumeExceeded {
			usedGB := float64(volumeExceededBytes) / (1024 * 1024 * 1024)
			volumeMsg := fmt.Sprintf("Volume limit exceeded (%.2f GB / %d GB)", usedGB, config.VolumeLimitGB)
			e.logger.Error(volumeMsg,
				zap.String("container_id", containerID),
			)
			if logCallback != nil {
				logCallback(volumeMsg)
			}
			return logs + "\n" + volumeMsg, fmt.Errorf("volume limit exceeded")
		}
		// Timeout occurred - append clear message to logs
		timeoutMsg := "Process killed due to timeout."
		e.logger.Warn(timeoutMsg,
			zap.String("container_id", containerID),
			zap.Int32("timeout_seconds", config.TimeoutSeconds),
		)
		if logCallback != nil {
			logCallback(timeoutMsg)
		}
		return logs + "\n" + timeoutMsg, fmt.Errorf("execution timeout: %w", timeoutCtx.Err())
	}

	return logs, nil
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

// injectScript creates a tar archive containing the script in RAM and copies it into the container.
// This achieves zero disk footprint on the host by never writing to the host filesystem.
//
// The script is placed at /app/task.py inside the container.
func (e *Executor) injectScript(ctx context.Context, containerID string, script string) error {
	// Step 1: Create a buffer to hold the tar archive in RAM
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Step 2: Write the tar header for task.py
	scriptBytes := []byte(script)
	hdr := &tar.Header{
		Name: "task.py",
		Mode: 0644,
		Size: int64(len(scriptBytes)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("failed to write tar header: %w", err)
	}

	// Step 3: Write the script content
	if _, err := tw.Write(scriptBytes); err != nil {
		return fmt.Errorf("failed to write script to tar: %w", err)
	}

	// Step 4: Close the tar writer to finalize the archive
	if err := tw.Close(); err != nil {
		return fmt.Errorf("failed to close tar writer: %w", err)
	}

	// Step 5: Copy the tar archive into the container at /app
	// This places task.py at /app/task.py inside the container
	err := e.client.CopyToContainer(ctx, containerID, "/app", &buf, container.CopyToContainerOptions{})
	if err != nil {
		return fmt.Errorf("failed to copy script to container: %w", err)
	}

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
	// Build command based on requirements.
	// All containers use the vortix-isolated network which allows internet egress
	// but blocks all private/internal subnets via iptables rules.
	var finalCmd []string

	if len(config.Requirements) > 0 {
		e.logger.Info("Installing dependencies before script execution",
			zap.Strings("requirements", config.Requirements),
		)

		pipInstallCmd := "pip install --user --no-cache-dir " + strings.Join(config.Requirements, " ")
		combinedCmd := pipInstallCmd + " && python task.py"
		finalCmd = []string{"/bin/sh", "-c", combinedCmd}

		e.logger.Debug("Built combined command with pip install",
			zap.String("combined_command", combinedCmd),
		)
	} else {
		finalCmd = []string{"python", "task.py"}
	}

	containerConfig := &container.Config{
		Image: imageName,
		Cmd:   finalCmd,
		Tty:   false, // Don't allocate a pseudo-TTY
		Env: []string{
			"HOME=/tmp",
			// Python'un --user ile kurduğu paketleri bulabilmesi için PATH güncellemesi
			"PATH=/tmp/.local/bin:/usr/local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		},
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
	//
	// Memory: Convert MB to Bytes
	// Docker expects memory limit in bytes, so we multiply MB by 1024*1024
	memoryBytes := config.MemoryLimitMB * 1024 * 1024
	//
	// CPU: Convert CPU cores to NanoCPUs
	// Docker uses NanoCPUs where 1 CPU core = 1,000,000,000 (1e9) nanoseconds
	// For example:
	//   - 0.5 cores = 500,000,000 NanoCPUs (half a CPU core)
	//   - 1.0 cores = 1,000,000,000 NanoCPUs (one full CPU core)
	//   - 2.0 cores = 2,000,000,000 NanoCPUs (two CPU cores)
	// This provides precise CPU allocation in billionths of a CPU
	nanoCPUs := int64(config.CPULimit * 1_000_000_000)

	hostConfig := &container.HostConfig{
		AutoRemove: false, // We'll remove manually after getting logs
		Resources: container.Resources{
			Memory:     memoryBytes, // Memory limit in bytes
			MemorySwap: memoryBytes,
			NanoCPUs:   nanoCPUs, // CPU limit in nanoseconds per second
		},
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
		zap.Int64("nano_cpus", nanoCPUs),
		zap.String("network", isolatedNetworkName),
		zap.Strings("cap_drop", hostConfig.CapDrop),
		zap.Strings("security_opt", hostConfig.SecurityOpt),
		zap.Bool("privileged", hostConfig.Privileged),
		zap.String("user", containerConfig.User),
		zap.String("working_dir", containerConfig.WorkingDir),
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
