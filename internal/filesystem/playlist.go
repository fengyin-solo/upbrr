// Copyright (c) 2025-2026, Audionut and the autobrr contributors.
// SPDX-License-Identifier: GPL-2.0-or-later

package filesystem

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	internalerrors "github.com/autobrr/upbrr/internal/errors"
)

// PlaylistInfo describes one MPLS candidate. File and item names are basenames,
// Duration is seconds, item sizes are bytes, and Score ranks main-feature
// likelihood. Edition is currently left empty by discovery.
type PlaylistInfo struct {
	File     string
	Duration float64
	Items    []PlaylistItem
	Score    float64
	Edition  string
}

// PlaylistItem identifies one referenced stream basename and its size in bytes.
type PlaylistItem struct {
	File string
	Size int64
}

// PlaylistFailureStage locates where parsing one MPLS file failed.
type PlaylistFailureStage string

const (
	// PlaylistFailureOpen means the MPLS file itself could not be opened.
	PlaylistFailureOpen PlaylistFailureStage = "open"
	// PlaylistFailureHeader means the MPLS header could not be read or validated.
	PlaylistFailureHeader PlaylistFailureStage = "header"
	// PlaylistFailurePlayItems means the playlist play items could not be read.
	PlaylistFailurePlayItems PlaylistFailureStage = "play_items"
	// PlaylistFailureStreamFiles means referenced stream files could not be associated.
	PlaylistFailureStreamFiles PlaylistFailureStage = "stream_files"
)

// PlaylistIssue describes one MPLS file that could not be parsed. File is the
// playlist basename, Stage locates the failure, and Reason is a readable
// explanation that never contains absolute filesystem paths.
type PlaylistIssue struct {
	File   string
	Stage  PlaylistFailureStage
	Reason string
}

// PlaylistReport is the diagnostic playlist listing returned by ListPlaylists:
// valid score-ranked Playlists plus filename-sorted Issues for MPLS files that
// could not be parsed.
type PlaylistReport struct {
	Playlists []PlaylistInfo
	Issues    []PlaylistIssue
}

// DiscoverPlaylists discovers and scores all MPLS files in a BDMV folder. It
// returns candidates ordered by descending score. The first unreadable MPLS
// aborts discovery; callers that need per-file isolation and diagnostics
// should use ListPlaylists instead.
func DiscoverPlaylists(ctx context.Context, bdmvRoot string) ([]PlaylistInfo, error) {
	playlistDir, entries, err := resolvePlaylistDirectory(bdmvRoot)
	if err != nil {
		return nil, err
	}

	var playlists []PlaylistInfo
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context canceled: %w", ctx.Err())
		default:
		}
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if !strings.EqualFold(filepath.Ext(name), ".MPLS") {
			continue
		}

		mpslPath := filepath.Join(playlistDir, name)
		duration, items, err := ParseMPLS(mpslPath)
		if err != nil {
			return nil, fmt.Errorf("playlist: parse %q: %w", name, err)
		}

		// Calculate playlist score using weighted algorithm.
		score := calculatePlaylistScore(duration, items)

		playlists = append(playlists, PlaylistInfo{
			File:     name,
			Duration: duration,
			Items:    items,
			Score:    score,
			Edition:  "",
		})
	}

	if len(playlists) == 0 {
		return nil, fmt.Errorf("playlist: no MPLS files found in %q: %w", playlistDir, internalerrors.ErrNotFound)
	}

	// Sort by score descending (highest score first).
	sort.Slice(playlists, func(i, j int) bool {
		return playlists[i].Score > playlists[j].Score
	})

	return playlists, nil
}

// ListPlaylists discovers and scores every readable MPLS file in a BDMV
// folder, isolating per-file parse failures instead of aborting the whole
// directory. It returns valid playlists ordered by descending score (filename
// order preserved for ties) and unreadable files ordered by filename. When
// every MPLS file is unreadable the report carries no playlists but complete
// diagnostics and a nil error; a missing PLAYLIST directory or one without
// MPLS files still returns an ErrNotFound error.
func ListPlaylists(ctx context.Context, bdmvRoot string) (PlaylistReport, error) {
	if err := ctx.Err(); err != nil {
		return PlaylistReport{}, fmt.Errorf("context canceled: %w", err)
	}

	playlistDir, entries, err := resolvePlaylistDirectory(bdmvRoot)
	if err != nil {
		return PlaylistReport{}, err
	}

	report := PlaylistReport{}
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return PlaylistReport{}, fmt.Errorf("context canceled: %w", ctx.Err())
		default:
		}
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if !strings.EqualFold(filepath.Ext(name), ".MPLS") {
			continue
		}

		duration, items, stageErr := parseMPLSStaged(filepath.Join(playlistDir, name))
		if stageErr != nil {
			report.Issues = append(report.Issues, PlaylistIssue{
				File:   name,
				Stage:  stageErr.stage,
				Reason: stageErr.reason,
			})
			continue
		}

		// Calculate playlist score using the same weighted algorithm as discovery.
		score := calculatePlaylistScore(duration, items)

		report.Playlists = append(report.Playlists, PlaylistInfo{
			File:     name,
			Duration: duration,
			Items:    items,
			Score:    score,
			Edition:  "",
		})
	}

	if len(report.Playlists) == 0 && len(report.Issues) == 0 {
		return PlaylistReport{}, fmt.Errorf("playlist: no MPLS files found in %q: %w", playlistDir, internalerrors.ErrNotFound)
	}

	// Stable score ordering keeps equal-score playlists in filename order.
	sort.SliceStable(report.Playlists, func(i, j int) bool {
		return report.Playlists[i].Score > report.Playlists[j].Score
	})
	sort.SliceStable(report.Issues, func(i, j int) bool {
		return report.Issues[i].File < report.Issues[j].File
	})

	return report, nil
}

// resolvePlaylistDirectory validates bdmvRoot and returns its PLAYLIST
// directory together with the sorted directory entries.
func resolvePlaylistDirectory(bdmvRoot string) (string, []os.DirEntry, error) {
	if bdmvRoot == "" {
		return "", nil, fmt.Errorf("playlist: empty path: %w", internalerrors.ErrInvalidInput)
	}

	playlistDir := filepath.Join(bdmvRoot, "PLAYLIST")
	info, err := os.Stat(playlistDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, fmt.Errorf("playlist: directory %q not found: %w", playlistDir, internalerrors.ErrNotFound)
		}
		return "", nil, fmt.Errorf("playlist: stat %q: %w", playlistDir, err)
	}
	if !info.IsDir() {
		return "", nil, fmt.Errorf("playlist: %q is not a directory: %w", playlistDir, internalerrors.ErrInvalidInput)
	}

	entries, err := os.ReadDir(playlistDir)
	if err != nil {
		return "", nil, fmt.Errorf("playlist: read directory %q: %w", playlistDir, err)
	}
	return playlistDir, entries, nil
}

// calculatePlaylistScore computes a weighted score for playlist ranking.
// Matches Python algorithm: largest file 40%, total size 30%, duration 20%, file concentration 10%.
func calculatePlaylistScore(duration float64, items []PlaylistItem) float64 {
	if len(items) == 0 {
		return 0.0
	}

	// Normalize metrics.
	maxFileSize := 100.0 * 1024 * 1024 * 1024  // 100 GB
	maxTotalSize := 150.0 * 1024 * 1024 * 1024 // 150 GB
	maxDuration := 14400.0                     // 4 hours in seconds

	// Deduplicate by filename so that looping playlists (which repeat a small set
	// of files many times) don't inflate the largest-file and total-size metrics.
	uniqueSizes := make(map[string]int64)
	for _, item := range items {
		if sz, seen := uniqueSizes[item.File]; !seen || item.Size > sz {
			uniqueSizes[item.File] = item.Size
		}
	}

	var largestFile int64
	var totalSize int64
	for _, sz := range uniqueSizes {
		totalSize += sz
		if sz > largestFile {
			largestFile = sz
		}
	}

	// File concentration: unique files / total references (with duplicates).
	// Looping playlists have many references to few files → low concentration.
	fileConcentration := float64(len(uniqueSizes)) / float64(len(items))

	// Compute weighted score.
	score := 0.0
	score += math.Min(float64(largestFile)/maxFileSize, 1.0) * 40.0
	score += math.Min(float64(totalSize)/maxTotalSize, 1.0) * 30.0
	score += math.Min(duration/maxDuration, 1.0) * 20.0
	score += fileConcentration * 10.0

	return score
}

// ParseMPLS returns an MPLS playlist's duration in seconds and referenced M2TS
// basenames with stream sizes in bytes.
func ParseMPLS(mpslPath string) (float64, []PlaylistItem, error) {
	duration, items, stageErr := parseMPLSStaged(mpslPath)
	if stageErr != nil {
		return 0, nil, stageErr.err
	}
	return duration, items, nil
}

// playlistStageError pairs a parse failure with its diagnostic stage and a
// path-free readable reason while preserving the legacy wrapped message used
// by ParseMPLS.
type playlistStageError struct {
	stage  PlaylistFailureStage
	reason string
	err    error
}

func (e *playlistStageError) Error() string {
	return e.err.Error()
}

func (e *playlistStageError) Unwrap() error {
	return e.err
}

func newPlaylistStageError(stage PlaylistFailureStage, reason string, err error) *playlistStageError {
	return &playlistStageError{
		stage:  stage,
		reason: reason,
		err:    err,
	}
}

// parseMPLSStaged parses one MPLS file, tagging every failure with the stage
// it occurred in. The wrapped error text stays compatible with ParseMPLS.
func parseMPLSStaged(mpslPath string) (float64, []PlaylistItem, *playlistStageError) {
	file, err := os.Open(mpslPath)
	if err != nil {
		return 0, nil, newPlaylistStageError(
			PlaylistFailureOpen,
			"cannot open playlist file: "+pathFreePlaylistCause(err),
			fmt.Errorf("open MPLS playlist: %w", err),
		)
	}
	defer file.Close()

	// Validate MPLS signature and get header.
	header, err := loadMoviePlaylist(file)
	if err != nil {
		return 0, nil, newPlaylistStageError(
			PlaylistFailureHeader,
			playlistHeaderFailureReason(err),
			fmt.Errorf("failed to load movie playlist header: %w", err),
		)
	}

	// Seek to playlist start and load the playlist structure.
	if _, err := file.Seek(int64(header.PlaylistStartAddress), io.SeekStart); err != nil {
		return 0, nil, newPlaylistStageError(
			PlaylistFailurePlayItems,
			"cannot read playlist play items: "+pathFreePlaylistCause(err),
			fmt.Errorf("failed to seek to playlist: %w", err),
		)
	}

	_, playlistItems, err := loadPlaylist(file)
	if err != nil {
		return 0, nil, newPlaylistStageError(
			PlaylistFailurePlayItems,
			"cannot read playlist play items: "+pathFreePlaylistCause(err),
			fmt.Errorf("failed to load playlist: %w", err),
		)
	}

	// Calculate total duration from play items (intime/outtime at 45kHz).
	const frequencyHz = 45000.0
	var totalDuration float64
	for _, item := range playlistItems {
		itemDuration := float64(item.OutTime-item.InTime) / frequencyHz
		totalDuration += itemDuration
	}

	// Build PlaylistItem list from clip names.
	clipNames := make([]PlaylistItem, 0, len(playlistItems))
	for _, item := range playlistItems {
		clipNames = append(clipNames, PlaylistItem{
			File: item.ClipName + ".m2ts",
			Size: 0, // Will be populated by attachStreamSizes.
		})
	}

	// Get file sizes from STREAM directory.
	itemsWithSizes, err := attachStreamSizes(filepath.Dir(mpslPath), clipNames)
	if err != nil {
		return 0, nil, newPlaylistStageError(
			PlaylistFailureStreamFiles,
			"cannot associate referenced stream files: "+pathFreePlaylistCause(err),
			err,
		)
	}

	return totalDuration, itemsWithSizes, nil
}

// playlistHeaderFailureReason renders header failures without raw signature
// bytes (which may be non-printable) or absolute paths.
func playlistHeaderFailureReason(err error) string {
	if strings.Contains(err.Error(), "invalid MPLS signature") {
		return "invalid MPLS signature"
	}
	return "cannot read MPLS header: " + pathFreePlaylistCause(err)
}

// pathFreePlaylistCause renders an MPLS parse cause as a stable readable
// string without absolute filesystem paths.
func pathFreePlaylistCause(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected end of file"
	}
	if pathErr, ok := errors.AsType[*os.PathError](err); ok {
		return pathErr.Err.Error()
	}
	if linkErr, ok := errors.AsType[*os.LinkError](err); ok {
		return linkErr.Err.Error()
	}
	return strings.TrimSpace(err.Error())
}

// MoviePlaylistHeader represents the MPLS file header.
type MoviePlaylistHeader struct {
	TypeIndicator             string
	VersionNumber             string
	PlaylistStartAddress      uint32
	PlaylistMarkStartAddress  uint32
	ExtensionDataStartAddress uint32
}

// PlaylistInfo represents a parsed playlist structure.
type PlaylistData struct {
	Length      uint32
	NbPlayItems uint16
	NbSubPaths  uint16
	PlayItems   []PlaylistItem
}

// PlaylistItemInfo represents a single play item in a playlist with timing info.
type PlaylistItemInfo struct {
	ClipName string
	InTime   uint32
	OutTime  uint32
}

// loadMoviePlaylist loads and parses the MPLS file header.
func loadMoviePlaylist(f *os.File) (*MoviePlaylistHeader, error) {
	header := make([]byte, 32)
	if _, err := f.Read(header); err != nil {
		return nil, fmt.Errorf("failed to read MPLS header: %w", err)
	}

	typeIndicator := string(header[0:4])
	if typeIndicator != "MPLS" {
		return nil, fmt.Errorf("invalid MPLS signature: %s", typeIndicator)
	}

	versionNumber := string(header[4:8])
	playlistStartAddr := binary.BigEndian.Uint32(header[8:12])
	playlistMarkStartAddr := binary.BigEndian.Uint32(header[12:16])
	extensionDataStartAddr := binary.BigEndian.Uint32(header[16:20])

	return &MoviePlaylistHeader{
		TypeIndicator:             typeIndicator,
		VersionNumber:             versionNumber,
		PlaylistStartAddress:      playlistStartAddr,
		PlaylistMarkStartAddress:  playlistMarkStartAddr,
		ExtensionDataStartAddress: extensionDataStartAddr,
	}, nil
}

// loadPlaylist loads and parses the playlist structure at the current file position.
// Returns both the playlist metadata and the detailed play items with timing info.
func loadPlaylist(f *os.File) (*PlaylistData, []PlaylistItemInfo, error) {
	// Read length (4 bytes, big-endian).
	lengthBytes := make([]byte, 4)
	if _, err := f.Read(lengthBytes); err != nil {
		return nil, nil, fmt.Errorf("failed to read playlist length: %w", err)
	}
	length := binary.BigEndian.Uint32(lengthBytes)

	if length == 0 {
		return &PlaylistData{Length: 0}, nil, nil
	}

	// Read reserved (2 bytes).
	reserved := make([]byte, 2)
	if _, err := f.Read(reserved); err != nil {
		return nil, nil, fmt.Errorf("failed to read reserved: %w", err)
	}

	// Read nb_play_items (2 bytes, big-endian).
	nbItemsBytes := make([]byte, 2)
	if _, err := f.Read(nbItemsBytes); err != nil {
		return nil, nil, fmt.Errorf("failed to read nb_play_items: %w", err)
	}
	nbPlayItems := binary.BigEndian.Uint16(nbItemsBytes)

	// Read nb_sub_paths (2 bytes, big-endian).
	nbSubPathsBytes := make([]byte, 2)
	if _, err := f.Read(nbSubPathsBytes); err != nil {
		return nil, nil, fmt.Errorf("failed to read nb_sub_paths: %w", err)
	}
	nbSubPaths := binary.BigEndian.Uint16(nbSubPathsBytes)

	// Load all play items.
	playItems := make([]PlaylistItemInfo, 0, nbPlayItems)
	clipItems := make([]PlaylistItem, 0, nbPlayItems)
	for i := 0; i < int(nbPlayItems); i++ {
		item, err := loadPlayItem(f)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to load play item %d: %w", i, err)
		}
		// Only add if we got a valid clip filename.
		if item != nil && item.ClipName != "" {
			playItems = append(playItems, *item)
			clipItems = append(clipItems, PlaylistItem{
				File: item.ClipName + ".m2ts",
				Size: 0,
			})
		}
	}

	return &PlaylistData{
		Length:      length,
		NbPlayItems: nbPlayItems,
		NbSubPaths:  nbSubPaths,
		PlayItems:   clipItems,
	}, playItems, nil
}

// loadPlayItem loads a single play item from the file, following the Python MplsParser pattern.
func loadPlayItem(f *os.File) (*PlaylistItemInfo, error) {
	// Remember current position for seeking later.
	startPos, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, fmt.Errorf("failed to get file position: %w", err)
	}

	// Read item length (2 bytes, big-endian).
	lengthBytes := make([]byte, 2)
	if _, err := f.Read(lengthBytes); err != nil {
		return nil, fmt.Errorf("failed to read play item length: %w", err)
	}
	itemLength := binary.BigEndian.Uint16(lengthBytes)

	if itemLength == 0 {
		// Seek past this empty item (length itself is 2 bytes).
		_, err := f.Seek(startPos+int64(itemLength)+2, io.SeekStart)
		if err != nil {
			return nil, fmt.Errorf("failed to seek past empty item: %w", err)
		}
		return nil, nil
	}

	// Read clip information filename (5 bytes, UTF-8).
	clipNameBytes := make([]byte, 5)
	if _, err := f.Read(clipNameBytes); err != nil {
		return nil, fmt.Errorf("failed to read clip name: %w", err)
	}
	clipName := strings.TrimSpace(string(clipNameBytes))

	// Read remaining fixed fields.
	codecID := make([]byte, 4)
	if _, err := f.Read(codecID); err != nil {
		return nil, fmt.Errorf("failed to read clip_codec_identifier: %w", err)
	}

	miscFlags := make([]byte, 2)
	if _, err := f.Read(miscFlags); err != nil {
		return nil, fmt.Errorf("failed to read misc_flags_1: %w", err)
	}

	refToStcID := make([]byte, 1)
	if _, err := f.Read(refToStcID); err != nil {
		return nil, fmt.Errorf("failed to read ref_to_stcid: %w", err)
	}

	// Read intime (4 bytes, big-endian).
	inTimeBytes := make([]byte, 4)
	if _, err := f.Read(inTimeBytes); err != nil {
		return nil, fmt.Errorf("failed to read intime: %w", err)
	}
	inTime := binary.BigEndian.Uint32(inTimeBytes)

	// Read outtime (4 bytes, big-endian).
	outTimeBytes := make([]byte, 4)
	if _, err := f.Read(outTimeBytes); err != nil {
		return nil, fmt.Errorf("failed to read outtime: %w", err)
	}
	outTime := binary.BigEndian.Uint32(outTimeBytes)

	// Seek to next item: original_position + itemLength + 2 (Python pattern).
	_, err = f.Seek(startPos+int64(itemLength)+2, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("failed to seek to next item: %w", err)
	}

	return &PlaylistItemInfo{
		ClipName: clipName,
		InTime:   inTime,
		OutTime:  outTime,
	}, nil
}

// attachStreamSizes maps clip files to their actual sizes from the STREAM directory.
func attachStreamSizes(playlistDir string, items []PlaylistItem) ([]PlaylistItem, error) {
	streamDir := filepath.Join(filepath.Dir(playlistDir), "STREAM")

	// Build map of available stream files and their sizes (using lowercase keys for case-insensitive matching).
	streamFiles := make(map[string]int64)
	entries, err := os.ReadDir(streamDir)
	if err != nil {
		if os.IsNotExist(err) {
			return items, nil // No STREAM directory; return items as-is.
		}
		return nil, fmt.Errorf("read BD stream directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.EqualFold(filepath.Ext(name), ".m2ts") {
			info, err := entry.Info()
			if err == nil {
				// Store both original name and lowercase for matching
				streamFiles[name] = info.Size()
				streamFiles[strings.ToLower(name)] = info.Size()
			}
		}
	}

	// Update item sizes from stream files with best-effort matching.
	var result []PlaylistItem
	for _, item := range items {
		// Try exact match first
		if size, ok := streamFiles[item.File]; ok {
			result = append(result, PlaylistItem{
				File: item.File,
				Size: size,
			})
			continue
		}

		// If no match but items list is empty, fall back to all available streams
		if len(result) == 0 {
			// Add item with size 0 for now, we'll fill in all streams as fallback
			result = append(result, PlaylistItem{
				File: item.File,
				Size: 0,
			})
		}
	}

	// If no items matched files, return all available streams instead.
	if len(result) == 0 || (len(result) > 0 && result[0].Size == 0) {
		result = []PlaylistItem{}
		seen := make(map[string]bool)
		for name, size := range streamFiles {
			// Only add each file once (skip the lowercase duplicates we created for matching)
			if !seen[strings.ToLower(name)] && strings.EqualFold(filepath.Ext(name), ".m2ts") {
				result = append(result, PlaylistItem{
					File: name,
					Size: size,
				})
				seen[strings.ToLower(name)] = true
			}
		}
	}

	// Sort results by filename for consistent ordering
	sort.Slice(result, func(i, j int) bool {
		return result[i].File < result[j].File
	})

	return result, nil
}

// FormatDuration truncates fractional seconds and formats the result as
// HH:MM:SS; hours are not capped at 24.
func FormatDuration(seconds float64) string {
	d := time.Duration(seconds * float64(time.Second))
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}
