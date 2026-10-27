package monitoring

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func overviewSample(sequence uint64, observed time.Time, cpu float64, rx, swapUsed string) contracts.NodeMetricSample {
	return contracts.NodeMetricSample{ServerID: "server-local-0001", CollectorEpoch: "epoch-local-0001", Sequence: sequence, ObservedAt: observed,
		Values:   map[string]float64{"cpu.utilization": cpu, "cpu.cores": 4, "memory.used_percent": 40, "disk.root.used_percent": 12.3456},
		Counters: map[string]string{"net.billing.rx_bytes": rx, "net.billing.tx_bytes": rx, "memory.swap_used_bytes": swapUsed, "memory.swap_total_bytes": "1000", "memory.total_bytes": "16000000000", "memory.used_bytes": "6400000000", "disk.root.total_bytes": "250000000000", "disk.root.used_bytes": "30864000000"},
		Validity: map[string]string{"cpu.utilization": "valid", "cpu.cores": "valid", "memory.used_percent": "valid", "disk.root.used_percent": "valid", "net.billing.rx_bytes": "valid", "net.billing.tx_bytes": "valid", "memory.swap_used_bytes": "valid", "memory.swap_total_bytes": "valid", "memory.total_bytes": "valid", "memory.used_bytes": "valid", "disk.root.total_bytes": "valid", "disk.root.used_bytes": "valid"},
		Units:    map[string]string{"cpu.utilization": "percent", "cpu.cores": "count", "memory.used_percent": "percent", "disk.root.used_percent": "percent", "net.billing.rx_bytes": "bytes", "net.billing.tx_bytes": "bytes", "memory.swap_used_bytes": "bytes", "memory.swap_total_bytes": "bytes", "memory.total_bytes": "bytes", "memory.used_bytes": "bytes", "disk.root.total_bytes": "bytes", "disk.root.used_bytes": "bytes"}}
}

func TestOverviewComputesCurrentValuesRatesAndSwap(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertServer(ctx, testServer()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	samples := []contracts.NodeMetricSample{
		overviewSample(0, now.Add(-30*time.Second), 10, "1000", "100"),
		overviewSample(1, now.Add(-15*time.Second), 20, "16000", "250"),
	}
	if _, err := store.IngestNodeBatch(ctx, testServer().ID, samples, nil, now); err != nil {
		t.Fatal(err)
	}
	overview, err := store.QueryOverview(ctx, now, nil)
	if err != nil || len(overview.Items) != 1 {
		t.Fatalf("overview = %+v, %v", overview, err)
	}
	item := overview.Items[0]
	if *item.CPU != 20 || *item.Memory != 40 || *item.Disk != 12.35 || *item.Swap != 25 {
		t.Fatalf("current values cpu=%v mem=%v disk=%v swap=%v", *item.CPU, *item.Memory, *item.Disk, *item.Swap)
	}
	if item.CPUCores == nil || *item.CPUCores != 4 {
		t.Fatalf("cpu cores = %v, expected 4", item.CPUCores)
	}
	if item.DiskTotal == nil || *item.DiskTotal != "250000000000" || item.DiskUsed == nil || *item.DiskUsed != "30864000000" {
		t.Fatalf("disk capacity total=%v used=%v", item.DiskTotal, item.DiskUsed)
	}
	if item.MemTotal == nil || *item.MemTotal != "16000000000" {
		t.Fatalf("memory total = %v", item.MemTotal)
	}
	if item.RX == nil || *item.RX != 1000 { // 15000 bytes over 15 s
		t.Fatalf("rx rate = %v", item.RX)
	}
	if len(item.Series.T) != 2 || item.Series.RX[0] != nil || *item.Series.CPU[0] != 10 {
		t.Fatalf("series = %+v", item.Series)
	}
	if scoped, _ := store.QueryOverview(ctx, now, []string{"server-other-0000001"}); len(scoped.Items) != 0 {
		t.Fatal("scope leaked another server's data")
	}

	api := &API{Store: store, Token: "token"}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil)
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Accept-Encoding", "gzip")
	recorder := httptest.NewRecorder()
	api.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("status %d encoding %q", recorder.Code, recorder.Header().Get("Content-Encoding"))
	}
	reader, err := gzip.NewReader(recorder.Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Overview
	if err := json.NewDecoder(reader).Decode(&decoded); err != nil || len(decoded.Items) != 1 {
		t.Fatalf("decoded = %+v, %v", decoded, err)
	}
}

func TestServerTags(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertServer(ctx, testServer()); err != nil {
		t.Fatal(err)
	}
	tags, err := store.SetServerTags(ctx, testServer().ID, []string{" iran ", "Exit", "exit", "", "ایران"})
	if err != nil || len(tags) != 3 {
		t.Fatalf("tags = %q, %v", tags, err)
	}
	server, _, _ := store.GetServer(ctx, testServer().ID)
	if len(server.Tags) != 3 {
		t.Fatalf("server tags = %q", server.Tags)
	}
	page, _ := store.QueryServerPage(ctx, 10, "")
	if len(page.Items[0].Tags) != 3 {
		t.Fatalf("listed tags = %q", page.Items[0].Tags)
	}
	if _, err := store.SetServerTags(ctx, testServer().ID, []string{"bad\x01tag"}); err == nil {
		t.Fatal("control character accepted")
	}
	many := make([]string, 17)
	for i := range many {
		many[i] = string(rune('a' + i))
	}
	if _, err := store.SetServerTags(ctx, testServer().ID, many); err == nil {
		t.Fatal("17 tags accepted")
	}
	if _, err := store.SetServerTags(ctx, "server-missing-00001", []string{"x"}); err == nil {
		t.Fatal("tags set on a missing server")
	}
	if _, err := store.SetServerTags(ctx, testServer().ID, nil); err != nil {
		t.Fatal(err)
	}
	if server, _, _ = store.GetServer(ctx, testServer().ID); len(server.Tags) != 0 {
		t.Fatalf("tags not cleared: %q", server.Tags)
	}
}

func TestNodeAddressesAreNormalizedAndMerged(t *testing.T) {
	got := NormalizeNodeAddresses([]string{"10.0.0.2", "127.0.0.1", "fe80::1", "2001:db8::5", "198.51.100.7", "::ffff:198.51.100.7", "junk", "203.0.113.9"})
	if strings.Join(got, ",") != "198.51.100.7,203.0.113.9,2001:db8::5,10.0.0.2" {
		t.Fatalf("normalized = %v", got)
	}
	merged := serverAddresses("203.0.113.9", "192.0.2.1", "198.51.100.7\x1f203.0.113.9")
	if strings.Join(merged, ",") != "203.0.113.9,192.0.2.1,198.51.100.7" {
		t.Fatalf("merged = %v", merged)
	}
	if serverAddresses("", "", "") != nil {
		t.Fatal("no addresses should stay empty")
	}
}
