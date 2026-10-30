// Copyright (c) 2025-2026, Audionut and the autobrr contributors.
// SPDX-License-Identifier: GPL-2.0-or-later

package metadata

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/autobrr/upbrr/internal/logging"

	internalerrors "github.com/autobrr/upbrr/internal/errors"
	"github.com/autobrr/upbrr/internal/filesystem"
	"github.com/autobrr/upbrr/internal/metadata/discparse"
	preparationstate "github.com/autobrr/upbrr/internal/preparedrelease/state"
	"github.com/autobrr/upbrr/internal/sourcelayout"
	"github.com/autobrr/upbrr/pkg/api"
)

// resolveBDMVPlaylistSelection resolves one complete selection against every
// discovered BDMV and persists it only after all discs validate. Unreadable
// individual MPLS files are isolated into diagnostics; selection is blocked
// only when a disc offers no usable candidate at all.
func (s *Service) resolveBDMVPlaylistSelection(ctx context.Context, request preparationstate.Request) ([]api.PlaylistInfo, error) {
	discs := request.Layout.Discs
	if len(discs) == 0 {
		return nil, fmt.Errorf("metadata: BDMV resources are unavailable: %w", internalerrors.ErrInvalidInput)
	}
	logger := logging.FromContext(ctx, s.logger)
	candidates := make([]api.PlaylistInfo, 0)
	issues := make([]api.PlaylistIssue, 0)
	for _, disc := range discs {
		if disc.Type != "BDMV" {
			return nil, fmt.Errorf("metadata: invalid BDMV resource set: %w", internalerrors.ErrInvalidInput)
		}
		report, err := discoverBDMVPlaylists(ctx, disc.Root)
		if err != nil {
			return nil, fmt.Errorf("metadata: discover BDMV playlists: %w", err)
		}
		for _, issue := range report.Issues {
			apiIssue := apiPlaylistIssueForDisc(issue, disc)
			issues = append(issues, apiIssue)
			logger.Warnf(
				"metadata: unreadable BDMV playlist disc=%s file=%s stage=%s reason=%s",
				disc.Name,
				apiIssue.File,
				apiIssue.Stage,
				apiIssue.Reason,
			)
		}
		if len(report.Playlists) == 0 {
			if len(report.Issues) > 0 {
				return nil, &api.PlaylistDiscoveryError{
					SourcePath: request.Layout.SourcePath,
					Issues:     append([]api.PlaylistIssue(nil), issues...),
				}
			}
			return nil, fmt.Errorf("metadata: BDMV disc has no discoverable playlists: %w", internalerrors.ErrInvalidInput)
		}
		for _, playlist := range report.Playlists {
			candidates = append(candidates, apiPlaylistForDisc(playlist, disc, len(discs) == 1))
		}
	}

	instruction := request.Input.Instructions.Playlist
	selected := append([]string(nil), instruction.Selected...)
	useAll := instruction.UseAll
	if !instruction.Set {
		stored, err := s.repo.GetPlaylistSelection(ctx, playlistSelectionKey(request.Layout.SourcePath))
		if err != nil {
			if errors.Is(err, internalerrors.ErrNotFound) {
				return nil, playlistSelectionRequired(request, candidates, issues)
			}
			return nil, fmt.Errorf("metadata: load remembered playlist selection: %w", err)
		}
		storedFingerprint := strings.TrimSpace(stored.SourceFingerprint)
		currentFingerprint := strings.TrimSpace(request.SourceFingerprint)
		if storedFingerprint != currentFingerprint && (storedFingerprint != "" || len(discs) != 1) {
			return nil, playlistSelectionRequired(request, candidates, issues)
		}
		selected = append([]string(nil), stored.SelectedPlaylists...)
		useAll = stored.UseAll
	}

	resolved, err := resolvePlaylistIDs(request, candidates, issues, selected, useAll)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(resolved))
	for i := range resolved {
		ids[i] = resolved[i].ID
	}
	if err := s.repo.SavePlaylistSelection(
		ctx,
		playlistSelectionKey(request.Layout.SourcePath),
		strings.TrimSpace(request.SourceFingerprint),
		ids,
		useAll,
	); err != nil {
		return nil, fmt.Errorf("metadata: save playlist selection: %w", err)
	}
	return resolved, nil
}

func resolvePlaylistIDs(
	request preparationstate.Request,
	candidates []api.PlaylistInfo,
	issues []api.PlaylistIssue,
	selected []string,
	useAll bool,
) ([]api.PlaylistInfo, error) {
	if useAll {
		return append([]api.PlaylistInfo(nil), candidates...), nil
	}
	if len(selected) == 0 {
		return nil, playlistSelectionRequired(request, candidates, issues)
	}

	byID := make(map[string]api.PlaylistInfo, len(candidates))
	for _, candidate := range candidates {
		if candidate.ID == "" {
			return nil, fmt.Errorf("metadata: discovered playlist has no ID: %w", internalerrors.ErrInvalidInput)
		}
		if _, exists := byID[candidate.ID]; exists {
			return nil, fmt.Errorf("metadata: duplicate playlist ID: %w", internalerrors.ErrInvalidInput)
		}
		byID[candidate.ID] = candidate
	}

	selectedIDs := make(map[string]struct{}, len(selected))
	selectedDiscs := make(map[string]struct{}, len(request.Layout.Discs))
	for _, raw := range selected {
		id, err := decodePlaylistSelectionID(raw, candidates, len(request.Layout.Discs) == 1)
		if err != nil {
			return nil, &api.InvalidPlaylistSelectionError{
				SourcePath: request.Layout.SourcePath,
				Playlist:   strings.TrimSpace(raw),
				Reason:     err.Error(),
			}
		}
		if _, exists := selectedIDs[id]; exists {
			continue
		}
		selectedIDs[id] = struct{}{}
		selectedDiscs[byID[id].DiscID] = struct{}{}
	}
	if len(selectedDiscs) != len(request.Layout.Discs) {
		return nil, &api.InvalidPlaylistSelectionError{
			SourcePath: request.Layout.SourcePath,
			Reason:     "at least one playlist must be selected for every disc",
		}
	}

	result := make([]api.PlaylistInfo, 0, len(selectedIDs))
	for _, candidate := range candidates {
		if _, selected := selectedIDs[candidate.ID]; selected {
			result = append(result, candidate)
		}
	}
	return result, nil
}

func decodePlaylistSelectionID(raw string, candidates []api.PlaylistInfo, singleDisc bool) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("playlist ID is empty")
	}
	if !singleDisc {
		for _, candidate := range candidates {
			if candidate.ID == trimmed {
				return candidate.ID, nil
			}
		}
		return "", errors.New("playlist ID was not found in the current source")
	}

	normalized, err := validateLegacyPlaylistName(trimmed)
	if err != nil {
		return "", err
	}
	for _, candidate := range candidates {
		if strings.EqualFold(candidate.ID, normalized) {
			return candidate.ID, nil
		}
	}
	return "", errors.New("playlist was not found in the current source")
}

// validateLegacyPlaylistName is the only compatibility decoder that treats a
// selection as a one-disc MPLS basename.
func validateLegacyPlaylistName(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if filepath.IsAbs(trimmed) || filepath.VolumeName(trimmed) != "" || strings.ContainsAny(trimmed, `/\\`) || trimmed == "." || trimmed == ".." {
		return "", errors.New("playlist name must be a local filename")
	}
	normalized := discparse.NormalizePlaylistName(trimmed)
	if normalized == "" || !strings.EqualFold(filepath.Ext(normalized), ".MPLS") {
		return "", errors.New("playlist name is invalid")
	}
	return normalized, nil
}

func playlistSelectionRequired(request preparationstate.Request, candidates []api.PlaylistInfo, issues []api.PlaylistIssue) error {
	return &api.PlaylistSelectionRequiredError{
		SourcePath: request.Layout.SourcePath,
		Candidates: append([]api.PlaylistInfo(nil), candidates...),
		Issues:     append([]api.PlaylistIssue(nil), issues...),
	}
}

// playlistSelectionKey preserves the repository's slash-normalized source key.
func playlistSelectionKey(sourcePath string) string {
	return filepath.ToSlash(filepath.Clean(sourcePath))
}

func apiPlaylistForDisc(value filesystem.PlaylistInfo, disc sourcelayout.DiscResource, singleDisc bool) api.PlaylistInfo {
	items := make([]api.PlaylistItem, 0, len(value.Items))
	for _, item := range value.Items {
		items = append(items, api.PlaylistItem{File: item.File, Size: item.Size})
	}
	file := discparse.NormalizePlaylistName(value.File)
	id := file
	if !singleDisc {
		id = disc.ID + ":" + file
	}
	return api.PlaylistInfo{
		ID:       id,
		DiscID:   disc.ID,
		DiscName: disc.Name,
		File:     file,
		Duration: value.Duration,
		Items:    items,
		Score:    value.Score,
		Edition:  value.Edition,
	}
}

// apiPlaylistIssueForDisc scopes one filesystem-level playlist issue to its
// owning disc. The on-disk basename is preserved verbatim so the corrupt
// entry can be located exactly.
func apiPlaylistIssueForDisc(value filesystem.PlaylistIssue, disc sourcelayout.DiscResource) api.PlaylistIssue {
	return api.PlaylistIssue{
		DiscID:   disc.ID,
		DiscName: disc.Name,
		File:     value.File,
		Stage:    string(value.Stage),
		Reason:   value.Reason,
	}
}
