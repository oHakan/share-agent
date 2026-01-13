# Version Management & Build System

## 📋 Özet

Vortix Agent'ın versiyon yönetimi GitHub tags'den otomatik olarak çekiliyor. Build zamanında `ldflags` ile versiyon bilgisi binary'ye gömülüyor.

## 🏗️ Sistem Mimarisi

### 1. Git Tags (Versiyonlandırma Kaynağı)
```bash
# Mevcut tag
$ git describe --tags --always
v0.2.9

# Yeni versiyon tagglemek
$ git tag v1.0.0
$ git push origin v1.0.0
```

### 2. Build Time Version Injection
GoReleaser (`goreleaser.yml`) ve Makefile her ikisi de aynı metodolojiye sahip:

```yaml
# .goreleaser.yml (CI/CD)
ldflags:
  - -X github.com/depin-agent/agent/internal/config.Version={{.Version}}
  - -X github.com/depin-agent/agent/internal/config.Commit={{.ShortCommit}}
  - -X github.com/depin-agent/agent/internal/config.Date={{.Date}}
```

```makefile
# Makefile (Local Development)
VERSION ?= $(shell git describe --tags --always)
COMMIT ?= $(shell git rev-parse --short HEAD)
DATE ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')

LDFLAGS := -ldflags "-s -w \
	-X github.com/depin-agent/agent/internal/config.Version=$(VERSION) \
	-X github.com/depin-agent/agent/internal/config.Commit=$(COMMIT) \
	-X github.com/depin-agent/agent/internal/config.Date=$(DATE)"
```

### 3. Build Time Variables (`internal/config/config.go`)
```go
var (
	Version = "dev"     // Overwritten at build time
	Commit  = "unknown" // Overwritten at build time
	Date    = "unknown" // Overwritten at build time
)

// Yeni fonksiyon: GetVersionInfo()
func GetVersionInfo() string {
	return fmt.Sprintf("Vortix Agent %s (commit: %s, built: %s)", Version, Commit, Date)
}
```

## 🛠️ Kullanım

### Local Build
```bash
# Versiyonu otomatik olarak git tag'den çeker
make build
# Output: Building vortix-agent v0.2.9...

# Version bilgisini görmek
make version
# Output:
# Version: v0.2.9
# Commit: 411e934
# Date: 2026-01-13T18:10:13Z
```

### Runtime Bilgisi
```bash
# Binary'yi çalıştırırken versiyon bilgisini görmek
./bin/vortix-agent --version-info
# Output: Vortix Agent v0.2.9 (commit: 411e934, built: 2026-01-13T18:11:39Z)
```

### Production Build (GoReleaser)
```bash
# CI/CD pipeline'da (GitHub Actions, etc.)
goreleaser release
# Otomatik olarak github tag'den versiyon çeker ve multiple platform'lar için build yapar
```

## 📁 Dosyalar

- `Makefile` - Development build tool'u
- `.goreleaser.yml` - Production release configuration
- `internal/config/config.go` - Build-time version variables
- `cmd/agent/main.go` - Version flag handler

## 🔄 Versiyon Güncelleme Akışı

1. **Development**
```bash
git tag v1.0.0
make build  # v1.0.0 otomatik çekiliyor
```

2. **Production (CI/CD)**
```bash
git tag v1.0.0
git push origin v1.0.0
# GitHub Actions/CI otomatik olarak:
# - goreleaser tarafından tüm platformlar için build yapılır
# - Version bilgisi otomatik gömülür
# - Binary'ler GitHub Releases'a upload edilir
```

## ✅ Avantajlar

✅ Tek kaynak (Git tags) - Version information'ı deuplicate etmez  
✅ Otomatik - Manual version güncelleme gerekmez  
✅ CI/CD friendly - Pipeline'lar binary'den versiyon bilgisini okuyabilir  
✅ Transparent - `--version-info` ile runtime'da görebilir  
✅ Cross-platform - Linux, macOS, Windows için aynı system  
