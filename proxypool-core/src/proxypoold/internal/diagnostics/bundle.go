package diagnostics

import (
	"context"
	"errors"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	MaxEntryBytes         = 2 << 20
	MaxBundleBytes        = 16 << 20
	MaxSeedEntries        = 32
	MaxCommands           = 32
	TruncationMarker      = "[truncated]"
	defaultCommandTimeout = 5 * time.Second
)

var entryNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type Runner interface {
	RunBounded(context.Context, int, string, ...string) ([]byte, bool, error)
}

type Command struct {
	Name string
	Path string
	Args []string
}

type Entry struct {
	Name string
	Data []byte
}

type CollectorOption func(*Collector)

func WithCommandTimeout(timeout time.Duration) CollectorOption {
	return func(collector *Collector) {
		if timeout > 0 {
			collector.commandTimeout = timeout
		}
	}
}

type Collector struct {
	runner         Runner
	redactor       *Redactor
	commands       []Command
	commandTimeout time.Duration
}

func NewCollector(runner Runner, redactor *Redactor, commands []Command, options ...CollectorOption) *Collector {
	collector := &Collector{runner: runner, redactor: redactor, commandTimeout: defaultCommandTimeout}
	collector.commands = make([]Command, len(commands))
	for index, command := range commands {
		collector.commands[index] = Command{Name: command.Name, Path: command.Path, Args: append([]string(nil), command.Args...)}
	}
	for _, option := range options {
		if option != nil {
			option(collector)
		}
	}
	return collector
}

func (collector *Collector) Collect(ctx context.Context, seed map[string][]byte) ([]Entry, error) {
	if collector == nil || ctx == nil || collector.commandTimeout <= 0 || len(seed) > MaxSeedEntries || len(collector.commands) > MaxCommands || (len(collector.commands) > 0 && collector.runner == nil) {
		return nil, errors.New("diagnostic collector is invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(seed)+len(collector.commands))
	entries := make([]Entry, 0, len(seed)+len(collector.commands))
	total := 0
	seedNames := make([]string, 0, len(seed))
	for name := range seed {
		seedNames = append(seedNames, name)
	}
	sort.Strings(seedNames)
	for _, name := range seedNames {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := validateEntryName(name, seen); err != nil {
			return nil, err
		}
		entry, nextTotal, fits := collector.makeEntry(name, seed[name], total, false)
		if !fits {
			return appendBundleTruncation(entries, total, seen), nil
		}
		entries, total = append(entries, entry), nextTotal
	}
	for _, command := range collector.commands {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := validateCommand(command, seen); err != nil {
			return nil, err
		}
		limit := payloadLimit(total)
		if limit <= 0 {
			return appendBundleTruncation(entries, total, seen), nil
		}
		commandCtx, cancel := context.WithTimeout(ctx, collector.commandTimeout)
		output, outputTruncated, err := collector.runner.RunBounded(commandCtx, limit, command.Path, command.Args...)
		deadlineError := commandCtx.Err()
		cancel()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err != nil {
			if errors.Is(deadlineError, context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
				output = []byte("collection_timeout\n")
			} else {
				output = []byte("collection_failed\n")
			}
		}
		entry, nextTotal, fits := collector.makeEntry(command.Name, output, total, outputTruncated)
		if !fits {
			return appendBundleTruncation(entries, total, seen), nil
		}
		entries, total = append(entries, entry), nextTotal
	}
	return entries, nil
}

func (collector *Collector) makeEntry(name string, data []byte, total int, alreadyTruncated bool) (Entry, int, bool) {
	limit := payloadLimit(total)
	if limit <= 0 {
		return Entry{}, total, false
	}
	truncated := alreadyTruncated || len(data) > limit
	if len(data) > limit {
		data = data[:limit]
	}
	data = collector.redactor.Redact(data)
	entryLimit := MaxEntryBytes
	if remaining := MaxBundleBytes - total; remaining < entryLimit {
		entryLimit = remaining
	}
	if truncated {
		data = withTruncationMarker(data, entryLimit)
	} else {
		data = truncate(data, entryLimit)
	}
	return Entry{Name: name, Data: data}, total + len(data), true
}

func truncate(data []byte, limit int) []byte {
	if len(data) <= limit {
		return append([]byte(nil), data...)
	}
	marker := []byte("\n" + TruncationMarker + "\n")
	if limit <= len(marker) {
		return append([]byte(nil), marker[:limit]...)
	}
	result := make([]byte, 0, limit)
	result = append(result, data[:limit-len(marker)]...)
	result = append(result, marker...)
	return result
}

func withTruncationMarker(data []byte, limit int) []byte {
	marker := []byte("\n" + TruncationMarker + "\n")
	if limit < len(marker) {
		return nil
	}
	if len(data) > limit-len(marker) {
		data = data[:limit-len(marker)]
	}
	result := make([]byte, 0, len(data)+len(marker))
	result = append(result, data...)
	result = append(result, marker...)
	return result
}

func payloadLimit(total int) int {
	markerLength := len("\n" + TruncationMarker + "\n")
	limit := MaxEntryBytes - markerLength
	if remaining := MaxBundleBytes - total - markerLength; remaining < limit {
		limit = remaining
	}
	return limit
}

func appendBundleTruncation(entries []Entry, total int, seen map[string]struct{}) []Entry {
	name := "bundle-truncated.txt"
	if _, exists := seen[name]; exists {
		return entries
	}
	marker := []byte(TruncationMarker + "\n")
	if len(marker) > MaxBundleBytes-total {
		return entries
	}
	return append(entries, Entry{Name: name, Data: marker})
}

func validateEntryName(name string, seen map[string]struct{}) error {
	if !entryNamePattern.MatchString(name) || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return errors.New("diagnostic entry name is unsafe")
	}
	if _, exists := seen[name]; exists {
		return errors.New("diagnostic entry name is duplicated")
	}
	seen[name] = struct{}{}
	return nil
}

func validateCommand(command Command, seen map[string]struct{}) error {
	if err := validateEntryName(command.Name, seen); err != nil {
		return err
	}
	if !strings.HasPrefix(command.Path, "/") || path.Clean(command.Path) != command.Path || strings.ContainsRune(command.Path, 0) || strings.Contains(command.Path, `\`) {
		return errors.New("diagnostic command is unsafe")
	}
	for _, argument := range command.Args {
		if strings.ContainsRune(argument, 0) {
			return errors.New("diagnostic command is unsafe")
		}
	}
	return nil
}
