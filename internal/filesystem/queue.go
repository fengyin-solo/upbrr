// Copyright (c) 2025-2026, Audionut and the autobrr contributors.
// SPDX-License-Identifier: GPL-2.0-or-later

package filesystem

import (
	"context"
	"fmt"
	"os"
	"path" //nolint:depguard // Normalizes slash-style queue ignore entries, which are cross-platform config data, not local filesystem paths.
	"path/filepath"
	"sort"
	"strings"

	internalerrors "github.com/autobrr/upbrr/internal/errors"
	"github.com/autobrr/upbrr/internal/pathing"
)

// QueueIgnoreFileName is the optional exclusion list read from a named queue
// scan root. Each non-comment line names a root-relative first-level candidate
// that discovery must leave out.
const QueueIgnoreFileName = ".upbrr-queue-ignore"

// QueueOptions configures optional queue discovery behavior. The zero value
// preserves the historical GatherQueuePaths behavior.
type QueueOptions struct {
	// IgnoreFile names an optional exclusion list relative to the scan root.
	// When empty, no ignore file is read. A missing named file leaves
	// discovery unchanged; unsafe entries fail discovery with a line number.
	IgnoreFile string
}

var queueVideoExts = map[string]struct{}{
	".mkv": {},
	".mp4": {},
	".ts":  {},
}

// GatherQueuePaths returns sorted absolute host paths for first-level upload
// candidates. A file root is returned directly; directory children are kept
// only when they are video files or directories containing video or disc data.
// Root whitespace is preserved except that an empty or whitespace-only root is rejected.
// Ignore-file processing is disabled; use GatherQueuePathsWithOptions to opt in.
func GatherQueuePaths(ctx context.Context, root string) ([]string, error) {
	return GatherQueuePathsWithOptions(ctx, root, QueueOptions{})
}

// GatherQueuePathsWithOptions behaves like GatherQueuePaths and additionally
// honors opts.IgnoreFile when it is set and present in the scan root. Its
// entries name root-relative first-level candidates excluded from results; a
// directory entry also covers everything beneath it. Matching compares
// normalized slash paths exactly, including case. Unsafe entries fail before
// the directory is scanned.
func GatherQueuePathsWithOptions(ctx context.Context, root string, opts QueueOptions) ([]string, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("filesystem: empty queue path: %w", internalerrors.ErrInvalidInput)
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("filesystem: resolve queue path: %w", err)
	}

	info, err := os.Stat(absRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("filesystem: queue path %q: %w", absRoot, internalerrors.ErrNotFound)
		}
		return nil, fmt.Errorf("filesystem: queue path %q: %w", absRoot, err)
	}

	if !info.IsDir() {
		return []string{absRoot}, nil
	}

	var ignored map[string]struct{}
	if opts.IgnoreFile != "" {
		ignored, err = readQueueIgnoreFile(absRoot, opts.IgnoreFile)
		if err != nil {
			return nil, err
		}
	}

	entries, err := os.ReadDir(absRoot)
	if err != nil {
		return nil, fmt.Errorf("filesystem: read queue path %q: %w", absRoot, err)
	}

	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context canceled: %w", ctx.Err())
		default:
		}

		if _, ok := ignored[filepath.ToSlash(entry.Name())]; ok {
			continue
		}

		entryPath := filepath.Join(absRoot, entry.Name())
		if entry.IsDir() {
			include, includeErr := shouldIncludeQueueDirectory(ctx, entryPath)
			if includeErr != nil {
				return nil, includeErr
			}
			if include {
				paths = append(paths, entryPath)
			}
			continue
		}

		if isQueueVideoFile(entry.Name()) {
			paths = append(paths, entryPath)
		}
	}

	sort.Strings(paths)
	return paths, nil
}

// readQueueIgnoreFile loads the optional first-level exclusion list named
// relative to absRoot. A missing file reports no exclusions. Blank lines and
// lines beginning with # are ignored; remaining entries must stay inside the
// scan root and fail with their 1-based line number otherwise.
func readQueueIgnoreFile(absRoot, name string) (map[string]struct{}, error) {
	ignorePath := filepath.Join(absRoot, name)
	if !pathing.IsWithinRoot(absRoot, ignorePath) {
		return nil, fmt.Errorf("filesystem: queue ignore file %q: %w", name, internalerrors.ErrInvalidInput)
	}

	content, err := os.ReadFile(ignorePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("filesystem: read queue ignore %q: %w", ignorePath, err)
	}

	ignored := make(map[string]struct{})
	for index, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		normalized, err := normalizeQueueIgnoreEntry(absRoot, line)
		if err != nil {
			return nil, fmt.Errorf("filesystem: read queue ignore %q line %d: %w", ignorePath, index+1, err)
		}
		ignored[normalized] = struct{}{}
	}
	return ignored, nil
}

// normalizeQueueIgnoreEntry validates one ignore entry on every operating
// system: absolute paths, Windows drives, UNC roots, parent references and
// anything resolving outside the scan root are rejected. The returned value
// is the normalized slash-style relative path used for exact matching.
func normalizeQueueIgnoreEntry(absRoot, entry string) (string, error) {
	slashed := strings.ReplaceAll(entry, "\\", "/")
	switch {
	case strings.HasPrefix(slashed, "/"):
		// Also covers leading backslashes, including UNC roots, once normalized.
		return "", fmt.Errorf("%w: %q must be relative to the queue root", internalerrors.ErrInvalidInput, entry)
	case hasQueueIgnoreDrivePrefix(slashed):
		return "", fmt.Errorf("%w: %q must not use a drive letter", internalerrors.ErrInvalidInput, entry)
	}

	for segment := range strings.SplitSeq(slashed, "/") {
		if segment == ".." {
			return "", fmt.Errorf("%w: %q must not reference a parent directory", internalerrors.ErrInvalidInput, entry)
		}
	}

	normalized := path.Clean(slashed)
	if normalized == "." || normalized == "" {
		return "", fmt.Errorf("%w: %q must name a queue candidate below the root", internalerrors.ErrInvalidInput, entry)
	}

	target := filepath.Join(absRoot, filepath.FromSlash(normalized))
	if !pathing.IsWithinRoot(absRoot, target) {
		return "", fmt.Errorf("%w: %q escapes the queue root", internalerrors.ErrInvalidInput, entry)
	}
	return normalized, nil
}

func hasQueueIgnoreDrivePrefix(value string) bool {
	return len(value) >= 2 && value[1] == ':' &&
		(value[0] >= 'a' && value[0] <= 'z' || value[0] >= 'A' && value[0] <= 'Z')
}

// LimitQueuePaths returns a defensive copy capped at limit. A non-positive
// limit returns all paths.
func LimitQueuePaths(paths []string, limit int) []string {
	if limit <= 0 || len(paths) <= limit {
		return append([]string{}, paths...)
	}
	return append([]string{}, paths[:limit]...)
}

func shouldIncludeQueueDirectory(ctx context.Context, dirPath string) (bool, error) {
	select {
	case <-ctx.Done():
		return false, fmt.Errorf("context canceled: %w", ctx.Err())
	default:
	}

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return false, fmt.Errorf("filesystem: read queue dir %q: %w", dirPath, err)
	}

	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("context canceled: %w", ctx.Err())
		default:
		}

		if entry.IsDir() {
			name := strings.ToUpper(strings.TrimSpace(entry.Name()))
			if name == "BDMV" || name == "VIDEO_TS" || name == "HVDVD_TS" {
				return true, nil
			}
			continue
		}
		if isQueueVideoFile(entry.Name()) {
			return true, nil
		}
	}

	return false, nil
}

func isQueueVideoFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(name)))
	_, ok := queueVideoExts[ext]
	return ok
}
