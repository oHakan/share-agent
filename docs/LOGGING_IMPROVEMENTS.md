# Logging Enhancement & User-Friendly Output

## 🎨 Yapılan Geliştirmeler

### 1. **User-Friendly Formatter Module**
**Dosya:** `pkg/logger/formatter.go`

✅ Başlık formatı (Header)
```
Vortix Agent Installation
```

✅ İşlem durumu göstergeleri:
- `[✓]` Success (yeşil)
- `[✗]` Error (kırmızı)  
- `[⚠]` Warning (sarı)
- `[➜]` Info (mavi)
- `[~]` Pending (mavi)

✅ Detaylı açıklamalar:
```
[✓] System check passed (16 Cores, 32GB RAM, 2x RTX 4090)
[✓] Secure tunnel established
[➜] Node "Vortix-Titan" is now ONLINE
```

### 2. **Sensitive Data Sanitization**
**Sınıfı:** `Sanitizer` (formatter.go)

Otomatik olarak aşağıdakiler maskesiz hale getirilir:
- 📍 IPv4 adresleri: `192.168.1.1` → `***.***.***.**`
- 📍 IPv6 adresleri: `2001:db8::1` → `****:****:****:****`
- 🔑 API Keys: `api-key=abc123xyz` → `api-key=[REDACTED]`
- 🔐 Tokens: `bearer eyJhb...` → `bearer [REDACTED]`
- 🖥️ MAC Adresleri: `00:1A:2B:3C:4D:5E` → `**:**:**:**:**:**`

### 3. **Version Management**
**Dosya:** `Makefile` + `internal/config/config.go`

Git tags'den otomatik versiyon çekimi:
```bash
# Build time'da ldflags ile injection
-X github.com/depin-agent/agent/internal/config.Version=$(VERSION)
-X github.com/depin-agent/agent/internal/config.Commit=$(COMMIT)
-X github.com/depin-agent/agent/internal/config.Date=$(DATE)
```

Runtime'da versiyon bilgisi:
```bash
./bin/vortix-agent --version-info
# Output: Vortix Agent v0.2.9 (commit: 411e934, built: 2026-01-13T18:11:39Z)
```

### 4. **Installation Bootstrap Logları**
**Dosya:** `cmd/agent/main.go`

Kurulum aşamaları adım-adım gösterilir:

```
➜ Vortix Agent v0.2.9 Installing...

[✓] System check passed (16 Cores, 32GB RAM, 2x RTX 4090)
[✓] Secure tunnel established
[➜] Node "Vortix-Titan" is now ONLINE
```

### 5. **gRPC Client IP Maskeleme**
**Dosya:** `internal/client/grpc_client.go`

Bağlantı loglarında IP adresleri otomatik maskeli:
```go
safeAddress := sanitizer.Sanitize(c.config.OrchestratorAddress)
```

## 📋 Kullanılan Yapı

### Main Initialization Flow

```go
// 1. Formatter oluştur
formatter := logger.NewFormatter(!cfg.DevMode)

// 2. Header göster
fmt.Println(formatter.Header("Vortix Agent Installation"))
fmt.Println(formatter.Pending("Vortix Agent v0.2.9 Installing..."))

// 3. Discovery çalıştır (formatter'ı geç)
capacity, err := runDiscovery(ctx, cfg, log, formatter)

// 4. System check göster
fmt.Println(formatter.Success("System check passed", "16 Cores, 32GB RAM, 2x RTX 4090"))

// 5. Registration'a geç
registerAndStream(ctx, cfg, capacity, executor, log, formatter)

// 6. Node online mesajı
fmt.Println(formatter.Info("Node \"Vortix-Titan\" is now ONLINE"))
```

## 🔒 Güvenlik Özellikleri

### API Key & Token Maskeleme
```
❌ LOG: "api_key=sk_live_51234567890abcdef"
✅ LOG: "api_key=[REDACTED]"
```

### IP Gizleme
```
❌ LOG: "Connecting to 192.168.1.100:50051"
✅ LOG: "Connecting to ***.***.***.**:50051"
```

### MAC Address Gizleme
```
❌ LOG: "Host MAC: 00:1A:2B:3C:4D:5E"
✅ LOG: "Host MAC: **:**:**:**:**:**"
```

## 📁 İlgili Dosyalar

| Dosya | Amaç |
|-------|------|
| `pkg/logger/formatter.go` | User-friendly formatter & sanitizer |
| `pkg/logger/logger.go` | Zap logger configuration |
| `cmd/agent/main.go` | Bootstrap & installation steps |
| `internal/client/grpc_client.go` | gRPC client with safe logging |
| `internal/config/config.go` | Version info functions |
| `Makefile` | Build-time version injection |
| `.goreleaser.yml` | Release configuration |

## 🚀 Kullanım Örnekleri

### Development Build
```bash
make build
./bin/vortix-agent
```

### Version Bilgisi
```bash
make version
./bin/vortix-agent --version-info
```

### Production Build (CI/CD)
```bash
goreleaser release
# Otomatik olarak tüm platformlar için build yapar
# Version git tags'den çekilir
```

## ✨ Renkli Output Desteği

- Terminal'e direkt yazıldığında: **Renkli**
- Pipe'lanmış çıktı (`|`): Renkler korunur
- File'a yönlendirilmiş (`>`): ANSI kodları içerilir (optional olarak strip edilebilir)

## 🔄 Güncelleme Akışı

1. **Yeni versiyon tagglemesi**
   ```bash
   git tag v1.0.0
   git push origin v1.0.0
   ```

2. **Local Build**
   ```bash
   make build  # v1.0.0 otomatik çekilir
   ```

3. **Binary Test**
   ```bash
   ./bin/vortix-agent --version-info
   # Output: Vortix Agent v1.0.0 ...
   ```

4. **Production Release** (CI/CD)
   ```bash
   # GitHub Actions otomatik olarak:
   # - goreleaser çalıştırır
   # - Tüm platformlar için build yapar
   # - Releases oluşturur
   ```

## 📊 Avantajlar

✅ **Kullanıcı dostu** - Renkli, işlemli, açık mesajlar  
✅ **Güvenli** - Sensitive veri otomatik maskeli  
✅ **Tutarlı** - Tüm uygulamada aynı format  
✅ **Otomatik** - Versiyon yönetimi git'ten  
✅ **Cross-platform** - Linux, macOS, Windows uyumlu  
✅ **Bakım kolay** - Formatter modülü bağımsız ve reusable  
