# Logging & Bootstrap System - Quick Start

## 📌 Overview

Vortix Agent'ın logging sistemi iki kısımdan oluşur:

1. **User-Friendly Bootstrap Output** - Installation sırasında renkli, adım-adım mesajlar
2. **Structured Zap Logs** - Production-grade JSON logs ve debug output

## 🚀 Quick Start

### Build & Run
```bash
# Build
make build

# Run with installation output
./bin/vortix-agent
```

### Check Version
```bash
# Makefile'dan
make version

# Binary'den
./bin/vortix-agent --version-info
```

## 📝 Logging Architecture

```
┌─────────────────────────────────┐
│  cmd/agent/main.go              │
│  ├─ fmt.Println() → Terminal    │ ← User-Friendly Output
│  └─ log.Info() → Zap            │ ← Structured Logs
└─────────────────────────────────┘
            │
            ├─ pkg/logger/formatter.go
            │  └─ Formatter, Sanitizer
            │
            ├─ pkg/logger/logger.go
            │  └─ Zap Configuration
            │
            └─ internal/client/grpc_client.go
               └─ Safe logging with IP masking
```

## 🎯 Features

### ✅ User-Friendly Installation Steps
- Renkli emojiler: `[✓]`, `[✗]`, `[⚠]`, `[➜]`, `[~]`
- Adım-adım progress
- Sistem bilgisi: CPU cores, RAM, GPU modeli

### ✅ Sensitive Data Protection
- 📍 IP adresleri maskeleme
- 🔑 API keys gizleme
- 🔐 Tokens redaction
- 🖥️ MAC addresses maskeleme

### ✅ Automatic Version Management
- Git tags'den otomatik çekme
- Build time'da `ldflags` ile injection
- Runtime'da `--version-info` flag'ı

## 📂 Key Files

| Dosya | Rol |
|-------|-----|
| `pkg/logger/formatter.go` | User-friendly output formatter |
| `pkg/logger/logger.go` | Zap logger setup |
| `cmd/agent/main.go` | Installation steps & bootstrap |
| `internal/config/config.go` | Version variables |
| `Makefile` | Build & version management |
| `docs/LOGGING_IMPROVEMENTS.md` | Detailed documentation |
| `docs/INSTALLATION_OUTPUT.md` | Output examples |

## 🔧 Configuration

### Dev Mode
```bash
# JSON logs, verbose output
./bin/vortix-agent

# Dev-friendly console logs
AGENT_DEV_MODE=true ./bin/vortix-agent
```

### Production
```bash
# Default: JSON structured logs
./bin/vortix-agent --api-key=your-key
```

## 🎨 Output Examples

### Installation Success
```
Vortix Agent Installation

[~] Vortix Agent v0.2.9 Installing...

[✓] System check passed (16 Cores, 32GB RAM, 2x RTX 4090)
[✓] Secure tunnel established
[➜] Node "Vortix-Titan" is now ONLINE
```

### With Logs
```
2026-01-13T18:15:23.456Z  INFO  Starting DePIN GPU Agent {"version": "v0.2.9"}
[~] Vortix Agent v0.2.9 Installing...
[✓] System check passed (16 Cores, 32GB RAM, 2x RTX 4090)
[✓] Secure tunnel established
[➜] Node "Vortix-Titan" is now ONLINE
2026-01-13T18:15:28.789Z  INFO  Agent is running. Press Ctrl+C to stop.
```

## 🔒 Security Considerations

### What Gets Logged
✅ Version information  
✅ System specs (CPU count, RAM, GPU model)  
✅ Status messages  
✅ Error types  

### What Gets Masked/Redacted
❌ IP addresses  
❌ API keys  
❌ Authentication tokens  
❌ MAC addresses  
❌ Sensitive environment variables  

Example:
```
❌ LOG: "Connected to 192.168.1.100 with api-key=sk_live_xyz123"
✅ LOG: "Connected to ***.***.***.**  with api-key=[REDACTED]"
```

## 🔄 Version Management Workflow

### 1. Tag a New Release
```bash
git tag v1.0.0
git push origin v1.0.0
```

### 2. Local Build
```bash
make build  # Automatically reads v1.0.0 from git
./bin/vortix-agent --version-info
# Output: Vortix Agent v1.0.0 (commit: abc1234, built: 2026-01-13T18:15:23Z)
```

### 3. Production Release (CI/CD)
```bash
# GitHub Actions automatically:
# 1. Reads git tag
# 2. Runs GoReleaser
# 3. Builds all platforms
# 4. Creates releases
```

## 🧪 Testing

### Test Formatter Output
```bash
# Create test file
cat > test_log.go << 'EOF'
package main
import (
    "fmt"
    "github.com/depin-agent/agent/pkg/logger"
)
func main() {
    f := logger.NewFormatter(true)
    fmt.Println(f.Success("Test", "details"))
}
EOF

go run test_log.go
```

### Test Sanitizer
```bash
# Create sanitizer test
cat > test_sanitize.go << 'EOF'
package main
import (
    "fmt"
    "github.com/depin-agent/agent/pkg/logger"
)
func main() {
    s := logger.NewSanitizer(true)
    msg := "IP: 192.168.1.1, Key: api-key=abc123"
    fmt.Println(s.Sanitize(msg))
}
EOF

go run test_sanitize.go
# Output: IP: ***.***.***.**, Key: api-key=[REDACTED]
```

## 📊 Log Levels

| Level | Usage | Example |
|-------|-------|---------|
| **Debug** | Development info | "Starting GPU discovery" |
| **Info** | Important events | "Node registered successfully" |
| **Warn** | Non-fatal issues | "Docker connection failed, continuing in degraded mode" |
| **Error** | Recoverable errors | "Failed to collect telemetry" |
| **Fatal** | Unrecoverable errors | "Configuration load failed" |

## 🎯 Best Practices

### When Adding Logs

✅ **DO:**
```go
// Use structured fields
log.Info("Node registered",
    zap.String("node_id", nodeID),
    zap.String("status", status),
)

// Sanitize sensitive data
safeAddr := sanitizer.Sanitize(address)
log.Info("Connected", zap.String("addr", safeAddr))
```

❌ **DON'T:**
```go
// Don't log raw IPs
log.Info("Connected to " + ipAddress)

// Don't log API keys
log.Info("Using key: " + apiKey)

// Don't use fmt.Println for important info
fmt.Println("Agent started on " + port)
```

### When Using Formatter

✅ **DO:**
```go
// Bootstrap output for user visibility
fmt.Println(formatter.Success("System check passed", details))
fmt.Println(formatter.Info("Node \"name\" is now ONLINE"))
```

❌ **DON'T:**
```go
// Don't use formatter for debug info
fmt.Println(formatter.Info("Debug info: " + debugData))

// Don't spam with formatter output
for i := 0; i < 1000; i++ {
    fmt.Println(formatter.Pending("Processing..."))
}
```

## 📚 Additional Resources

- [Logging Improvements Detail](./LOGGING_IMPROVEMENTS.md)
- [Installation Output Examples](./INSTALLATION_OUTPUT.md)
- [Version Management](./VERSION_MANAGEMENT.md)
- [Zap Logger Docs](https://github.com/uber-go/zap)

## 🆘 Troubleshooting

### Colors not showing?
- Check if output is TTY
- Some terminals need explicit color support
- Use `--color=always` flag if needed

### Log file is too large?
- Rotate logs (implement in future)
- Filter by level: `grep "ERROR" agent.log`
- Use JSON parsing for analysis

### Can't find version?
```bash
# Check if tag exists
git describe --tags --always

# Rebuild without cache
make clean && make build
./bin/vortix-agent --version-info
```

## 📞 Support

For logging-related issues:
1. Check [LOGGING_IMPROVEMENTS.md](./LOGGING_IMPROVEMENTS.md)
2. Review examples in [INSTALLATION_OUTPUT.md](./INSTALLATION_OUTPUT.md)
3. Check source code comments in `pkg/logger/`
