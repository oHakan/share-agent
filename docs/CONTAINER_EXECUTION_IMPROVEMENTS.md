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
**Etki:** executor.go, proto, orchestrator
**Proto degisikligi:** Evet

### Problem

Container icinde olusturulan dosyalar (egitilmis model, sonuc CSV, grafik PNG) kullaniciya ulasmiyor.
`JobResult.output_log` sadece text iceriyor.

### Cozum

Container bittikten sonra `/app/output/` dizininden `CopyFromContainer` ile dosyalari cikar:

```go
reader, _, err := e.client.CopyFromContainer(ctx, containerID, "/app/output/")
// reader bir tar archive, parse et
```

### Akis

```
1. Container calisir, kullanici dosyalari /app/output/ altina yazar
2. Container biter
3. Agent CopyFromContainer ile /app/output/ tar'ini cikarir
4. Dosyalar boyut kontrolunden gecirilir (max limit)
5. Artifact verileri JobResult ile orchestrator'a gonderilir
6. Orchestrator S3/object storage'a yukler
7. Kullanici dashboard'dan veya API'den indirir
```

### Proto Degisikligi

```protobuf
message JobArtifact {
  string filename = 1;      // "model.pt", "results/output.csv"
  bytes content = 2;        // Dosya icerigi
  int64 size_bytes = 3;     // Boyut
}

message JobResult {
  string job_id = 1;
  string status = 2;
  string output_log = 3;
  JobStats stats = 4;
  repeated JobArtifact artifacts = 5;  // Yeni
}
```

### Guvenlik

- Maksimum toplam artifact boyutu sinirlanmali (ornegin 1GB).
- Symlink takibi devre disi birakilmali (container disina cikmak icin kullanilabilir).
- Dosya sayisi sinirlanmali (tar bomb korunmasi).

### Alternatif: Buyuk Dosyalar Icin

gRPC mesaj boyutu sinirli (default 4MB). Buyuk artifactlar icin:

- **Opsiyon A:** Agent artifact'i S3'e yukler, JobResult'ta sadece URL doner. Presigned URL ile guvenli erisim.
- **Opsiyon B:** gRPC stream uzerinden chunk'lar halinde gonderim.
- **Opsiyon C:** Orchestrator tarafindan saglanan presigned upload URL'i container'a env var olarak verilir, container kendi yukler.

Baslangic icin kuucuk artifact'lar (<50MB) CopyFromContainer + proto ile, buyuk dosyalar icin ileride S3 entegrasyonu oneririr.

---

## 5. Multi-File Injection

**Durum:** Sadece tek `task.py` inject ediliyor
**Oncelik:** Orta
**Etki:** executor.go, proto, orchestrator
**Proto degisikligi:** Evet

### Problem

Gercek projeler birden fazla dosyadan olusur:
- `train.py`, `model.py`, `utils.py`
- `config.yaml`, `requirements.txt`
- Kucuk veri dosyalari

### Cozum

Mevcut tar injection yaklasimini genislet. Tek dosya yerine dosya map'i kabul et:

```go
func (e *Executor) injectFiles(ctx context.Context, containerID string, files map[string][]byte) error {
    var buf bytes.Buffer
    tw := tar.NewWriter(&buf)
    for name, content := range files {
        hdr := &tar.Header{
            Name: name,
            Mode: 0644,
            Size: int64(len(content)),
        }
        tw.WriteHeader(hdr)
        tw.Write(content)
    }
    tw.Close()
    return e.client.CopyToContainer(ctx, containerID, "/app", &buf, container.CopyToContainerOptions{})
}
```

### Proto Degisikligi

```protobuf
message JobFile {
  string path = 1;        // "model/network.py", "config.yaml"
  bytes content = 2;      // Dosya icerigi
}

message JobRequest {
  // ... mevcut alanlar ...
  string script_content = 4;        // Ana script (geriye uyumluluk)
  repeated JobFile files = 9;       // Ek dosyalar (yeni)
  string entrypoint = 10;           // Ana calistirma dosyasi, default "task.py" (yeni)
}
```

### Geriye Uyumluluk

- `script_content` dolu, `files` bos: mevcut davranis, `task.py` olarak inject et.
- `files` dolu: tum dosyalari inject et, `entrypoint` ile belirtilen dosyayi calistir.
- `script_content` + `files` birlikte: `script_content` `task.py` olarak eklenir, `files` ek dosyalar olarak eklenir.

### Boyut Limiti

Toplam injection boyutu sinirlanmali (ornegin 100MB). Buyuk datasetler container icinden indirilmeli, inject edilmemeli.

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

Faz 2 - Proto + Orchestrator (koordinasyon gerekli)
├── 4. Artifact Cikartma
└── 5. Multi-File Injection

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

Faz 2 proto degisikligi ve orchestrator tarafinda calisma gerektirir.
Faz 3 bagimsiz, herhangi bir zamanda uygulanabilir.
