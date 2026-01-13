# Installation Output Examples

## Başarılı Kurulum Örneği

```
Vortix Agent Installation

[~] Vortix Agent v0.2.9 Installing...

[✓] System check passed (16 Cores, 32GB RAM, 2x RTX 4090)
[✓] Secure tunnel established
[➜] Node "Vortix-Titan" is now ONLINE
```

## Detailed Installation Flow

### Step 1: System Verification
```
[~] Vortix Agent v0.2.9 Installing...
```
- Agent başlatılıyor ve system check hazırlanıyor

### Step 2: Hardware Detection
```
[✓] System check passed (16 Cores, 32GB RAM, 2x RTX 4090)
```
- 16 CPU cores
- 32 GB RAM
- 2 adet NVIDIA RTX 4090 GPU

### Step 3: Network Connection
```
[✓] Secure tunnel established
```
- Orchestrator'a güvenli bağlantı kuruldu
- gRPC over encrypted channel

### Step 4: Node Registration
```
[➜] Node "Vortix-Titan" is now ONLINE
```
- Node ID: `Vortix-Titan` (machine hostname)
- Status: ONLINE ve ready for jobs

## Error Scenarios

### Docker Not Available
```
[~] Vortix Agent v0.2.9 Installing...

[✓] System check passed (16 Cores, 32GB RAM, CPU Only)
[✗] Docker daemon connection failed
[⚠] Continuing in degraded mode (job execution disabled)
```

### Network Connection Failure
```
[~] Vortix Agent v0.2.9 Installing...

[✓] System check passed (16 Cores, 32GB RAM, 2x RTX 4090)
[✗] Secure tunnel establishment failed
[⚠] Retrying connection (attempt 1 of 5)
```

### GPU Detection Issues
```
[~] Vortix Agent v0.2.9 Installing...

[✓] System check passed (16 Cores, 32GB RAM, CPU Only)
[⚠] NVIDIA drivers not detected
[✓] Secure tunnel established
[➜] Node "Vortix-Titan" is now ONLINE (CPU mode)
```

## Log Output Details

### Standard Zap Logs (JSON - Production)
```json
{
  "level": "info",
  "ts": 1673553623.456789,
  "caller": "cmd/agent/main.go:155",
  "msg": "Starting DePIN GPU Agent",
  "version": "v0.2.9",
  "dev_mode": false,
  "orchestrator": "****.***.***.**:50051"
}
```

### Development Logs (Human-Readable)
```
2026-01-13T18:15:23.456Z  INFO  cmd/agent/main.go:155  Starting DePIN GPU Agent
    {"version": "v0.2.9", "dev_mode": false, "orchestrator": "****.***.***.**:50051"}
```

## Security Features in Logs

### IP Masking
```
❌ Maskelenmeyen: "Connecting to 192.168.1.100:50051"
✅ Maskelenen: "Connecting to ***.***.***.**:50051"
```

### API Key Protection
```
❌ Maskelenmeyen: "api-key=sk_live_abc123xyz789"
✅ Maskelenen: "api-key=[REDACTED]"
```

### Token Protection
```
❌ Maskelenmeyen: "authorization: Bearer eyJhbGc..."
✅ Maskelenen: "authorization: Bearer [REDACTED]"
```

## Version Information

### Binary Version Check
```bash
$ ./bin/vortix-agent --version-info
Vortix Agent v0.2.9 (commit: 411e934, built: 2026-01-13T18:11:39Z)
```

### Makefile Version Command
```bash
$ make version
Version: v0.2.9
Commit: 411e934
Date: 2026-01-13T18:10:13Z
```

## Color Output (Terminal)

The formatter uses ANSI color codes when outputting to terminal:
- **Green** `[✓]` Success messages
- **Red** `[✗]` Error messages
- **Yellow** `[⚠]` Warning messages
- **Cyan** `[➜]` Info messages
- **Blue** `[~]` Pending messages

When piped or redirected, the ANSI codes are preserved for proper rendering in:
- Log files
- CI/CD pipelines
- Terminal multiplexers (tmux, screen)

## Real-Time Monitoring Example

```bash
$ ./bin/vortix-agent 2>&1 | tee agent.log

2026-01-13T18:15:23.456Z  INFO  cmd/agent/main.go:155  Starting DePIN GPU Agent
[~] Vortix Agent v0.2.9 Installing...
[✓] System check passed (16 Cores, 32GB RAM, 2x RTX 4090)
[✓] Secure tunnel established
[➜] Node "Vortix-Titan" is now ONLINE

2026-01-13T18:15:28.789Z  INFO  cmd/agent/main.go:278  Agent is running. Press Ctrl+C to stop.
```
