# Vortix Agent

DePIN GPU compute agent for Vortix Cloud. Users install this on their machines; the orchestrator assigns containerized jobs (Python scripts) that the agent executes in sandboxed Docker containers.

## Quick Reference

```bash
make build          # Build local binary
make run            # Build + run
make run-with-key   # Build + run with API_KEY
make test           # go test -v ./...
make lint           # golangci-lint
make fmt            # gofmt
make build-all      # Cross-compile (linux/darwin/windows, amd64/arm64)
```

## Project Structure

```
cmd/agent/main.go          # Entry point: config, discovery, register, stream
internal/
  client/grpc_client.go     # gRPC client: Connect, Register, StreamEvents, RTT
  config/config.go          # Viper config + build-time ldflags (Version, Commit, etc.)
  hardware/
    gpu/nvidia.go           # GPU types & interfaces (Discoverer, GPUInfo, DiscoveryResult)
    gpu/nvidia_linux.go     # NVML via go-nvml (CGO required)
    gpu/nvidia_windows.go   # nvidia-smi CLI wrapper
    gpu/nvidia_other.go     # CPU-only fallback (macOS, non-CGO Linux)
    host/machine.go         # CPU/RAM/Disk/OS info via gopsutil
  runtime/docker/
    client.go               # Docker daemon health check (DockerChecker)
    executor.go             # Container lifecycle: pull, create, inject script, run, stream logs
  limits/limits.go          # Thread-safe resource limits store (CPU, RAM, Volume)
  telemetry/collector.go    # Real-time CPU/RAM/GPU stats for heartbeat
pkg/logger/
  logger.go                 # Zap logger (dev=colored console, prod=JSON)
  formatter.go              # User-friendly status icons + sensitive data sanitizer
proto/
  depin.proto               # gRPC service: NodeService{Register, StreamEvents}
  depin.pb.go               # Generated protobuf
  depin_grpc.pb.go          # Generated gRPC stubs
```

## Architecture

### Lifecycle

1. **Discovery** - Parallel (WaitGroup): host info, GPU detection, Docker check
2. **Registration** - gRPC `Register(NodeInfo)` -> receives `RegistrationResponse` + resource limits
3. **Streaming** - Bidirectional gRPC stream: sends heartbeat every 3s, receives jobs + limit updates + heartbeat acks
4. **Job Execution** - Each job runs in a separate goroutine with Docker container sandbox

### gRPC Protocol

```
Agent sends:   AgentEvent { oneof: Heartbeat | JobResult | JobLog }
Agent receives: ServerEvent { oneof: JobRequest | ResourceLimitsUpdate | HeartbeatAck }
```

- Heartbeat includes `sent_timestamp_ms` for RTT measurement
- Jobs stream logs line-by-line via `JobLog` messages
- Resource limits can be updated dynamically from orchestrator

### Container Sandbox

- `CapDrop: ALL`, `User: 1000:1000`, `no-new-privileges`
- `NetworkMode: "none"` (switches to `"bridge"` when pip packages needed)
- Script injected as in-memory tar archive to `/app/task.py`
- Resource limits: Memory (OOM at limit), CPU (NanoCPUs), Disk (volume monitor goroutine), Time (context timeout)

### Platform-Specific GPU Support

| Platform | Build tag | GPU |
|---|---|---|
| Linux + CGO | `linux && cgo` | NVML library |
| Windows | `windows` | nvidia-smi CLI |
| macOS / Linux no-CGO | `(!linux && !windows) \|\| (linux && !cgo)` | CPU-only fallback |

## Configuration

Priority: CLI flags > env vars (`AGENT_*` prefix, `VORTIX_API_KEY`) > config files (`./agent.yaml`) > defaults

Build-time variables injected via ldflags in Makefile:
- `config.Version`, `config.Commit`, `config.Date`, `config.DevMode`, `config.OrchestratorAddressVar`

## Key Interfaces

```go
gpu.Discoverer      { Discover(ctx), GetUsageStats(ctx), Close() }
host.Collector      { Collect(ctx) (*HostInfo, error) }
docker.Checker      { Check(ctx) (*DockerInfo, error); Close() }
telemetry.Collector { Collect(ctx) (*TelemetryData, error) }
```

## Concurrency Model

- WaitGroup for parallel discovery
- Channel-based send queue in StreamEvents
- Per-job goroutines for container execution
- RWMutex on limits store and RTT tracker
- Volume monitoring goroutine per container (2s interval)

## Important Patterns

- **Retry**: Exponential backoff 1s->30s, max 5 retries, only for transient gRPC codes
- **Graceful degradation**: GPU fail -> CPU-only mode; Docker unavailable -> warn or fail per config
- **Log sanitization**: IPs, API keys, Bearer/JWT tokens, MAC addresses are auto-redacted
- **Resource clamping**: Job resource requests are clamped to `limits.Store` values before container creation

## Module Path

`github.com/depin-agent/agent` (Go 1.24)

## Dependencies

- `github.com/NVIDIA/go-nvml` - GPU discovery (Linux)
- `github.com/docker/docker` - Container execution
- `github.com/shirou/gopsutil/v3` - Host stats + telemetry
- `github.com/spf13/viper` - Configuration
- `go.uber.org/zap` - Logging
- `google.golang.org/grpc` + `protobuf` - Orchestrator communication
