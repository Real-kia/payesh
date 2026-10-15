package monitoring

import (
	"context"
	"testing"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestHelloKeepsNodeVersionCapability(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(ctx, ":memory:", StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := testServer()
	if err := store.UpsertServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	hello := contracts.Hello{Version: "0.7.1", ProtocolMin: "v1", ProtocolMax: "v1", Architecture: "amd64", Platform: "linux", Capabilities: []string{"metrics", "node-version:0.7.0"}}
	if err := hello.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateServerHello(ctx, server.ID, hello, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.GetServer(ctx, server.ID)
	if err != nil || !ok {
		t.Fatalf("server=%+v ok=%v err=%v", got, ok, err)
	}
	found := false
	for _, capability := range got.Capabilities {
		found = found || capability == "node-version:0.7.0"
	}
	if !found {
		t.Fatalf("capabilities=%v", got.Capabilities)
	}
}
