# Container Execution Improvements

Agent'in genel amacli bir container execution platformu olarak kullanilabilmesi icin gerekli iyilestirmeler.
Kullanici herhangi bir workload calistirabilmeli: ML model egitimi, veri isleme, rendering, inference, vb.

---

## 1. GPU Passthrough

**Durum:** Kesfediliyor ama container'a verilmiyor
**Oncelik:** Kritik
**Etki:** executor.go
**Proto degisikligi:** Hayir

### Problem

GPU discovery calisiyor, orchestrator'a raporlaniyor ama container olusturulurken `DeviceRequests` eklenmemis.
Container icinde `nvidia-smi` bile calismaz, CUDA kulanilamaz.

### Cozum

`createContainer()` icinde `HostConfig.Resources`'a `DeviceRequests` eklenecek:

```go
DeviceRequests: []container.DeviceRequest{
    {
        Driver:       "nvidia",
        Count:        -1, // tum GPU'lar
        Capabilities: [][]string{{"gpu"}},
    },
}
```

### Tasarim Kararlari

- GPU mevcut degilse (CPU-only node) `DeviceRequests` eklenmemeli, yoksa container olusturulamaz.
- Discovery sirasinda tespit edilen GPU bilgisi executor'a aktarilmali.
- Orchestrator ileride job bazinda GPU sayisi belirleyebilir (`Count` fieldi). Su an icin mevcut tum GPU'lar verilecek.
- Container icine `NVIDIA_VISIBLE_DEVICES=all` ve `NVIDIA_DRIVER_CAPABILITIES=compute,utility` env var'lari set edilmeli.

### Gereksinim

Node provider'da **NVIDIA Container Toolkit** kurulu olmali. Agent bu kontrolu yapmiyor (bkz. Madde 6).

---

## 2. Shared Memory (SHM) Boyutu

**Durum:** Docker default 64MB, GPU workload'lar icin yetersiz
**Oncelik:** Kritik
**Etki:** executor.go
**Proto degisikligi:** Hayir

### Problem

PyTorch `DataLoader(num_workers>0)` shared memory kullanir. 64MB ile:
- `RuntimeError: DataLoader worker is killed`
- `ERROR: Unexpected bus error encountered in worker`
- NCCL multi-GPU iletisiminde segfault

### Cozum

`createContainer()` icinde `HostConfig.ShmSize` ayarlanacak:

```go
ShmSize: shmBytes, // Container memory limitinin %25'i, minimum 256MB
```

### Hesaplama Formulu

```
shmSize = max(256MB, containerMemoryLimit * 0.25)
```

- Non-GPU workload'lar 256MB ile calisir.
- GPU workload'lar otomatik olarak yeterli SHM alir.
- SHM, container memory limitinden dusulur. 4GB memory limiti + 1GB SHM = uygulamaya 3GB kalir.
  Bu zaten Docker'in dahili davranisi, ekstra islem gerektirmez.

---

## 3. Signal Forwarding

**Durum:** `/bin/sh -c` SIGTERM'i child process'e iletmiyor
**Oncelik:** Yuksek
**Etki:** executor.go
**Proto degisikligi:** Hayir

### Problem

Suanki `requirements` akisi:

```
Cmd: ["/bin/sh", "-c", "pip install ... && python task.py"]
```

`sh` PID 1 olur. Docker SIGTERM gonderdiginde `sh` sinyali `python`'a iletmez.
Sonuc: graceful shutdown calismaz, checkpoint kaydetme firsati olmaz, 10 saniye sonra SIGKILL gelir.

### Cozum

`exec` komutu ile shell'i python process'i ile degistir:

```
Cmd: ["/bin/sh", "-c", "pip install ... && exec python task.py"]
```

`exec` shell process'ini python ile degistirir. Python PID 1 olur, SIGTERM dogrudan ona ulasir.

Requirements yoksa zaten `["python", "task.py"]` kullaniliyor, bu durumda sorun yok.

---

## 4. Artifact/Output Cikartma

**Durum:** Sadece stdout/stderr text donuyor, dosya cikartma yok
**Oncelik:** Yuksek
**Etki:** agent, orchestrator, proto

### Problem

Container icinde olusturulan dosyalar (egitilmis model, sonuc CSV, grafik PNG) kullaniciya ulasmiyor.
`JobResult.output_log` sadece stdout/stderr text iceriyor.

### Tasarim Karari: Presigned URL Yaklasimi

gRPC mesaj boyutu sinirli (default 4MB). Model dosyalari GB'larca olabilir.
`CopyFromContainer` + proto icinde `bytes` gondermek olceklenmez.

Bunun yerine: Orchestrator job gonderirken bir **presigned upload URL** verir.
Agent, container bittikten sonra artifact'lari bu URL'e yukler.
Orchestrator'da sadece metadata (dosya adi, boyut, URL) saklanir.

### Veri Akisi

```
Gateway                    Orchestrator                 Agent                     S3/MinIO
  │                            │                          │                         │
  │  POST /job                 │                          │                         │
  │  {script, image, ...}      │                          │                         │
  │ ─────────────────────────> │                          │                         │
  │                            │                          │                         │
  │                            │  Presigned upload URL    │                         │
  │                            │  olustur (job_id bazli)  │                         │
  │                            │ ──────────────────────> ╔═══════════════════════╗  │
  │                            │                         ║ JobRequest {          ║  │
  │                            │                         ║   ...mevcut alanlar   ║  │
  │                            │                         ║   artifact_upload_url ║  │
  │                            │                         ╚═══════════════════════╝  │
  │                            │                          │                         │
  │                            │                          │  Container calisir      │
  │                            │                          │  /app/output/ altina    │
  │                            │                          │  dosyalar yazar         │
  │                            │                          │                         │
  │                            │                          │  Container biter        │
  │                            │                          │                         │
  │                            │                          │  CopyFromContainer      │
  │                            │                          │  /app/output/ → tar     │
  │                            │                          │                         │
  │                            │                          │  PUT artifact_upload_url│
  │                            │                          │ ──────────────────────> │
  │                            │                          │                         │
  │                            │                          │  JobResult {            │
  │                            │  <─────────────────────  │    artifacts: [         │
  │                            │                          │      {name, size, url}  │
  │                            │                          │    ]                    │
  │                            │                          │  }                      │
  │                            │                          │                         │
  │  GET /job/:id/artifacts    │                          │                         │
  │ ─────────────────────────> │                          │                         │
  │                            │  Presigned download URL  │                         │
  │  <─────────────────────── │  olustur                 │                         │
  │                            │                          │                         │
  │  Download (presigned)      │                          │                         │
  │ ──────────────────────────────────────────────────────────────────────────────> │
```

### Proto Degisiklikleri

```protobuf
// JobRequest - yeni alan
message JobRequest {
  // ... mevcut alanlar (1-8) ...
  string artifact_upload_url = 11;    // Presigned PUT URL for artifact upload
  int64 max_artifact_bytes = 12;      // Maximum allowed artifact size (0 = no artifacts)
}

// JobResult - yeni alan
message ArtifactInfo {
  string filename = 1;       // "model.pt", "results/metrics.csv"
  int64 size_bytes = 2;      // Dosya boyutu
}

message JobResult {
  string job_id = 1;
  string status = 2;
  string output_log = 3;
  JobStats stats = 4;
  repeated ArtifactInfo artifacts = 5;  // Yuklenen artifact metadata'lari
}
```

### Degisiklik Plani

#### A. Proto (her iki repo'da ayni dosya)

`depin.proto`:
- `JobRequest`'e `artifact_upload_url` (field 11) ve `max_artifact_bytes` (field 12) ekle
- `ArtifactInfo` mesaji ekle
- `JobResult`'a `repeated ArtifactInfo artifacts = 5` ekle
- Her iki repoda proto'yu regenerate et

#### B. Orchestrator (`share-orchestrator`)

1. **S3/MinIO client entegrasyonu** (`internal/storage/s3.go` - yeni dosya):
   - S3-compatible client (MinIO SDK veya AWS SDK)
   - `GenerateUploadURL(jobID string) (string, error)` - presigned PUT URL olustur
   - `GenerateDownloadURL(jobID, filename string) (string, error)` - presigned GET URL olustur
   - Bucket: `vortix-artifacts`, path: `jobs/{job_id}/{filename}`

2. **HTTP server** (`internal/server/http_server.go`):
   - `handleSubmitJob`: Job olustururken presigned upload URL olustur, `JobRequest.artifact_upload_url`'e ekle
   - `GET /job/:id/artifacts` - yeni endpoint: Job'un artifact listesini presigned download URL'leri ile don

3. **gRPC server** (`internal/server/grpc_server.go`):
   - `processAgentEvent` JobResult case'inde: artifact metadata'yi DB'ye kaydet

4. **Database** (`internal/database/db.go`):
   - `Job` modeline `Artifacts json.RawMessage` (JSONB) alani ekle
   - Artifact metadata'lari: `[{filename, size_bytes, s3_key}]`

#### C. Agent (`share-agent`)

1. **Executor** (`internal/runtime/docker/executor.go`):
   - `ContainerConfig`'e `ArtifactUploadURL string` ve `MaxArtifactBytes int64` ekle
   - `RunContainer` donusunden once: `extractArtifacts()` cagir
   - `extractArtifacts()`: `CopyFromContainer(ctx, containerID, "/app/output/")` ile tar cikar
   - Tar'i parse et, boyut kontrolu yap (symlink atla, toplam boyut limiti)
   - Her dosyayi presigned URL'e HTTP PUT ile yukle
   - `ArtifactInfo` listesi don

2. **Job handler** (`cmd/agent/main.go`):
   - `createJobHandler`: `req.ArtifactUploadUrl` ve `req.MaxArtifactBytes`'i `ContainerConfig`'e aktar
   - `JobResult`'a artifact metadata ekle

### Guvenlik

- Presigned URL'ler 1 saat sureli (job timeout'undan uzun)
- Agent sadece PUT yapabilir (download yetkisi yok)
- Maksimum artifact boyutu orchestrator tarafindan belirlenir (`max_artifact_bytes`)
- Tar parse ederken: symlink atla, `..` iceren path'leri reddet, dosya sayisi limiti (1000)
- S3 bucket policy: public erisim yok, sadece presigned URL ile

### Ilk Surum Icin Basitlestirme

S3 entegrasyonu zaman alabilir. Ilk surum icin:
- Kucuk artifact'lar (<50MB) dogrudan proto icinde `bytes` olarak gonderilebilir
- gRPC max message size 50MB'a yukseltilir (agent + orchestrator)
- Buyuk dosya destegi Faz 2.1 olarak S3 ile gelir

Bu durumda proto:
```protobuf
message ArtifactInfo {
  string filename = 1;
  int64 size_bytes = 2;
  bytes content = 3;          // Ilk surum: dogrudan icerik (<50MB)
  // string download_url = 4; // Faz 2.1: S3 presigned URL
}
```

---

## 5. Multi-File Injection

**Durum:** Sadece tek `task.py` inject ediliyor
**Oncelik:** Yuksek
**Etki:** agent, orchestrator, proto

### Problem

Gercek projeler birden fazla dosyadan olusur:
- `train.py`, `model.py`, `utils.py`, `config.yaml`
- Kucuk veri dosyalari, pretrained weight'ler

Tek `script_content` string'i yetersiz.

### Proto Degisiklikleri

```protobuf
message JobFile {
  string path = 1;       // "model/network.py", "config.yaml", "data/labels.json"
  bytes content = 2;     // Dosya icerigi
}

message JobRequest {
  // ... mevcut alanlar (1-8) ...
  repeated JobFile files = 9;       // Ek dosyalar
  string entrypoint = 10;           // Calistirilacak dosya, default "task.py"
  // 11, 12 artifact icin ayrildi
}
```

### Geriye Uyumluluk Matrisi

| `script_content` | `files` | `entrypoint` | Davranis |
|---|---|---|---|
| dolu | bos | bos | Mevcut davranis: `script_content` → `task.py` |
| dolu | dolu | bos | `script_content` → `task.py` + `files` ek olarak inject |
| bos | dolu | dolu | `files` inject, `entrypoint` calistir |
| bos | dolu | bos | `files` inject, ilk dosya calistir |
| dolu | bos | dolu | `script_content` → `entrypoint` adi ile inject |

### Degisiklik Plani

#### A. Proto (her iki repo)

- `JobFile` mesaji ekle
- `JobRequest`'e `files` (field 9) ve `entrypoint` (field 10) ekle

#### B. Agent (`share-agent`)

1. **Executor** (`internal/runtime/docker/executor.go`):
   - `ContainerConfig`'e `Files map[string][]byte` ve `Entrypoint string` ekle
   - `injectScript` → `injectFiles` olarak genislet:
     ```go
     func (e *Executor) injectFiles(ctx, containerID, files map[string][]byte) error {
         var buf bytes.Buffer
         tw := tar.NewWriter(&buf)
         for path, content := range files {
             tw.WriteHeader(&tar.Header{Name: path, Mode: 0644, Size: int64(len(content))})
             tw.Write(content)
         }
         tw.Close()
         return e.client.CopyToContainer(ctx, containerID, "/app", &buf, ...)
     }
     ```
   - `createContainer`: `entrypoint` kullanarak Cmd olustur

2. **Job handler** (`cmd/agent/main.go`):
   - `createJobHandler`: `req.Files` ve `req.Entrypoint`'i `ContainerConfig`'e aktar
   - `files` map'ini olustur: once `script_content` → `task.py`, sonra `req.Files` ekle

#### C. Orchestrator (`share-orchestrator`)

1. **HTTP server** (`internal/server/http_server.go`):
   - `JobSubmission` struct'ina `Files` ve `Entrypoint` ekle:
     ```go
     type JobSubmission struct {
         // ... mevcut alanlar ...
         Files      []FileEntry `json:"files"`
         Entrypoint string      `json:"entrypoint"`
     }
     type FileEntry struct {
         Path    string `json:"path"`
         Content string `json:"content"` // base64 encoded
     }
     ```
   - `handleSubmitJob`: `files`'i proto `JobFile`'lara cevir, `entrypoint`'i aktar

### Boyut Limitleri

- Tek dosya: max 10MB
- Toplam injection: max 100MB
- Validation hem orchestrator hem agent tarafinda
- Buyuk datasetler inject edilmemeli, container icinden indirilmeli (network artik acik)

### `/app` Dizin Yapisi Ornegi

Kullanici sunlari gonderirse:
```json
{
  "script": "import model; model.train()",
  "files": [
    {"path": "model/__init__.py", "content": "..."},
    {"path": "model/network.py", "content": "..."},
    {"path": "config.yaml", "content": "..."}
  ],
  "entrypoint": "task.py"
}
```

Container icinde:
```
/app/
├── task.py              ← script_content
├── model/
│   ├── __init__.py      ← files[0]
│   └── network.py       ← files[1]
└── config.yaml          ← files[2]
```

---

## 6. NVIDIA Container Toolkit Kontrolu

**Durum:** Kontrol yok, GPU passthrough sessizce basarisiz olur
**Oncelik:** Orta
**Etki:** executor.go veya discovery
**Proto degisikligi:** Hayir

### Problem

GPU kesfedilse bile NVIDIA Container Toolkit kurulu degilse container'da GPU kullanilamaz.
Hata mesaji belirsiz olur.

### Cozum

Agent baslarken toolkit varligini kontrol et:

```go
func (e *Executor) checkNVIDIARuntime(ctx context.Context) bool {
    // Yontem: nvidia runtime ile test container calistir
    // docker run --rm --gpus all nvidia/cuda:12.3.1-base-ubuntu22.04 nvidia-smi
    // Basariliysa toolkit kurulu, degilse kullaniciya uyari ver.
}
```

### Kontrol Stratejisi

- Agent baslangicinda GPU kesfedildiyse toolkit kontrolu yap.
- Toolkit yoksa:
  - GPU passthrough devre disi birak.
  - Kullaniciya net bir uyari mesaji goster (kurulum talimatlari ile).
  - Node'u CPU-only olarak kaydet.
- Toolkit varsa: GPU passthrough aktif, node GPU-capable olarak kaydet.

---

## Uygulama Sirasi

```
Faz 1 - Agent-Only (proto degisikligi yok)              ✅ TAMAMLANDI
├── 1. GPU Passthrough                                    ✅
├── 2. SHM Boyutu                                        ✅
└── 3. Signal Forwarding                                  ✅

Faz 2 - Proto + Orchestrator (koordinasyon gerekli)      ✅ TAMAMLANDI
├── 4. Artifact Cikartma                                  ✅
└── 5. Multi-File Injection                               ✅

Faz 3 - Kalite
└── 6. NVIDIA Toolkit Kontrolu
```

### Faz 1 Uygulama Detaylari

Degisiklikler `executor.go` ve `main.go` icinde yapildi:

- **GPU Passthrough:** `Executor` struct'ina `gpuCount` alani eklendi. `NewExecutor(logger, gpuCount)` imzasi
  guncellendi. `gpuCount > 0` ise `DeviceRequests` ile tum GPU'lar container'a verilir, `NVIDIA_VISIBLE_DEVICES`
  ve `NVIDIA_DRIVER_CAPABILITIES` env var'lari set edilir. CPU-only node'larda bu kisim atlanir.

- **SHM Boyutu:** `ShmSize = max(256MB, memoryLimit * 25%)` formulu ile hesaplanir. PyTorch DataLoader
  ve NCCL multi-GPU iletisimi icin yeterli shared memory saglar.

- **Signal Forwarding:** Requirements'li komutlarda `exec` prefix eklendi:
  `pip install ... && exec python task.py`. Bu sayede python PID 1 olur ve Docker'dan gelen
  SIGTERM dogrudan python'a ulasir.

### Faz 2 Uygulama Plani

Proto degisikligi + agent + orchestrator koordineli calisma gerektirir.

**Uygulama sirasi:**

```
Adim 1: Proto guncelle (depin.proto)
├── JobFile, ArtifactInfo mesajlari ekle
├── JobRequest'e files(9), entrypoint(10), artifact_upload_url(11), max_artifact_bytes(12) ekle
├── JobResult'a artifacts(5) ekle
├── Agent repo'da proto regenerate et
└── Orchestrator repo'da proto regenerate et

Adim 2: Agent - Multi-file injection
├── ContainerConfig'e Files + Entrypoint ekle
├── injectScript → injectFiles genislet (map[string][]byte)
├── createContainer: entrypoint destegi
└── createJobHandler: req.Files → ContainerConfig.Files mapping

Adim 3: Agent - Artifact cikartma
├── extractArtifacts(): CopyFromContainer + tar parse + guvenlik kontrolleri
├── RunContainer: container bittikten sonra artifact cikar
├── ArtifactInfo listesini don
└── createJobHandler: artifact info'yu JobResult'a ekle

Adim 4: Orchestrator - Multi-file destegi
├── JobSubmission struct'ina Files + Entrypoint ekle
├── handleSubmitJob: files → proto JobFile donusumu
└── base64 decode (HTTP JSON'dan gelen files)

Adim 5: Orchestrator - Artifact destegi (ilk surum: proto icinde bytes)
├── gRPC max message size 50MB'a yukselt
├── processAgentEvent: artifact metadata'yi DB'ye kaydet
├── Job modeline Artifacts JSONB alani ekle
├── GET /job/:id/artifacts endpoint'i ekle
└── Artifact content'i response'a dahil et

Adim 6 (ileride): S3 entegrasyonu
├── internal/storage/s3.go olustur
├── Presigned URL olusturma
├── Agent'da HTTP PUT ile upload
└── Gateway'de presigned download URL
```

**Dosya bazinda degisiklik listesi:**

| Dosya | Degisiklik |
|---|---|
| `proto/depin.proto` (her iki repo) | JobFile, ArtifactInfo, JobRequest fields 9-12, JobResult field 5 |
| `share-agent/internal/runtime/docker/executor.go` | ContainerConfig, injectFiles, extractArtifacts, RunContainer |
| `share-agent/cmd/agent/main.go` | createJobHandler: files + artifact mapping |
| `share-orchestrator/internal/server/http_server.go` | JobSubmission struct, handleSubmitJob, artifacts endpoint |
| `share-orchestrator/internal/server/grpc_server.go` | processAgentEvent: artifact DB kaydi |
| `share-orchestrator/internal/database/db.go` | Job model: Artifacts JSONB |

Faz 3 bagimsiz, herhangi bir zamanda uygulanabilir.
