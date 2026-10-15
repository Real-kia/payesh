package transport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Real-kia/payesh/internal/contracts"
)

func TestEndpointDurabilitySyncOrderingAndFailure(t *testing.T) {
	for _, fail := range []string{"", "file", "directory"} {
		t.Run(fail, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "identity.transport.json")
			a := AgentClient{IdentityPath: filepath.Join(dir, "identity"), configuredURL: "wss://hub.example.test/node/v1"}
			events := []string{}
			injected := errors.New("sync refused")
			a.endpointSync = func(file *os.File) error {
				info, err := file.Stat()
				if err != nil {
					return err
				}
				stage := "file"
				if info.IsDir() {
					stage = "directory"
				}
				events = append(events, stage)
				_, readErr := os.ReadFile(path)
				if stage == "file" && !errors.Is(readErr, os.ErrNotExist) {
					t.Error("endpoint published before file sync")
				}
				if stage == "directory" && readErr != nil && len(events) == 2 {
					t.Error("parent sync preceded rename")
				}
				if fail == stage {
					return injected
				}
				return file.Sync()
			}
			err := a.SaveEndpoint("wss://hub.example.test:9797/node/v1", a.configuredURL)
			if fail != "" && !errors.Is(err, injected) {
				t.Fatalf("sync failure not refused: %v", err)
			}
			if fail == "" && err != nil {
				t.Fatal(err)
			}
			want := []string{"file", "directory"}
			if fail == "directory" {
				want = append(want, "directory")
			}
			if fail == "file" {
				want = want[:1]
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("sync order %v, want %v", events, want)
			}
			matches, _ := filepath.Glob(filepath.Join(dir, ".transport-*"))
			if len(matches) != 0 {
				t.Fatalf("temporary files remain: %v", matches)
			}
		})
	}
}

func TestAgentAcceptedEndpointRefusesFallbackRemovalOnSyncFailure(t *testing.T) {
	dir := t.TempDir()
	a := AgentClient{IdentityPath: filepath.Join(dir, "identity"), configuredURL: "wss://hub.example.test/node/v1", URL: "wss://hub.example.test:9797/node/v1", previousURL: "wss://hub.example.test/node/v1"}
	if err := a.SaveEndpoint(a.URL, a.previousURL); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("directory sync refused")
	failed := false
	a.endpointSync = func(file *os.File) error {
		info, _ := file.Stat()
		if info.IsDir() && !failed {
			failed = true
			return injected
		}
		return file.Sync()
	}
	err := a.handleIncoming(context.Background(), nil, inboundEnvelope{envelope: contracts.Envelope{Message: "hello_accepted"}})
	if !errors.Is(err, injected) {
		t.Fatalf("fallback removal acknowledged on unsynced write: %v", err)
	}
	if a.previousURL == "" {
		t.Fatal("fallback cleared despite sync failure")
	}
	b := AgentClient{IdentityPath: a.IdentityPath, URL: a.configuredURL}
	if err := b.LoadEndpoint(); err != nil || b.previousURL != a.previousURL {
		t.Fatalf("restored endpoint lost fallback: %q %v", b.previousURL, err)
	}
}
