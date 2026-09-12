package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCollectSnapshotIncludesCurrentProcess(t *testing.T) {
	s, err := collectSnapshot(os.Getpid())
	if err != nil {
		t.Skipf("process sampler unavailable: %v", err)
	}
	if s.ProcessCount < 1 || s.Metric.RSSBytes == 0 {
		t.Fatalf("snapshot = %#v", s)
	}
}

func TestWriteReportJSONAndTSV(t *testing.T) {
	r := report{Schema: schema, PID: 42, MeasurementMode: "proc", ProcessScope: "root-and-descendants", Command: []string{"echo", "a b"}}
	for _, format := range []string{"json", "tsv"} {
		path := filepath.Join(t.TempDir(), "report."+format)
		if err := writeReport(path, format, r); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) == 0 {
			t.Fatalf("empty %s report", format)
		}
		if format == "json" {
			var got report
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if got.Schema != schema {
				t.Fatalf("schema = %q", got.Schema)
			}
		}
	}
}
