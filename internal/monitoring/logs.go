package monitoring

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	DefaultLogEntries   = 200
	DefaultLogBytes     = 1 << 20
	DefaultLogLineSize  = 64 << 10
	MaxLiveTailDuration = 5 * time.Minute
)

type LogReadOptions struct {
	MaxEntries  int
	MaxBytes    int
	MaxLineSize int
	From        time.Time
	To          time.Time
	Severity    string
	Search      string
	Now         func() time.Time
	StartOffset int64
}

type LogReadResult struct {
	Entries   []LogEntry
	Truncated bool
	// pending is true when the file ended in an unterminated line. The bytes
	// have been deliberately withheld so a resumable cursor never splits one
	// logical event across two responses.
	pending bool
}

type JournalOptions struct {
	Unit       string
	Cursor     string
	MaxEntries int
	MaxBytes   int
	Since      time.Time
	Until      time.Time
	Now        func() time.Time
}

var sensitivePattern = regexp.MustCompile(`(?i)(password|passwd|token|secret|authorization|private[_-]?key)\s*[:=]\s*[^\s,;]+`)
var sensitiveJSONPattern = regexp.MustCompile(`(?i)["'](?:password|passwd|token|secret|authorization|private[_-]?key)["']\s*:\s*(?:"[^"]*"|'[^']*'|[^,}\s]+)`)

// ReadConfiguredLog reads only a caller-approved regular file. It rejects
// symlinks, caps bytes/lines/entries, preserves multiline continuations, and
// redacts obvious credential fields before persistence.
func ReadConfiguredLog(ctx context.Context, source LogSource, options LogReadOptions) (LogReadResult, error) {
	return readConfiguredLogAtOffset(ctx, source, options, options.StartOffset)
}

// ReadConfiguredLogCursor resumes a historical source cursor when it still
// names the current file. Rotation safely restarts from offset zero.
func ReadConfiguredLogCursor(ctx context.Context, source LogSource, options LogReadOptions, cursor string) (LogReadResult, error) {
	if cursor == "" {
		return ReadConfiguredLog(ctx, source, options)
	}
	cursorID, offset, _, err := decodeFileCursorWithFingerprint(cursor)
	if err != nil {
		return LogReadResult{}, err
	}
	if info, statErr := os.Lstat(source.Path); statErr == nil && cursorID == fileIdentity(info) {
		options.StartOffset = offset
	}
	return ReadConfiguredLog(ctx, source, options)
}

// ReadJournal reads an already-running systemd journal without accepting a
// shell command. It is intentionally a snapshot operation; cursor-based live
// journal streaming is owned by the transport/API integration.
func ReadJournal(ctx context.Context, source LogSource, options JournalOptions) (LogReadResult, error) {
	if source.ServerID == "" || source.ID == "" {
		return LogReadResult{}, errors.New("journal source requires server and id")
	}
	if options.MaxEntries <= 0 || options.MaxEntries > MaxPageItems {
		options.MaxEntries = DefaultLogEntries
	}
	if options.MaxBytes <= 0 || options.MaxBytes > MaxLogBytes {
		options.MaxBytes = DefaultLogBytes
	}
	args := []string{"--no-pager", "--quiet", "--output=json", "-n", strconv.Itoa(options.MaxEntries)}
	if options.Unit != "" {
		if !isSafeJournalUnit(options.Unit) {
			return LogReadResult{}, errors.New("invalid journal unit")
		}
		args = append(args, "-u", options.Unit)
	}
	if options.Cursor != "" {
		if !isSafeJournalCursor(options.Cursor) {
			return LogReadResult{}, errors.New("invalid journal cursor")
		}
		args = append(args, "--after-cursor", options.Cursor)
	}
	if !options.Since.IsZero() {
		args = append(args, "--since", options.Since.UTC().Format(time.RFC3339Nano))
	}
	if !options.Until.IsZero() {
		args = append(args, "--until", options.Until.UTC().Format(time.RFC3339Nano))
	}
	command := exec.CommandContext(ctx, "journalctl", args...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return LogReadResult{}, err
	}
	if err := command.Start(); err != nil {
		return LogReadResult{}, fmt.Errorf("start journalctl: %w", err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = command.Wait()
		}
	}()
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	result := LogReadResult{Entries: make([]LogEntry, 0, options.MaxEntries)}
	scanner := bufio.NewScanner(io.LimitReader(stdout, int64(options.MaxBytes)+1))
	scanner.Buffer(make([]byte, 4096), DefaultLogLineSize)
	consumed := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		consumed += len(line) + 1
		if consumed > options.MaxBytes {
			result.Truncated = true
			_ = command.Process.Kill()
			break
		}
		var raw struct {
			Cursor    string `json:"__CURSOR"`
			Timestamp string `json:"__REALTIME_TIMESTAMP"`
			Priority  string `json:"PRIORITY"`
			Unit      string `json:"_SYSTEMD_UNIT"`
			Message   string `json:"MESSAGE"`
		}
		if err := json.Unmarshal(line, &raw); err != nil {
			result.Truncated = true
			continue
		}
		timestamp := now().UTC()
		if micros, err := strconv.ParseInt(raw.Timestamp, 10, 64); err == nil && micros > 0 {
			timestamp = time.Unix(0, micros*int64(time.Microsecond)).UTC()
		}
		text, redacted := redact(strings.ToValidUTF8(raw.Message, "�"))
		entry := LogEntry{ServerID: source.ServerID, SourceID: source.ID, Cursor: raw.Cursor, Timestamp: timestamp, Severity: DetectSeverity(text, journalSeverity(raw.Priority)), Text: text, Redacted: redacted}
		if entry.Cursor == "" {
			entry.Cursor = fmt.Sprintf("journal:%d", len(result.Entries))
		}
		result.Entries = append(result.Entries, entry)
		if len(result.Entries) >= options.MaxEntries {
			result.Truncated = true
			_ = command.Process.Kill()
			break
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return LogReadResult{}, ctx.Err()
		}
		result.Truncated = true
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	waited = true
	if waitErr != nil && !result.Truncated && ctx.Err() == nil {
		return LogReadResult{}, fmt.Errorf("journalctl: %w", waitErr)
	}
	return result, nil
}

type JournalTailOptions struct {
	Cursor       string
	MaxEntries   int
	MaxBytes     int
	MaxDuration  time.Duration
	PollInterval time.Duration
	Now          func() time.Time
}

// TailJournal polls an approved journald unit with --after-cursor. It keeps
// the same bounded wait and response limits as file tails without accepting a
// shell command or exposing arbitrary journal units.
func TailJournal(ctx context.Context, source LogSource, options JournalTailOptions) (LogReadResult, error) {
	if source.ServerID == "" || source.ID == "" || source.Path != "" {
		return LogReadResult{}, errors.New("journal tail requires a registered journal source")
	}
	if options.MaxEntries <= 0 || options.MaxEntries > MaxPageItems {
		options.MaxEntries = DefaultLogEntries
	}
	if options.MaxBytes <= 0 || options.MaxBytes > MaxLogBytes {
		options.MaxBytes = DefaultLogBytes
	}
	if options.MaxDuration <= 0 || options.MaxDuration > MaxLiveTailDuration {
		options.MaxDuration = 30 * time.Second
	}
	if options.PollInterval <= 0 || options.PollInterval > 5*time.Second {
		options.PollInterval = 250 * time.Millisecond
	}
	if options.Cursor != "" && !isSafeJournalCursor(options.Cursor) {
		return LogReadResult{}, errors.New("invalid journal cursor")
	}
	deadline := time.NewTimer(options.MaxDuration)
	defer deadline.Stop()
	result := LogReadResult{Entries: make([]LogEntry, 0, options.MaxEntries)}
	cursor := options.Cursor
	for {
		batch, err := ReadJournal(ctx, source, JournalOptions{Unit: source.ID, Cursor: cursor, MaxEntries: options.MaxEntries, MaxBytes: options.MaxBytes, Now: options.Now})
		if err != nil {
			return LogReadResult{}, err
		}
		if len(batch.Entries) > 0 {
			result.Entries = batch.Entries
			result.Truncated = batch.Truncated
			return result, nil
		}
		select {
		case <-ctx.Done():
			return LogReadResult{}, ctx.Err()
		case <-deadline.C:
			return result, nil
		case <-time.After(options.PollInterval):
		}
	}
}

func isSafeJournalUnit(unit string) bool {
	if unit == "" || len(unit) > 256 {
		return false
	}
	for _, character := range unit {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && !strings.ContainsRune("@._:-", character) {
			return false
		}
	}
	return true
}

func isSafeJournalCursor(cursor string) bool {
	if cursor == "" || len(cursor) > 512 {
		return false
	}
	for _, character := range cursor {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && !strings.ContainsRune("=;:_-", character) {
			return false
		}
	}
	return true
}

func journalSeverity(priority string) string {
	switch priority {
	case "0", "1":
		return "CRITICAL"
	case "2":
		return "ERROR"
	case "3":
		return "ERROR"
	case "4":
		return "WARN"
	case "5", "6":
		return "INFO"
	default:
		return "DEBUG"
	}
}

// DetectSeverity inspects log message text for common error and warning patterns,
// returning the elevated severity or the fallback value when no higher priority pattern matches.
func DetectSeverity(text string, fallback string) string {
	upper := strings.ToUpper(text)
	// Explicit error markers
	if strings.HasPrefix(upper, "[ERROR]") || strings.HasPrefix(upper, "[ERR]") ||
		strings.HasPrefix(upper, "[FATAL]") || strings.HasPrefix(upper, "[CRIT]") ||
		strings.HasPrefix(upper, "[EMERG]") || strings.HasPrefix(upper, "[ALERT]") ||
		strings.HasPrefix(upper, "ERROR:") || strings.HasPrefix(upper, "ERR:") ||
		strings.HasPrefix(upper, "FATAL:") || strings.HasPrefix(upper, "PANIC:") ||
		strings.HasPrefix(upper, "CRITICAL:") || strings.HasPrefix(upper, "EMERGENCY:") ||
		strings.Contains(upper, "LEVEL=ERROR") || strings.Contains(upper, `LEVEL="ERROR"`) ||
		strings.Contains(upper, `"LEVEL":"ERROR"`) || strings.Contains(upper, `"LEVEL": "ERROR"`) ||
		strings.Contains(upper, "LVL=ERROR") || strings.Contains(upper, "LVL=ERR") ||
		strings.Contains(upper, "STATUS=1/FAILURE") || strings.Contains(upper, "RESULT 'EXIT-CODE'") ||
		strings.Contains(upper, "SQLITE_BUSY") || strings.Contains(upper, "DATABASE IS LOCKED") {
		return "ERROR"
	}
	// Explicit warning markers
	if strings.HasPrefix(upper, "[WARN]") || strings.HasPrefix(upper, "[WARNING]") ||
		strings.HasPrefix(upper, "WARN:") || strings.HasPrefix(upper, "WARNING:") ||
		strings.Contains(upper, "LEVEL=WARN") || strings.Contains(upper, `LEVEL="WARN"`) ||
		strings.Contains(upper, `"LEVEL":"WARN"`) || strings.Contains(upper, `"LEVEL": "WARN"`) ||
		strings.Contains(upper, "LVL=WARN") || strings.Contains(upper, "QUEUE DEPTH") ||
		(strings.Contains(upper, "EXCEEDED") && strings.Contains(upper, "THRESHOLD")) {
		if fallback == "ERROR" || fallback == "CRITICAL" {
			return fallback
		}
		return "WARN"
	}
	// Debug markers
	if strings.HasPrefix(upper, "[DEBUG]") || strings.HasPrefix(upper, "DEBUG:") ||
		strings.Contains(upper, "LEVEL=DEBUG") || strings.Contains(upper, `"LEVEL":"DEBUG"`) {
		if fallback == "ERROR" || fallback == "WARN" {
			return fallback
		}
		return "DEBUG"
	}
	if fallback != "" {
		return fallback
	}
	return "INFO"
}

func readConfiguredLogAtOffset(ctx context.Context, source LogSource, options LogReadOptions, startOffset int64) (LogReadResult, error) {
	if source.ServerID == "" || source.ID == "" || source.Path == "" {
		return LogReadResult{}, errors.New("configured log source requires server, id, and path")
	}
	if !filepath.IsAbs(source.Path) {
		return LogReadResult{}, errors.New("log source path must be absolute")
	}
	path := filepath.Clean(source.Path)
	if err := validateConfiguredPath(path); err != nil {
		return LogReadResult{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return LogReadResult{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return LogReadResult{}, errors.New("log source must be a regular non-symlink file")
	}
	file, err := os.Open(path)
	if err != nil {
		return LogReadResult{}, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return LogReadResult{}, err
	}
	if !os.SameFile(info, openedInfo) || openedInfo.Mode()&os.ModeSymlink != 0 || !openedInfo.Mode().IsRegular() {
		return LogReadResult{}, errors.New("log source changed to a non-regular or different file")
	}
	if startOffset < 0 {
		return LogReadResult{}, errors.New("log cursor offset is invalid")
	}
	if startOffset > 0 {
		if _, err := file.Seek(startOffset, io.SeekStart); err != nil {
			return LogReadResult{}, err
		}
	}
	if options.MaxEntries <= 0 || options.MaxEntries > MaxPageItems {
		options.MaxEntries = DefaultLogEntries
	}
	if options.MaxBytes <= 0 || options.MaxBytes > MaxLogBytes {
		options.MaxBytes = DefaultLogBytes
	}
	if options.MaxLineSize <= 0 || options.MaxLineSize > DefaultLogLineSize {
		options.MaxLineSize = DefaultLogLineSize
	}
	now := options.Now
	if now == nil {
		// Timestamp-less lines represent the requested snapshot. Anchor them to
		// its upper bound when supplied; using a later wall-clock instant made a
		// normal `to=now` browser request filter every such line back out.
		if !options.To.IsZero() {
			upper := options.To.UTC()
			now = func() time.Time { return upper }
		} else {
			now = func() time.Time { return time.Now().UTC() }
		}
	}
	reader := bufio.NewReaderSize(file, 32<<10)
	fileID := fileIdentity(info)
	result := LogReadResult{Entries: make([]LogEntry, 0, options.MaxEntries)}
	offset := startOffset
	var pending *LogEntry
	var consumed int
	for {
		if err := ctx.Err(); err != nil {
			return LogReadResult{}, err
		}
		remaining := options.MaxBytes - consumed
		if remaining <= 0 {
			result.Truncated = true
			break
		}
		line, bytesRead, eof, lineTruncated, budgetExceeded, readErr := readBoundedLine(reader, options.MaxLineSize, remaining)
		offset += int64(bytesRead)
		consumed += bytesRead
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return LogReadResult{}, readErr
		}
		if bytesRead == 0 && eof {
			break
		}
		terminated := strings.HasSuffix(line, "\n")
		if eof && !terminated && !lineTruncated && !budgetExceeded {
			// A partial top-level line proves that an earlier complete pending
			// entry ended; a partial continuation does not. Flush only in the
			// former case, while retaining the original resume offset for the
			// incomplete bytes.
			partial := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			_, _, _, continuation := parseLogLine(partial, now())
			if pending != nil && !continuation {
				if includeLog(*pending, options) {
					result.Entries = append(result.Entries, *pending)
					if len(result.Entries) >= options.MaxEntries {
						result.Truncated = true
						break
					}
				}
				pending = nil
			}
			// Keep an interrupted write pending. In particular, do not emit a
			// cursor at offset+bytesRead: doing so would cause the completed suffix
			// to be returned as a new, separate log entry on the next tail request.
			result.pending = true
			break
		}
		if budgetExceeded {
			result.Truncated = true
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		line = strings.ToValidUTF8(line, "�")
		if len(line) > options.MaxLineSize {
			lineTruncated = true
		}
		line = capUTF8(line, options.MaxLineSize)
		if line == "" && eof {
			break
		}
		severity, text, timestamp, continuation := parseLogLine(line, now())
		if continuation && pending != nil {
			redactedText, redacted := redact(text)
			if len(pending.Text)+1+len(redactedText) <= options.MaxLineSize {
				pending.Text += "\n" + redactedText
				pending.Redacted = pending.Redacted || redacted
				pending.Cursor = makeFileCursor(file, fileID, offset)
			} else {
				pending.Truncated = true
				pending.Cursor = makeFileCursor(file, fileID, offset)
			}
		} else {
			if pending != nil {
				if includeLog(*pending, options) {
					result.Entries = append(result.Entries, *pending)
					if len(result.Entries) >= options.MaxEntries {
						result.Truncated = true
						break
					}
				}
			}
			redactedText, redacted := redact(text)
			pending = &LogEntry{ServerID: source.ServerID, SourceID: source.ID, Timestamp: timestamp, Severity: severity, Text: redactedText, Truncated: lineTruncated, Redacted: redacted, Cursor: makeFileCursor(file, fileID, offset)}
		}
		if eof || budgetExceeded {
			break
		}
	}
	if pending != nil && !result.pending && len(result.Entries) < options.MaxEntries && includeLog(*pending, options) {
		result.Entries = append(result.Entries, *pending)
	}
	return result, nil
}

func validateConfiguredPath(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("log source path must be absolute")
	}
	info, err := os.Lstat(filepath.Clean(path))
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("log source path cannot be a symlink")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type LogTailOptions struct {
	Cursor       string
	MaxEntries   int
	MaxBytes     int
	MaxDuration  time.Duration
	PollInterval time.Duration
	Now          func() time.Time
}

// TailConfiguredLog performs a bounded, cursor-aware live tail. It returns
// as soon as appended entries are available, or when the bounded wait expires;
// rotation/truncation resets to the new file and marks the result truncated.
func TailConfiguredLog(ctx context.Context, source LogSource, options LogTailOptions) (LogReadResult, error) {
	if options.MaxDuration <= 0 || options.MaxDuration > MaxLiveTailDuration {
		options.MaxDuration = 30 * time.Second
	}
	if options.PollInterval <= 0 || options.PollInterval > 5*time.Second {
		options.PollInterval = 250 * time.Millisecond
	}
	fileID, offset, expectedFingerprint, err := decodeFileCursorWithFingerprint(options.Cursor)
	if err != nil {
		return LogReadResult{}, err
	}
	deadline := time.NewTimer(options.MaxDuration)
	defer deadline.Stop()
	result := LogReadResult{Entries: make([]LogEntry, 0)}
	reset := false
	for {
		info, err := os.Lstat(source.Path)
		if err != nil {
			return LogReadResult{}, err
		}
		currentID := fileIdentity(info)
		fingerprintMismatch := false
		if fileID != "" && expectedFingerprint != "" && offset <= info.Size() {
			currentFingerprint, fingerprintErr := fileFingerprintAtPath(source.Path, offset)
			if fingerprintErr != nil {
				return LogReadResult{}, fingerprintErr
			}
			fingerprintMismatch = currentFingerprint != expectedFingerprint
		}
		if fileID != "" && (fileID != currentID || offset > info.Size() || fingerprintMismatch) {
			offset, reset = 0, true
			expectedFingerprint = ""
		}
		batch, err := readConfiguredLogAtOffset(ctx, source, LogReadOptions{MaxEntries: options.MaxEntries, MaxBytes: options.MaxBytes, MaxLineSize: DefaultLogLineSize, Now: options.Now}, offset)
		if err != nil {
			return LogReadResult{}, err
		}
		if reset {
			batch.Truncated = true
		}
		if len(batch.Entries) > 0 {
			result.Entries = batch.Entries
			result.Truncated = batch.Truncated
			return result, nil
		}
		if batch.Truncated {
			return batch, nil
		}
		select {
		case <-ctx.Done():
			return LogReadResult{}, ctx.Err()
		case <-deadline.C:
			return result, nil
		case <-time.After(options.PollInterval):
			fileID = currentID
			if !batch.pending {
				offset = info.Size()
			}
			reset = false
		}
	}
}

func fileIdentity(info os.FileInfo) string {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		// Modification time changes on every append and therefore cannot be
		// part of a rotation-safe cursor identity. Device/inode remain stable
		// for ordinary appends and change when a file is replaced or rotated.
		return fmt.Sprintf("%x-%x", uint64(stat.Dev), uint64(stat.Ino))
	}
	// Non-Unix filesystems may not expose device/inode. Name plus mode is the
	// most stable identity available through os.FileInfo; the caller still
	// detects truncation through the offset-versus-size check.
	return fmt.Sprintf("%s-%o", info.Name(), info.Mode())
}

func decodeFileCursor(value string) (string, int64, error) {
	fileID, offset, _, err := decodeFileCursorWithFingerprint(value)
	return fileID, offset, err
}

func decodeFileCursorWithFingerprint(value string) (string, int64, string, error) {
	if value == "" {
		return "", 0, "", nil
	}
	if len(value) > 256 {
		return "", 0, "", errors.New("invalid log cursor")
	}
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return "", 0, "", errors.New("invalid log cursor")
	}
	offset, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || offset < 0 {
		return "", 0, "", errors.New("invalid log cursor")
	}
	fingerprint := ""
	if len(parts) == 3 {
		if len(parts[2]) != sha256.Size*2 {
			return "", 0, "", errors.New("invalid log cursor")
		}
		if _, err := hex.DecodeString(parts[2]); err != nil {
			return "", 0, "", errors.New("invalid log cursor")
		}
		fingerprint = parts[2]
	}
	return parts[0], offset, fingerprint, nil
}

func makeFileCursor(file *os.File, fileID string, offset int64) string {
	fingerprint := fileFingerprint(file, offset)
	if fingerprint == "" {
		return fmt.Sprintf("%s:%d", fileID, offset)
	}
	return fmt.Sprintf("%s:%d:%s", fileID, offset, fingerprint)
}

func fileFingerprintAtPath(path string, offset int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return fileFingerprint(file, offset), nil
}

func fileFingerprint(file *os.File, offset int64) string {
	if offset < 0 {
		return ""
	}
	hash := sha256.New()
	readRange := func(start, length int64) bool {
		if length <= 0 {
			return true
		}
		buffer := make([]byte, length)
		read, err := file.ReadAt(buffer, start)
		if err != nil && !errors.Is(err, io.EOF) {
			return false
		}
		_, _ = hash.Write(buffer[:read])
		return true
	}
	const window int64 = 4096
	prefixLength := offset
	if prefixLength > window {
		prefixLength = window
	}
	if !readRange(0, prefixLength) {
		return ""
	}
	if offset > window {
		if !readRange(offset-window, window) {
			return ""
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func readBoundedLine(reader *bufio.Reader, maxLine, maxBytes int) (string, int, bool, bool, bool, error) {
	var output strings.Builder
	bytesRead := 0
	truncated := false
	if maxLine <= 0 || maxBytes <= 0 {
		return "", 0, false, true, true, nil
	}
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(chunk) > maxBytes-bytesRead {
			remaining := maxBytes - bytesRead
			if remaining > 0 {
				if output.Len()+remaining > maxLine {
					remaining = maxLine - output.Len()
					truncated = true
				}
				if remaining > 0 {
					_, _ = output.Write(chunk[:remaining])
				}
			}
			return output.String(), bytesRead + len(chunk), false, true, true, nil
		}
		bytesRead += len(chunk)
		if output.Len()+len(chunk) > maxLine {
			remaining := maxLine - output.Len()
			if remaining > 0 {
				_, _ = output.Write(chunk[:remaining])
			}
			truncated = true
		} else {
			_, _ = output.Write(chunk)
		}
		if err == nil {
			return output.String(), bytesRead, false, truncated, false, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return output.String(), bytesRead, true, truncated, false, io.EOF
		}
		return output.String(), bytesRead, false, truncated, false, err
	}
}

func parseLogLine(line string, now time.Time) (severity, text string, timestamp time.Time, continuation bool) {
	timestamp = now.UTC()
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
		return "INFO", trimmed, timestamp, true
	}
	fields := strings.Fields(trimmed)
	for count := 1; count <= 2 && count <= len(fields); count++ {
		candidate := strings.Join(fields[:count], " ")
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, candidate); err == nil {
				timestamp = parsed.UTC()
				trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, candidate))
				count = len(fields) + 1
				break
			}
		}
	}
	severity = "INFO"
	upper := strings.ToUpper(trimmed)
	for _, level := range []string{"DEBUG", "INFO", "NOTICE", "WARN", "WARNING", "ERROR", "CRITICAL"} {
		prefix := "[" + level + "]"
		if strings.HasPrefix(upper, prefix) {
			severity = level
			trimmed = strings.TrimSpace(trimmed[len(prefix):])
			break
		}
		colonPrefix := level + ":"
		if strings.HasPrefix(upper, colonPrefix) {
			severity = level
			trimmed = strings.TrimSpace(trimmed[len(colonPrefix):])
			break
		}
	}
	severity = DetectSeverity(trimmed, severity)
	return severity, trimmed, timestamp, false
}

func includeLog(entry LogEntry, options LogReadOptions) bool {
	if !options.From.IsZero() && entry.Timestamp.Before(options.From) {
		return false
	}
	if !options.To.IsZero() && entry.Timestamp.After(options.To) {
		return false
	}
	if options.Severity != "" && !strings.EqualFold(options.Severity, entry.Severity) {
		return false
	}
	return options.Search == "" || strings.Contains(strings.ToLower(entry.Text), strings.ToLower(options.Search))
}

func redact(text string) (string, bool) {
	redacted := sensitivePattern.ReplaceAllStringFunc(text, func(value string) string {
		index := strings.IndexAny(value, ":=")
		if index < 0 {
			return value
		}
		return value[:index+1] + "[REDACTED]"
	})
	redacted = sensitiveJSONPattern.ReplaceAllStringFunc(redacted, func(value string) string {
		index := strings.IndexByte(value, ':')
		if index < 0 {
			return value
		}
		return value[:index+1] + "[REDACTED]"
	})
	return redacted, redacted != text
}

func capUTF8(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	for len(text) > maxBytes {
		_, size := utf8.DecodeLastRuneInString(text)
		if size <= 0 {
			break
		}
		text = text[:len(text)-size]
	}
	return text
}
