//go:build linux

package monitoring

import (
	"context"
	"testing"
	"time"
)

func TestLinuxCollectorReadsKernelSources(t *testing.T) {
	collector := NewCollector("/", DefaultServerID, DefaultEpoch)
	sample, err := collector.Collect(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := sample.Validate(); err != nil {
		t.Fatal(err)
	}
	if sample.ObservedAt.IsZero() || sample.Values == nil || sample.Counters == nil || sample.Validity == nil {
		t.Fatalf("linux collector returned an incomplete sample: %#v", sample)
	}
	if sample.Validity["disk.root.free_bytes"] != "valid" {
		t.Fatalf("root filesystem was not collected: %#v", sample.Validity)
	}
}
