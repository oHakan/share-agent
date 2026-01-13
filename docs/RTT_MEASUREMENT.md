# RTT (Round-Trip Time) Measurement

## Overview

Vortix Agent artık orchestrator ile arasındaki ağ gecikmesini (ping/latency) ölçebiliyor. Bu özellik, node'ların bağlantı kalitesini izlemek ve sorunları tespit etmek için kullanılır.

## Nasıl Çalışır?

### 1. Proto Değişiklikleri

#### Heartbeat Mesajına Timestamp Eklendi
```protobuf
message Heartbeat {
  // ... mevcut alanlar ...
  int64 sent_timestamp_ms = 11; // Heartbeat gönderilme zamanı (Unix milliseconds)
}
```

#### HeartbeatAck Mesajı Eklendi
```protobuf
message HeartbeatAck {
  int64 received_timestamp_ms = 1; // Orchestrator'ın heartbeat'i aldığı zaman
  int64 sent_timestamp_ms = 2;     // Agent'ın gönderdiği timestamp (echo)
}
```

#### ServerEvent Güncellendi
```protobuf
message ServerEvent {
  oneof event {
    JobRequest job_request = 1;
    ResourceLimitsUpdate resource_limits_update = 2;
    HeartbeatAck heartbeat_ack = 3; // YENİ: Heartbeat acknowledgment
  }
}
```

### 2. Agent Tarafı Implementasyon

#### Heartbeat Gönderimi
```go
// Agent her heartbeat gönderirken:
1. Mevcut zamanı Unix milliseconds olarak alır (time.Now().UnixMilli())
2. Heartbeat'e sent_timestamp_ms alanını set eder
3. Bu timestamp'i local bir map'te saklar (RTT hesabı için)
4. Heartbeat'i orchestrator'a gönderir
```

#### HeartbeatAck Alımı
```go
// Orchestrator'dan HeartbeatAck geldiğinde:
1. sent_timestamp_ms ile gönderilme zamanını bulur
2. RTT = şimdi - gönderilme_zamanı (RTT hesabı)
3. Son 10 RTT örneğinden ortalama hesaplar
4. ~1 dakikada bir kullanıcı dostu log gösterir
```

### 3. RTT Ölçüm Özellikleri

- **Heartbeat Interval**: 3 saniye (yüksek hassasiyet)
- **RTT Samples**: Son 10 ölçüm saklanır
- **Average Calculation**: Sliding window average
- **Log Frequency**: Her 20 ölçümde bir (~60 saniye)
- **Precision**: Millisecond düzeyinde

### 4. Kullanıcı Arayüzü

Agent çalışırken yaklaşık her dakikada bir RTT bilgisi gösterir:

```
[ℹ]  Network latency | RTT: 45ms (avg: 48ms)
```

- **RTT**: Son ölçülen değer
- **avg**: Son 10 ölçümün ortalaması

## Orchestrator Implementasyonu (Gerekli)

Orchestrator tarafında aşağıdaki değişiklikler yapılmalı:

### 1. Proto Güncelleme

```bash
# Proto dosyasını güncelledikten sonra:
protoc --go_out=. --go-grpc_out=. proto/depin.proto
```

### 2. StreamEvents Handler Güncelleme

```go
// Heartbeat aldığınızda HeartbeatAck gönderin:
func (s *Server) StreamEvents(stream pb.NodeService_StreamEventsServer) error {
    for {
        agentEvent, err := stream.Recv()
        if err != nil {
            return err
        }

        switch event := agentEvent.GetEvent().(type) {
        case *pb.AgentEvent_Heartbeat:
            heartbeat := event.Heartbeat
            
            // Store telemetry data...
            
            // Send HeartbeatAck
            ack := &pb.HeartbeatAck{
                ReceivedTimestampMs: time.Now().UnixMilli(),
                SentTimestampMs:     heartbeat.SentTimestampMs, // Echo back
            }
            
            serverEvent := &pb.ServerEvent{
                Event: &pb.ServerEvent_HeartbeatAck{
                    HeartbeatAck: ack,
                },
            }
            
            if err := stream.Send(serverEvent); err != nil {
                return err
            }
            
        // ... diğer event handler'lar
        }
    }
}
```

### 3. Optimizasyon (Opsiyonel)

Her heartbeat'e cevap vermek yerine, belirli aralıklarla ack gönderebilirsiniz:

```go
// Her 10 heartbeat'te bir ack gönder
heartbeatCounter := 0
if heartbeatCounter%10 == 0 {
    // Send HeartbeatAck
}
heartbeatCounter++
```

## Metrikler ve Monitoring

### Agent Tarafı Metrikler

RTT bilgileri agent loglarında görülebilir:

```bash
# Debug modda tüm RTT ölçümleri
{"level":"debug","msg":"Heartbeat RTT measured","rtt":"45ms","avg_rtt":"48ms"}

# Production modda sadece özet (her ~60 saniyede)
[ℹ]  Network latency | RTT: 45ms (avg: 48ms)
```

### Orchestrator Tarafı Kullanım

RTT bilgileri:
1. **Dashboard**: Node detay sayfasında gösterilebilir
2. **Alerting**: Yüksek latency alarmları
3. **Node Scoring**: RTT'ye göre job assignment önceliği
4. **Network Issues**: Bağlantı sorunlarını tespit etme

## Güvenlik ve Performans

### Performans İyileştirmeleri

1. **Throttling**: Her heartbeat'e cevap gereksiz. Her 3-5 heartbeat'te bir ack yeterli.
2. **Cleanup**: Eski timestamp'ler otomatik temizlenir (son 20 saklanır)
3. **Memory**: Minimal memory footprint (~1KB per node)

### Güvenlik Notları

- RTT ölçümü gRPC stream üzerinden yapılır (mevcut güvenlik korunur)
- Timestamp'ler sadece RTT hesabı için kullanılır
- Hiçbir hassas bilgi içermez

## Test

### Agent Test

```bash
# Development modda çalıştır
cd /Users/hakan/Desktop/HY/vortix.cloud/share-agent
DEV_MODE=true go run ./cmd/agent --owner=test_user

# RTT loglarını takip et
# Her ~60 saniyede bir göreceksiniz:
# [ℹ]  Network latency | RTT: XXms (avg: XXms)
```

### Orchestrator Test

```bash
# HeartbeatAck implementasyonunu ekledikten sonra:
# Agent'ları bağlayın ve RTT loglarını gözlemleyin
```

## Sorun Giderme

### RTT Gösterilmiyor

**Sebep**: Orchestrator HeartbeatAck göndermiyor olabilir.

**Çözüm**: 
1. Orchestrator loglarını kontrol edin
2. StreamEvents handler'ında HeartbeatAck implementasyonu olduğundan emin olun
3. Proto dosyalarının güncel olduğunu doğrulayın

### RTT Çok Yüksek

**Sebep**: Ağ gecikmesi, orchestrator yük, firewall

**Çözüm**:
1. Ping testi: `ping trolley.proxy.rlwy.net`
2. Traceroute: `traceroute trolley.proxy.rlwy.net`
3. Orchestrator CPU/memory kullanımını kontrol edin

### RTT Tutarsız

**Sebep**: Ağ dalgalanması, Wi-Fi bağlantısı

**Öneri**:
- Kablolu bağlantı kullanın
- Bandwidth'i kontrol edin
- Network Quality of Service (QoS) ayarları

## İleride Eklenebilecekler

1. **Prometheus Metrics**: RTT metriğini Prometheus'a export et
2. **Histogram**: RTT dağılımını göster (p50, p95, p99)
3. **Alerting**: Belirli threshold'ları aşan RTT'lerde alarm
4. **Dashboard Widget**: Grafana/UI'da real-time RTT grafiği
5. **Jitter Measurement**: RTT değişkenliğini ölç
6. **Packet Loss Detection**: Timeout olan heartbeat'leri say

## Örnek Çıktılar

### Agent Startup
```
[✓]  Vortix Agent v1.0.4 (Production Build)
[➜]  Establishing secure tunnel
[✓]  Secure tunnel established
[ℹ]  Node "Vortix-Agent" is now ONLINE
```

### Running
```
[ℹ]  Network latency | RTT: 42ms (avg: 45ms)
[➜]  Job job_123 received | image=python:3.11-slim timeout=60s
[ℹ]  Job job_123 started | image=python:3.11-slim cpu=2.00 mem=2048MB vol=10GB
[✓]  Job job_123 completed | duration=5.2s cpu_peak=45%
[ℹ]  Network latency | RTT: 44ms (avg: 45ms)
```

## Kaynaklar

- **Proto Definition**: [proto/depin.proto](../proto/depin.proto)
- **Client Implementation**: [internal/client/grpc_client.go](../internal/client/grpc_client.go)
- **Main Integration**: [cmd/agent/main.go](../cmd/agent/main.go)
