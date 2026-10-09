package monitoring

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Payesh writes some log entries itself (package installs). They must land
// in a listed source and be readable without a file or journald unit behind it.
func TestStoredLogSourceIsListedAndQueryableWithoutJournal(t *testing.T) {
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
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	if err := store.AppendStoredLog(ctx, server.ID, PackageLogSourceID, "INFO", "Module port-traffic v0.1.0 successfully installed", at); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendStoredLog(ctx, server.ID, "not-stored", "INFO", "x", at); err == nil {
		t.Fatal("a file or journal source accepted a stored entry")
	}
	sources, err := store.ListLogSources(ctx, server.ID, 10)
	if err != nil || len(sources) != 1 || sources[0].ID != PackageLogSourceID || sources[0].Label == "" {
		t.Fatalf("stored source not listed: %#v %v", sources, err)
	}

	api, err := NewAPI(store, "local-log-token")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/servers/"+string(server.ID)+"/logs?source="+PackageLogSourceID+"&from="+at.Add(-time.Hour).Format(time.RFC3339)+"&to="+time.Now().UTC().Add(time.Minute).Format(time.RFC3339), nil)
	request.Header.Set("Authorization", "Bearer local-log-token")
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("stored source query status=%d body=%s", response.Code, response.Body.String())
	}
	var page LogPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || !strings.Contains(page.Entries[0].Text, "port-traffic") {
		t.Fatalf("stored entry missing: %#v", page.Entries)
	}
}
