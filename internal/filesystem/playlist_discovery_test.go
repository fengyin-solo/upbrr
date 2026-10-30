// Copyright (c) 2025-2026, Audionut and the upbrr contributors.
// SPDX-License-Identifier: GPL-2.0-or-later

package filesystem

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalerrors "github.com/autobrr/upbrr/internal/errors"
)

const (
	testGiB = int64(1) << 30
)

// bdmvTree creates BDMV/PLAYLIST and BDMV/STREAM under root and returns the
// BDMV directory.
func bdmvTree(t *testing.T, root string) string {
	t.Helper()
	bdmv := filepath.Join(root, "BDMV")
	if err := os.MkdirAll(filepath.Join(bdmv, "PLAYLIST"), 0o700); err != nil {
		t.Fatalf("mkdir PLAYLIST: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(bdmv, "STREAM"), 0o700); err != nil {
		t.Fatalf("mkdir STREAM: %v", err)
	}
	return bdmv
}

// buildValidMPLS encodes a minimal one-item MPLS playlist whose single clip
// starts at zero and lasts durationSeconds at 45kHz.
func buildValidMPLS(clip string, durationSeconds uint32) []byte {
	var buf bytes.Buffer

	header := make([]byte, 32)
	copy(header[0:4], "MPLS")
	copy(header[4:8], "0300")
	binary.BigEndian.PutUint32(header[8:12], 32) // PlaylistStartAddress
	buf.Write(header)

	binary.Write(&buf, binary.BigEndian, uint32(28)) // playlist length (presence only)
	buf.Write(make([]byte, 2))                       // reserved
	binary.Write(&buf, binary.BigEndian, uint16(1))  // nb_play_items
	binary.Write(&buf, binary.BigEndian, uint16(0))  // nb_sub_paths

	binary.Write(&buf, binary.BigEndian, uint16(20)) // item length
	clipBytes := make([]byte, 5)
	copy(clipBytes, clip)
	buf.Write(clipBytes)
	buf.Write([]byte("M2TS"))  // codec identifier
	buf.Write(make([]byte, 2)) // misc flags
	buf.Write([]byte{0})       // ref_to_stc_id
	binary.Write(&buf, binary.BigEndian, uint32(0))
	binary.Write(&buf, binary.BigEndian, uint32(durationSeconds*45000))

	return buf.Bytes()
}

func writePlaylistFile(t *testing.T, bdmv, name string, payload []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(bdmv, "PLAYLIST", name), payload, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func writeValidPlaylistWithStream(t *testing.T, bdmv, name, clip string, durationSeconds uint32, streamSize int64) {
	t.Helper()
	stream, err := os.Create(filepath.Join(bdmv, "STREAM", clip+".m2ts"))
	if err != nil {
		t.Fatalf("create stream %s: %v", clip, err)
	}
	if err := stream.Truncate(streamSize); err != nil {
		t.Fatalf("truncate stream %s: %v", clip, err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream %s: %v", clip, err)
	}
	writePlaylistFile(t, bdmv, name, buildValidMPLS(clip, durationSeconds))
}

func badSignatureMPLS() []byte {
	payload := make([]byte, 32)
	copy(payload[0:4], "XXXX")
	copy(payload[4:8], "0300")
	return payload
}

func truncatedPlayItemsMPLS() []byte {
	header := make([]byte, 32)
	copy(header[0:4], "MPLS")
	copy(header[4:8], "0300")
	binary.BigEndian.PutUint32(header[8:12], 32)
	payload := append(header, make([]byte, 4)...) // playlist length only
	binary.BigEndian.PutUint32(payload[32:36], 28)
	return payload
}

func assertPathFreeReason(t *testing.T, reason, root string) {
	t.Helper()
	if strings.TrimSpace(reason) == "" {
		t.Fatal("playlist issue reason is empty")
	}
	if strings.Contains(reason, root) || strings.ContainsRune(reason, filepath.Separator) {
		t.Fatalf("playlist issue reason leaks filesystem path: %q", reason)
	}
}

func issueReason(report PlaylistReport, file string) string {
	for _, issue := range report.Issues {
		if issue.File == file {
			return issue.Reason
		}
	}
	return ""
}

func playlistNamesOf(report PlaylistReport) []string {
	names := make([]string, 0, len(report.Playlists))
	for _, playlist := range report.Playlists {
		names = append(names, playlist.File)
	}
	return names
}

func issueFileStageMap(report PlaylistReport) map[string]PlaylistFailureStage {
	issues := make(map[string]PlaylistFailureStage, len(report.Issues))
	for _, issue := range report.Issues {
		issues[issue.File] = issue.Stage
	}
	return issues
}

func TestListPlaylistsIsolatesCorruptionAndContinuesScoring(t *testing.T) {
	t.Parallel()

	bdmv := bdmvTree(t, t.TempDir())
	writeValidPlaylistWithStream(t, bdmv, "00001.MPLS", "00001", 100, testGiB)
	writePlaylistFile(t, bdmv, "00002.MPLS", badSignatureMPLS())
	writeValidPlaylistWithStream(t, bdmv, "00003.MPLS", "00003", 4*60*60, 60*testGiB)
	writePlaylistFile(t, bdmv, "00004.MPLS", truncatedPlayItemsMPLS())
	writeValidPlaylistWithStream(t, bdmv, "00005.MPLS", "00005", 60*60, 20*testGiB)

	report, err := ListPlaylists(context.Background(), bdmv)
	if err != nil {
		t.Fatalf("ListPlaylists returned error: %v", err)
	}

	wantNames := []string{"00003.MPLS", "00005.MPLS", "00001.MPLS"}
	if got := playlistNamesOf(report); strings.Join(got, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("candidates = %v, want %v", got, wantNames)
	}
	for i := 1; i < len(report.Playlists); i++ {
		if report.Playlists[i-1].Score < report.Playlists[i].Score {
			t.Fatalf("candidates not score ordered: %#v", report.Playlists)
		}
	}

	if len(report.Issues) != 2 {
		t.Fatalf("issues = %#v, want 2 entries", report.Issues)
	}
	if got := []string{report.Issues[0].File, report.Issues[1].File}; got[0] != "00002.MPLS" || got[1] != "00004.MPLS" {
		t.Fatalf("issues not filename ordered: %v", got)
	}
	stages := issueFileStageMap(report)
	if stages["00002.MPLS"] != PlaylistFailureHeader {
		t.Fatalf("00002 stage = %q, want header", stages["00002.MPLS"])
	}
	if stages["00004.MPLS"] != PlaylistFailurePlayItems {
		t.Fatalf("00004 stage = %q, want play_items", stages["00004.MPLS"])
	}
	for _, issue := range report.Issues {
		assertPathFreeReason(t, issue.Reason, bdmv)
	}
	if report.Issues[0].Reason != "invalid MPLS signature" {
		t.Fatalf("header reason = %q", report.Issues[0].Reason)
	}
	if !strings.Contains(report.Issues[1].Reason, "unexpected end of file") {
		t.Fatalf("play item reason = %q", report.Issues[1].Reason)
	}
}

func TestListPlaylistsAllCorruptReturnsDiagnosticsWithoutError(t *testing.T) {
	t.Parallel()

	bdmv := bdmvTree(t, t.TempDir())
	target := filepath.Join(t.TempDir(), "missing-playlist-target")
	if err := os.Symlink(target, filepath.Join(bdmv, "PLAYLIST", "00001.MPLS")); err != nil {
		t.Skipf("symlink unsupported on this system: %v", err)
	}
	writePlaylistFile(t, bdmv, "00002.MPLS", badSignatureMPLS())
	writePlaylistFile(t, bdmv, "00003.MPLS", truncatedPlayItemsMPLS())
	// A structurally valid playlist whose STREAM association cannot be read.
	if err := os.RemoveAll(filepath.Join(bdmv, "STREAM")); err != nil {
		t.Fatalf("remove STREAM: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bdmv, "STREAM"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("replace STREAM with file: %v", err)
	}
	writePlaylistFile(t, bdmv, "00004.MPLS", buildValidMPLS("00004", 100))

	report, err := ListPlaylists(context.Background(), bdmv)
	if err != nil {
		t.Fatalf("all-corrupt listing returned error: %v", err)
	}
	if len(report.Playlists) != 0 {
		t.Fatalf("expected no candidates, got %#v", report.Playlists)
	}
	if len(report.Issues) != 4 {
		t.Fatalf("issues = %#v, want 4 entries", report.Issues)
	}
	wantFiles := []string{"00001.MPLS", "00002.MPLS", "00003.MPLS", "00004.MPLS"}
	for i, want := range wantFiles {
		if report.Issues[i].File != want {
			t.Fatalf("issue %d = %q, want %q (issues=%#v)", i, report.Issues[i].File, want, report.Issues)
		}
		assertPathFreeReason(t, report.Issues[i].Reason, bdmv)
	}
	stages := issueFileStageMap(report)
	wantStages := map[string]PlaylistFailureStage{
		"00001.MPLS": PlaylistFailureOpen,
		"00002.MPLS": PlaylistFailureHeader,
		"00003.MPLS": PlaylistFailurePlayItems,
		"00004.MPLS": PlaylistFailureStreamFiles,
	}
	for file, wantStage := range wantStages {
		if stages[file] != wantStage {
			t.Fatalf("%s stage = %q, want %q (issues=%#v)", file, stages[file], wantStage, report.Issues)
		}
	}
	if !strings.Contains(issueReason(report, "00004.MPLS"), "not a directory") {
		t.Fatalf("stream association reason = %q", issueReason(report, "00004.MPLS"))
	}
}

func TestListPlaylistsEmptyAndMissingLocationsReturnNotFound(t *testing.T) {
	t.Parallel()

	t.Run("empty playlist directory", func(t *testing.T) {
		t.Parallel()
		bdmv := bdmvTree(t, t.TempDir())
		// Non-MPLS entries are ignored and must not count as diagnostics.
		if err := os.WriteFile(filepath.Join(bdmv, "PLAYLIST", "readme.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ListPlaylists(context.Background(), bdmv)
		if !errors.Is(err, internalerrors.ErrNotFound) {
			t.Fatalf("empty dir err = %v, want ErrNotFound", err)
		}
	})

	t.Run("missing bdmv root", func(t *testing.T) {
		t.Parallel()
		_, err := ListPlaylists(context.Background(), filepath.Join(t.TempDir(), "missing"))
		if !errors.Is(err, internalerrors.ErrNotFound) {
			t.Fatalf("missing root err = %v, want ErrNotFound", err)
		}
	})

	t.Run("empty path", func(t *testing.T) {
		t.Parallel()
		_, err := ListPlaylists(context.Background(), "")
		if !errors.Is(err, internalerrors.ErrInvalidInput) {
			t.Fatalf("empty path err = %v, want ErrInvalidInput", err)
		}
	})

	t.Run("playlist path is a file", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		bdmv := filepath.Join(root, "BDMV")
		if err := os.MkdirAll(bdmv, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bdmv, "PLAYLIST"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ListPlaylists(context.Background(), bdmv)
		if !errors.Is(err, internalerrors.ErrInvalidInput) {
			t.Fatalf("file-as-dir err = %v, want ErrInvalidInput", err)
		}
	})
}

func TestListPlaylistsCanceledReturnsImmediately(t *testing.T) {
	t.Parallel()

	bdmv := bdmvTree(t, t.TempDir())
	writeValidPlaylistWithStream(t, bdmv, "00001.MPLS", "00001", 100, testGiB)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := ListPlaylists(ctx, bdmv)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled listing err = %v, want context.Canceled", err)
	}
	if len(report.Playlists) != 0 || len(report.Issues) != 0 {
		t.Fatalf("canceled listing returned partial results: %#v", report)
	}
}

func TestListPlaylistsKeepsStableOrderForEqualScores(t *testing.T) {
	t.Parallel()

	bdmv := bdmvTree(t, t.TempDir())
	writeValidPlaylistWithStream(t, bdmv, "00001.MPLS", "00001", 100, testGiB)
	writeValidPlaylistWithStream(t, bdmv, "00002.MPLS", "00001", 100, testGiB)
	writeValidPlaylistWithStream(t, bdmv, "00003.MPLS", "00001", 100, testGiB)
	writeValidPlaylistWithStream(t, bdmv, "00004.MPLS", "00004", 4*60*60, 60*testGiB)

	report, err := ListPlaylists(context.Background(), bdmv)
	if err != nil {
		t.Fatalf("ListPlaylists returned error: %v", err)
	}
	wantNames := []string{"00004.MPLS", "00001.MPLS", "00002.MPLS", "00003.MPLS"}
	if got := playlistNamesOf(report); strings.Join(got, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("candidates = %v, want %v", got, wantNames)
	}
	equalScore := report.Playlists[1].Score
	for _, playlist := range report.Playlists[1:] {
		if playlist.Score != equalScore {
			t.Fatalf("tie group scores diverged: %#v", report.Playlists)
		}
	}
}

func TestParseMPLSStagedClassifiesFailureStages(t *testing.T) {
	t.Parallel()

	t.Run("open", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		_, _, stageErr := parseMPLSStaged(filepath.Join(root, "missing.MPLS"))
		if stageErr == nil || stageErr.stage != PlaylistFailureOpen {
			t.Fatalf("stageErr = %#v, want open", stageErr)
		}
		assertPathFreeReason(t, stageErr.reason, root)
	})

	t.Run("header signature", func(t *testing.T) {
		t.Parallel()
		bdmv := bdmvTree(t, t.TempDir())
		writePlaylistFile(t, bdmv, "00001.MPLS", badSignatureMPLS())
		_, _, stageErr := parseMPLSStaged(filepath.Join(bdmv, "PLAYLIST", "00001.MPLS"))
		if stageErr == nil || stageErr.stage != PlaylistFailureHeader {
			t.Fatalf("stageErr = %#v, want header", stageErr)
		}
		if stageErr.reason != "invalid MPLS signature" {
			t.Fatalf("reason = %q", stageErr.reason)
		}
	})

	t.Run("header truncation", func(t *testing.T) {
		t.Parallel()
		bdmv := bdmvTree(t, t.TempDir())
		// A zero-byte file makes the header read reach EOF; ordinary short
		// reads return without an error, so only emptiness forces the stage.
		writePlaylistFile(t, bdmv, "00001.MPLS", nil)
		_, _, stageErr := parseMPLSStaged(filepath.Join(bdmv, "PLAYLIST", "00001.MPLS"))
		if stageErr == nil || stageErr.stage != PlaylistFailureHeader {
			t.Fatalf("stageErr = %#v, want header", stageErr)
		}
		if !strings.Contains(stageErr.reason, "unexpected end of file") {
			t.Fatalf("reason = %q", stageErr.reason)
		}
	})

	t.Run("play items", func(t *testing.T) {
		t.Parallel()
		bdmv := bdmvTree(t, t.TempDir())
		writePlaylistFile(t, bdmv, "00001.MPLS", truncatedPlayItemsMPLS())
		_, _, stageErr := parseMPLSStaged(filepath.Join(bdmv, "PLAYLIST", "00001.MPLS"))
		if stageErr == nil || stageErr.stage != PlaylistFailurePlayItems {
			t.Fatalf("stageErr = %#v, want play_items", stageErr)
		}
		if !strings.Contains(stageErr.reason, "unexpected end of file") {
			t.Fatalf("reason = %q", stageErr.reason)
		}
	})

	t.Run("stream files", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		bdmv := bdmvTree(t, root)
		if err := os.RemoveAll(filepath.Join(bdmv, "STREAM")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bdmv, "STREAM"), []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		writePlaylistFile(t, bdmv, "00001.MPLS", buildValidMPLS("00001", 100))
		_, _, stageErr := parseMPLSStaged(filepath.Join(bdmv, "PLAYLIST", "00001.MPLS"))
		if stageErr == nil || stageErr.stage != PlaylistFailureStreamFiles {
			t.Fatalf("stageErr = %#v, want stream_files", stageErr)
		}
		assertPathFreeReason(t, stageErr.reason, root)
	})
}

func TestDiscoverPlaylistsFailFastBehaviorUnchanged(t *testing.T) {
	t.Parallel()

	t.Run("aborts on first corrupt file", func(t *testing.T) {
		t.Parallel()
		bdmv := bdmvTree(t, t.TempDir())
		writeValidPlaylistWithStream(t, bdmv, "00001.MPLS", "00001", 100, testGiB)
		writePlaylistFile(t, bdmv, "00002.MPLS", badSignatureMPLS())
		writeValidPlaylistWithStream(t, bdmv, "00003.MPLS", "00003", 200, 2*testGiB)

		playlists, err := DiscoverPlaylists(context.Background(), bdmv)
		if err == nil {
			t.Fatal("DiscoverPlaylists unexpectedly succeeded")
		}
		if playlists != nil {
			t.Fatalf("DiscoverPlaylists returned partial playlists: %#v", playlists)
		}
		if !strings.Contains(err.Error(), `parse "00002.MPLS"`) {
			t.Fatalf("error = %q, want parse failure for 00002.MPLS", err.Error())
		}

		// ParseMPLS keeps its legacy wrapped error message.
		if _, _, parseErr := func() (float64, []PlaylistItem, error) {
			return ParseMPLS(filepath.Join(bdmv, "PLAYLIST", "00002.MPLS"))
		}(); !strings.Contains(parseErr.Error(), "failed to load movie playlist header") {
			t.Fatalf("ParseMPLS error = %q", parseErr.Error())
		}
	})

	t.Run("valid directory stays score ordered", func(t *testing.T) {
		t.Parallel()
		bdmv := bdmvTree(t, t.TempDir())
		writeValidPlaylistWithStream(t, bdmv, "00001.MPLS", "00001", 100, testGiB)
		writeValidPlaylistWithStream(t, bdmv, "00002.MPLS", "00002", 4*60*60, 60*testGiB)

		playlists, err := DiscoverPlaylists(context.Background(), bdmv)
		if err != nil {
			t.Fatalf("DiscoverPlaylists returned error: %v", err)
		}
		if len(playlists) != 2 || playlists[0].File != "00002.MPLS" || playlists[1].File != "00001.MPLS" {
			t.Fatalf("playlists = %#v", playlists)
		}
	})

	t.Run("empty directory is not found", func(t *testing.T) {
		t.Parallel()
		bdmv := bdmvTree(t, t.TempDir())
		if _, err := DiscoverPlaylists(context.Background(), bdmv); !errors.Is(err, internalerrors.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("canceled context aborts", func(t *testing.T) {
		t.Parallel()
		bdmv := bdmvTree(t, t.TempDir())
		writeValidPlaylistWithStream(t, bdmv, "00001.MPLS", "00001", 100, testGiB)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := DiscoverPlaylists(ctx, bdmv)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}
