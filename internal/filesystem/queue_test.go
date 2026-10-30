// Copyright (c) 2025-2026, Audionut and the autobrr contributors.
// SPDX-License-Identifier: GPL-2.0-or-later

package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	internalerrors "github.com/autobrr/upbrr/internal/errors"
)

// queueIgnoreFixture builds a scan root with kept and ignorable first-level
// candidates. It returns the root and the sorted paths expected without any
// ignore filtering.
func queueIgnoreFixture(t *testing.T) (string, []string) {
	t.Helper()

	root := t.TempDir()
	for _, path := range []string{"keep.mkv", "skip.mkv", "note.txt"} {
		if err := os.WriteFile(filepath.Join(root, path), []byte("synthetic media"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	for _, nestedFile := range []string{filepath.Join("Season Keep", "episode1.mkv"), filepath.Join("Bad Sample", "broken.mkv")} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(nestedFile)), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(nestedFile), err)
		}
		if err := os.WriteFile(filepath.Join(root, nestedFile), []byte("synthetic media"), 0o600); err != nil {
			t.Fatalf("write %s: %v", nestedFile, err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "Empty"), 0o700); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}

	all := []string{
		filepath.Join(root, "Bad Sample"),
		filepath.Join(root, "Season Keep"),
		filepath.Join(root, "keep.mkv"),
		filepath.Join(root, "skip.mkv"),
	}
	return root, all
}

func writeQueueIgnoreFile(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, QueueIgnoreFileName), []byte(content), 0o600); err != nil {
		t.Fatalf("write queue ignore: %v", err)
	}
}

func TestGatherQueuePathsPreservesRootWhitespace(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	for _, name := range []string{" media", "media "} {
		t.Run(name, func(t *testing.T) {
			if name == "media " && runtime.GOOS == "windows" {
				t.Skip("ordinary Windows paths do not preserve trailing spaces")
			}
			for _, dir := range []string{name, "media"} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(name, "selected.mkv"), []byte("synthetic media"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join("media", "wrong.mkv"), []byte("synthetic media"), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := GatherQueuePaths(t.Context(), name)
			want := []string{filepath.Join(base, name, "selected.mkv")}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("selected wrong root: paths=%v err=%v", got, err)
			}
		})
	}
}

func TestGatherQueuePathsExpandsFirstLevelCandidates(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "movie.mkv"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write movie: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write note: %v", err)
	}

	seasonDir := filepath.Join(root, "Season 1")
	if err := os.MkdirAll(seasonDir, 0o700); err != nil {
		t.Fatalf("mkdir season: %v", err)
	}
	if err := os.WriteFile(filepath.Join(seasonDir, "episode1.mkv"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write episode: %v", err)
	}

	discDir := filepath.Join(root, "Disc")
	if err := os.MkdirAll(filepath.Join(discDir, "BDMV"), 0o700); err != nil {
		t.Fatalf("mkdir disc: %v", err)
	}

	emptyDir := filepath.Join(root, "Empty")
	if err := os.MkdirAll(emptyDir, 0o700); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}

	got, err := GatherQueuePaths(context.Background(), root)
	if err != nil {
		t.Fatalf("gather queue: %v", err)
	}

	want := []string{
		filepath.Join(root, "Disc"),
		filepath.Join(root, "Season 1"),
		filepath.Join(root, "movie.mkv"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gather queue = %#v, want %#v", got, want)
	}
}

func TestLimitQueuePaths(t *testing.T) {
	t.Parallel()

	input := []string{"a", "b", "c"}
	if got := LimitQueuePaths(input, 2); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("limited paths = %#v", got)
	}
	if got := LimitQueuePaths(input, 0); !reflect.DeepEqual(got, input) {
		t.Fatalf("unlimited paths = %#v", got)
	}
}

func TestShouldIncludeQueueDirectoryHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write movie: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	include, err := shouldIncludeQueueDirectory(ctx, dir)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got include=%v err=%v", include, err)
	}
	if include {
		t.Fatal("expected canceled scan to not include directory")
	}
}

func TestGatherQueuePathsWithOptionsExcludesIgnoredFileAndDirectory(t *testing.T) {
	t.Parallel()

	root, _ := queueIgnoreFixture(t)
	// CRLF line endings, comments and blank lines must be tolerated.
	writeQueueIgnoreFile(t, root, "# bad sample held for manual inspection\r\n\r\nskip.mkv\r\nBad Sample\r\n")

	got, err := GatherQueuePathsWithOptions(t.Context(), root, QueueOptions{IgnoreFile: QueueIgnoreFileName})
	if err != nil {
		t.Fatalf("gather queue with ignore: %v", err)
	}

	want := []string{
		filepath.Join(root, "Season Keep"),
		filepath.Join(root, "keep.mkv"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gather queue with ignore = %#v, want %#v", got, want)
	}
}

func TestGatherQueuePathsWithOptionsNormalizesIgnoredEntries(t *testing.T) {
	t.Parallel()

	root, _ := queueIgnoreFixture(t)
	writeQueueIgnoreFile(t, root, "# equivalent normalized forms\n./skip.mkv\nBad Sample/\n")

	got, err := GatherQueuePathsWithOptions(t.Context(), root, QueueOptions{IgnoreFile: QueueIgnoreFileName})
	if err != nil {
		t.Fatalf("gather queue with normalized ignore: %v", err)
	}

	want := []string{
		filepath.Join(root, "Season Keep"),
		filepath.Join(root, "keep.mkv"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gather queue with normalized ignore = %#v, want %#v", got, want)
	}
}

func TestGatherQueuePathsWithOptionsMissingIgnoreFileKeepsResults(t *testing.T) {
	t.Parallel()

	root, want := queueIgnoreFixture(t)

	got, err := GatherQueuePathsWithOptions(t.Context(), root, QueueOptions{IgnoreFile: QueueIgnoreFileName})
	if err != nil {
		t.Fatalf("gather queue without ignore file: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missing ignore file changed results = %#v, want %#v", got, want)
	}
}

func TestGatherQueuePathsLegacyEntryPointDoesNotReadIgnoreFile(t *testing.T) {
	t.Parallel()

	root, want := queueIgnoreFixture(t)
	writeQueueIgnoreFile(t, root, "skip.mkv\nBad Sample\n")

	got, err := GatherQueuePaths(t.Context(), root)
	if err != nil {
		t.Fatalf("legacy gather queue: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy gather queue honored ignore file = %#v, want %#v", got, want)
	}
}

func TestGatherQueuePathsIgnoreMatchingIsCaseSensitive(t *testing.T) {
	t.Parallel()

	root, _ := queueIgnoreFixture(t)
	// "Skip.mkv" must not match the lowercase candidate; keep.mkv still matches.
	writeQueueIgnoreFile(t, root, "keep.mkv\nSkip.mkv\n")

	got, err := GatherQueuePathsWithOptions(t.Context(), root, QueueOptions{IgnoreFile: QueueIgnoreFileName})
	if err != nil {
		t.Fatalf("gather queue with mixed case ignore: %v", err)
	}

	want := []string{
		filepath.Join(root, "Bad Sample"),
		filepath.Join(root, "Season Keep"),
		filepath.Join(root, "skip.mkv"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("case-sensitive ignore = %#v, want %#v", got, want)
	}
}

func TestReadQueueIgnoreFileRejectsUnsafeEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		entry string
	}{
		{name: "posix absolute", entry: "/etc/passwd"},
		{name: "leading backslash", entry: `\Windows\system32`},
		{name: "unc", entry: `\\server\share`},
		{name: "drive absolute", entry: `C:\bad\sample`},
		{name: "drive relative", entry: "C:bad.mkv"},
		{name: "parent reference", entry: "../outside.mkv"},
		{name: "parent below candidate", entry: "Season Keep/../../outside.mkv"},
		{name: "root itself", entry: "."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			// The unsafe entry sits on line 3 so the reported line number is asserted.
			writeQueueIgnoreFile(t, root, "# comment\n\n"+tc.entry+"\nkeep.mkv\n")

			got, err := GatherQueuePathsWithOptions(t.Context(), root, QueueOptions{IgnoreFile: QueueIgnoreFileName})
			if err == nil {
				t.Fatalf("expected unsafe entry %q to be rejected, got paths=%v", tc.entry, got)
			}
			if !errors.Is(err, internalerrors.ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput for %q, got %v", tc.entry, err)
			}
			if !strings.Contains(err.Error(), "line 3") {
				t.Fatalf("expected line number in error for %q, got %v", tc.entry, err)
			}
		})
	}
}

func TestGatherQueuePathsWithOptionsHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	root, _ := queueIgnoreFixture(t)
	writeQueueIgnoreFile(t, root, "# valid ignore file\nskip.mkv\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := GatherQueuePathsWithOptions(ctx, root, QueueOptions{IgnoreFile: QueueIgnoreFileName})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got paths=%v err=%v", got, err)
	}
}
