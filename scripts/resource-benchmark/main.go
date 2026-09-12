// Command resource-benchmark measures a bounded process and its descendants.
//
// Linux uses /proc for cumulative CPU, RSS, and block I/O counters. Other
// Unix systems use ps for CPU/RSS sampling and report I/O as unavailable. The
// command never walks outside the selected process tree and only terminates a
// process group that it started itself.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const schema = "payesh.resource_benchmark.v1"

type processMetric struct {
	CPUSeconds  float64
	RSSBytes    uint64
	IORead      uint64
	IOWrite     uint64
	IOSupported bool
}

type report struct {
	Schema                   string   `json:"schema_version"`
	Label                    string   `json:"label,omitempty"`
	Command                  []string `json:"command,omitempty"`
	PID                      int      `json:"root_pid"`
	MeasurementMode          string   `json:"measurement_mode"`
	ProcessScope             string   `json:"process_scope"`
	RequestedDurationSeconds float64  `json:"requested_duration_seconds"`
	ActualDurationSeconds    float64  `json:"actual_duration_seconds"`
	IntervalSeconds          float64  `json:"interval_seconds"`
	Samples                  int      `json:"samples"`
	ProcessCountMax          int      `json:"process_count_max"`
	CPUSeconds               float64  `json:"cpu_seconds"`
	CPUPercentAverage        float64  `json:"cpu_percent_average"`
	RSSAverageBytes          uint64   `json:"rss_average_bytes"`
	RSSMaxBytes              uint64   `json:"rss_max_bytes"`
	RSSFinalBytes            uint64   `json:"rss_final_bytes"`
	IOReadBytes              uint64   `json:"io_read_bytes"`
	IOWriteBytes             uint64   `json:"io_write_bytes"`
	IOSupported              bool     `json:"io_supported"`
	Exited                   bool     `json:"exited"`
	ExitCode                 *int     `json:"exit_code,omitempty"`
	TimedOut                 bool     `json:"timed_out"`
	SamplerErrors            int      `json:"sampler_errors"`
	Notes                    []string `json:"notes,omitempty"`
	StartedAt                string   `json:"started_at"`
	FinishedAt               string   `json:"finished_at"`
}

type snapshot struct {
	Metric       processMetric
	ProcessCount int
}

func main() {
	flags := flag.NewFlagSet("resource-benchmark", flag.ExitOnError)
	duration := flags.Duration("duration", 30*time.Second, "maximum measurement duration (10ms..24h)")
	interval := flags.Duration("interval", time.Second, "sampling interval (10ms..1h)")
	warmup := flags.Duration("warmup", 0, "time to let a started command warm up before sampling")
	pid := flags.Int("pid", 0, "existing root PID to observe; do not terminate it")
	label := flags.String("label", "", "optional component label")
	format := flags.String("format", "json", "output format: json or tsv")
	out := flags.String("out", "-", "output path, or - for stdout")
	flags.Parse(os.Args[1:])
	if *duration < 10*time.Millisecond || *duration > 24*time.Hour {
		fatal("duration must be between 10ms and 24h")
	}
	if *interval < 10*time.Millisecond || *interval > time.Hour {
		fatal("interval must be between 10ms and 1h")
	}
	if *warmup < 0 || *warmup >= *duration {
		fatal("warmup must be non-negative and shorter than duration")
	}
	if *format != "json" && *format != "tsv" {
		fatal("format must be json or tsv")
	}
	if *pid < 0 {
		fatal("pid must be positive")
	}
	command := flags.Args()
	if *pid != 0 && len(command) != 0 {
		fatal("-pid and a command are mutually exclusive")
	}
	if *pid == 0 && len(command) == 0 {
		fatal("provide -pid or a command after --")
	}

	started := time.Now().UTC()
	rootPID := *pid
	var cmd *exec.Cmd
	var wait <-chan error
	startedByUs := false
	if rootPID == 0 {
		var err error
		cmd, err = startCommand(command)
		if err != nil {
			fatal("start command: %v", err)
		}
		rootPID = cmd.Process.Pid
		startedByUs = true
		ch := make(chan error, 1)
		go func() { ch <- cmd.Wait() }()
		wait = ch
	}

	if *warmup > 0 {
		time.Sleep(*warmup)
	}
	result := measure(rootPID, command, *label, *duration-*warmup, *interval, started, startedByUs, wait)
	if err := writeReport(*out, *format, result); err != nil {
		fatal("write report: %v", err)
	}
	if result.TimedOut {
		os.Exit(124)
	}
}

func measure(pid int, command []string, label string, duration, interval time.Duration, started time.Time, startedByUs bool, wait <-chan error) report {
	r := report{
		Schema: schema, Label: label, Command: command, PID: pid,
		MeasurementMode: measurementMode(), ProcessScope: "root-and-descendants",
		RequestedDurationSeconds: duration.Seconds(), IntervalSeconds: interval.Seconds(),
		StartedAt: started.Format(time.RFC3339Nano),
	}
	begin := time.Now()
	deadline := begin.Add(duration)
	var first, last snapshot
	var rssTotal uint64
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	sample := func() bool {
		s, err := collectSnapshot(pid)
		if err != nil {
			r.SamplerErrors++
			return processExists(pid)
		}
		if r.Samples == 0 {
			first = s
		}
		last = s
		r.Samples++
		if s.ProcessCount > r.ProcessCountMax {
			r.ProcessCountMax = s.ProcessCount
		}
		rssTotal += s.Metric.RSSBytes
		if s.Metric.RSSBytes > r.RSSMaxBytes {
			r.RSSMaxBytes = s.Metric.RSSBytes
		}
		return true
	}

	// Capture a baseline immediately. It makes short-lived commands and zero-
	// interval reports useful while still preserving the bounded deadline.
	alive := sample()
	for alive && time.Now().Before(deadline) {
		select {
		case err := <-wait:
			r.Exited = true
			setExitCode(&r, err)
			wait = nil
			alive = false
		case <-ticker.C:
			alive = sample()
		case <-time.After(time.Until(deadline)):
			alive = false
		}
	}
	if startedByUs && !r.Exited {
		r.TimedOut = true
		terminateCommand(pid)
		if wait != nil {
			select {
			case err := <-wait:
				r.Exited = true
				setExitCode(&r, err)
			case <-time.After(2 * time.Second):
				r.Notes = append(r.Notes, "process group did not exit within 2s after timeout")
			}
		}
	} else if !startedByUs && !processExists(pid) {
		r.Exited = true
	}
	// A final sample captures counters after a graceful exit, if still visible.
	_ = sample()
	r.ActualDurationSeconds = time.Since(begin).Seconds()
	if r.Samples > 0 {
		r.RSSAverageBytes = rssTotal / uint64(r.Samples)
		r.RSSFinalBytes = last.Metric.RSSBytes
		r.CPUSeconds = last.Metric.CPUSeconds - first.Metric.CPUSeconds
		if r.CPUSeconds < 0 {
			r.CPUSeconds = 0
		}
		if r.ActualDurationSeconds > 0 {
			r.CPUPercentAverage = r.CPUSeconds / r.ActualDurationSeconds * 100
		}
		r.IOSupported = first.Metric.IOSupported && last.Metric.IOSupported
		if r.IOSupported {
			r.IOReadBytes = saturatingSub(last.Metric.IORead, first.Metric.IORead)
			r.IOWriteBytes = saturatingSub(last.Metric.IOWrite, first.Metric.IOWrite)
		}
	}
	r.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if !r.IOSupported {
		r.Notes = append(r.Notes, "block I/O counters unavailable; CPU/RSS remain sampled")
	}
	if runtime.GOOS != "linux" {
		r.Notes = append(r.Notes, "non-Linux sampling uses ps; descendant I/O is not available")
	}
	return r
}

func collectSnapshot(root int) (snapshot, error) {
	return collectPlatformSnapshot(root)
}

func writeReport(path, format string, r report) error {
	var f *os.File
	var err error
	if path == "-" || path == "" {
		f = os.Stdout
	} else {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
	}
	if format == "tsv" {
		_, err = fmt.Fprintf(f, "schema_version\tlabel\troot_pid\tmeasurement_mode\trequested_duration_seconds\tactual_duration_seconds\tsamples\tprocess_count_max\tcpu_seconds\tcpu_percent_average\trss_average_bytes\trss_max_bytes\trss_final_bytes\tio_read_bytes\tio_write_bytes\tio_supported\texited\texit_code\ttimed_out\tsampler_errors\tcommand\n")
		if err != nil {
			return err
		}
		exit := ""
		if r.ExitCode != nil {
			exit = strconv.Itoa(*r.ExitCode)
		}
		_, err = fmt.Fprintf(f, "%s\t%s\t%d\t%s\t%.6f\t%.6f\t%d\t%d\t%.6f\t%.3f\t%d\t%d\t%d\t%d\t%d\t%t\t%t\t%s\t%t\t%d\t%s\n", r.Schema, tsv(r.Label), r.PID, r.MeasurementMode, r.RequestedDurationSeconds, r.ActualDurationSeconds, r.Samples, r.ProcessCountMax, r.CPUSeconds, r.CPUPercentAverage, r.RSSAverageBytes, r.RSSMaxBytes, r.RSSFinalBytes, r.IOReadBytes, r.IOWriteBytes, r.IOSupported, r.Exited, exit, r.TimedOut, r.SamplerErrors, tsv(strings.Join(r.Command, " ")))
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func tsv(s string) string { return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s) }
func saturatingSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}
func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
func setExitCode(r *report, err error) {
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if status, ok := ee.Sys().(syscall.WaitStatus); ok {
				if status.Signaled() {
					code = 128 + int(status.Signal())
				} else {
					code = status.ExitStatus()
				}
			} else {
				code = 1
			}
		} else {
			code = 1
		}
	}
	r.ExitCode = &code
}

func measurementMode() string {
	if runtime.GOOS == "linux" {
		return "proc"
	}
	return "ps"
}
func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "resource-benchmark: "+format+"\n", args...)
	os.Exit(2)
}
