// Copyright (c) 2025-2026, Audionut and the upbrr contributors.
// SPDX-License-Identifier: GPL-2.0-or-later

package metadata

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	internalerrors "github.com/autobrr/upbrr/internal/errors"
	"github.com/autobrr/upbrr/internal/filesystem"
	preparationstate "github.com/autobrr/upbrr/internal/preparedrelease/state"
	"github.com/autobrr/upbrr/internal/sourcelayout"
	"github.com/autobrr/upbrr/pkg/api"
)

func stubPlaylistReport(playlists []filesystem.PlaylistInfo, issues ...filesystem.PlaylistIssue) filesystem.PlaylistReport {
	return filesystem.PlaylistReport{Playlists: playlists, Issues: issues}
}

func oneGoodPlaylist() []filesystem.PlaylistInfo {
	return []filesystem.PlaylistInfo{{
		File:     "00001.MPLS",
		Duration: 5400,
		Items:    []filesystem.PlaylistItem{{File: "00001.m2ts", Size: 100}},
		Score:    75,
	}}
}

func diagnosticBDMVLayout(sourcePath string, discIDs ...string) sourcelayout.Layout {
	discs := make([]sourcelayout.DiscResource, 0, len(discIDs))
	for index, id := range discIDs {
		discs = append(discs, sourcelayout.DiscResource{
			ID:   id,
			Name: "Disc " + string(rune('1'+index)),
			Type: "BDMV",
			Root: filepath.Join(sourcePath, id, "BDMV"),
		})
	}
	return sourcelayout.Layout{
		SourcePath: sourcePath,
		DiscType:   "BDMV",
		Discs:      discs,
	}
}

func TestResolveBDMVPlaylistSelectionIsolatesIssuesAndKeepsCandidates(t *testing.T) {
	originalDiscover := discoverBDMVPlaylists
	t.Cleanup(func() { discoverBDMVPlaylists = originalDiscover })
	discoverBDMVPlaylists = func(_ context.Context, _ string) (filesystem.PlaylistReport, error) {
		return stubPlaylistReport(
			oneGoodPlaylist(),
			filesystem.PlaylistIssue{
				File:   "00002.MPLS",
				Stage:  filesystem.PlaylistFailureHeader,
				Reason: "invalid MPLS signature",
			},
		), nil
	}

	_, err := (&Service{repo: &fakeRepo{}}).resolveBDMVPlaylistSelection(context.Background(), preparationstate.Request{
		Layout: diagnosticBDMVLayout("Disc", "disc-test"),
	})
	var required *api.PlaylistSelectionRequiredError
	if !errors.As(err, &required) {
		t.Fatalf("expected PlaylistSelectionRequiredError, got %v", err)
	}
	if len(required.Candidates) != 1 || required.Candidates[0].File != "00001.MPLS" {
		t.Fatalf("candidates = %#v", required.Candidates)
	}
	if len(required.Issues) != 1 {
		t.Fatalf("issues = %#v, want 1", required.Issues)
	}
	issue := required.Issues[0]
	if issue.File != "00002.MPLS" || issue.Stage != api.PlaylistIssueStageHeader || issue.Reason != "invalid MPLS signature" {
		t.Fatalf("issue = %#v", issue)
	}
	if issue.DiscID != "disc-test" || issue.DiscName != "Disc 1" {
		t.Fatalf("issue disc scope = %#v", issue)
	}
}

func TestResolveBDMVPlaylistSelectionAllCorruptReturnsDiscoveryError(t *testing.T) {
	originalDiscover := discoverBDMVPlaylists
	t.Cleanup(func() { discoverBDMVPlaylists = originalDiscover })
	discoverBDMVPlaylists = func(_ context.Context, _ string) (filesystem.PlaylistReport, error) {
		return stubPlaylistReport(nil,
			filesystem.PlaylistIssue{
				File:   "00001.MPLS",
				Stage:  filesystem.PlaylistFailureOpen,
				Reason: "cannot open playlist file: no such file or directory",
			},
			filesystem.PlaylistIssue{
				File:   "00002.MPLS",
				Stage:  filesystem.PlaylistFailureHeader,
				Reason: "invalid MPLS signature",
			},
		), nil
	}

	_, err := (&Service{repo: &fakeRepo{}}).resolveBDMVPlaylistSelection(context.Background(), preparationstate.Request{
		Layout: diagnosticBDMVLayout("Disc", "disc-test"),
	})
	var discovery *api.PlaylistDiscoveryError
	if !errors.As(err, &discovery) {
		t.Fatalf("expected PlaylistDiscoveryError, got %v", err)
	}
	if len(discovery.Issues) != 2 || discovery.Issues[0].File != "00001.MPLS" || discovery.Issues[1].File != "00002.MPLS" {
		t.Fatalf("discovery issues = %#v", discovery.Issues)
	}
	if errors.Is(err, internalerrors.ErrInvalidInput) {
		t.Fatalf("all-corrupt discovery must use the diagnostic error, got invalid-input wrapping: %v", err)
	}
	if _, ok := errors.AsType[*api.PlaylistSelectionRequiredError](err); ok {
		t.Fatal("all-corrupt disc must not produce a selectable action")
	}
	for _, issue := range discovery.Issues {
		if issue.Reason == "" || issue.Stage == "" || issue.File == "" {
			t.Fatalf("incomplete diagnostic issue: %#v", issue)
		}
	}
}

func TestResolveBDMVPlaylistSelectionProceedsWithCandidatesDespiteIssues(t *testing.T) {
	originalDiscover := discoverBDMVPlaylists
	t.Cleanup(func() { discoverBDMVPlaylists = originalDiscover })
	discoverBDMVPlaylists = func(_ context.Context, _ string) (filesystem.PlaylistReport, error) {
		return stubPlaylistReport(
			oneGoodPlaylist(),
			filesystem.PlaylistIssue{
				File:   "00002.MPLS",
				Stage:  filesystem.PlaylistFailurePlayItems,
				Reason: "cannot read playlist play items: unexpected end of file",
			},
		), nil
	}

	repo := &stubRepo{}
	selected, err := (&Service{repo: repo}).resolveBDMVPlaylistSelection(context.Background(), preparationstate.Request{
		Input: api.PrepareInput{Instructions: api.ReleaseFactInstructions{Playlist: api.PlaylistInstruction{
			Set:      true,
			Selected: []string{"00001.MPLS"},
		}}},
		Layout:            diagnosticBDMVLayout("Collection/Disc", "disc-test"),
		SourceFingerprint: "inventory-fingerprint",
	})
	if err != nil {
		t.Fatalf("selection with surviving candidates should proceed: %v", err)
	}
	if len(selected) != 1 || selected[0].File != "00001.MPLS" {
		t.Fatalf("selected = %#v", selected)
	}
	if repo.playlistSelectionSaveCalls != 1 {
		t.Fatalf("selection save calls = %d, want 1", repo.playlistSelectionSaveCalls)
	}
}

func TestResolveBDMVPlaylistSelectionMultiDiscFailsAtomicallyWhenDiscAllCorrupt(t *testing.T) {
	originalDiscover := discoverBDMVPlaylists
	t.Cleanup(func() { discoverBDMVPlaylists = originalDiscover })
	discoverBDMVPlaylists = func(_ context.Context, root string) (filesystem.PlaylistReport, error) {
		switch filepath.Base(root) {
		case "BDMV":
			if filepath.Base(filepath.Dir(root)) == "disc-a" {
				return stubPlaylistReport(oneGoodPlaylist()), nil
			}
			return stubPlaylistReport(nil, filesystem.PlaylistIssue{
				File:   "00009.MPLS",
				Stage:  filesystem.PlaylistFailureStreamFiles,
				Reason: "cannot associate referenced stream files: not a directory",
			}), nil
		default:
			return filesystem.PlaylistReport{}, errors.New("unexpected disc root")
		}
	}

	repo := &stubRepo{}
	request := preparationstate.Request{
		Input: api.PrepareInput{Instructions: api.ReleaseFactInstructions{Playlist: api.PlaylistInstruction{
			Set:      true,
			Selected: []string{"disc-a:00001.MPLS", "disc-b:00001.MPLS"},
		}}},
		Layout:            diagnosticBDMVLayout("Collection", "disc-a", "disc-b"),
		SourceFingerprint: "inventory-fingerprint",
	}
	_, err := (&Service{repo: repo}).resolveBDMVPlaylistSelection(context.Background(), request)
	var discovery *api.PlaylistDiscoveryError
	if !errors.As(err, &discovery) {
		t.Fatalf("expected PlaylistDiscoveryError, got %v", err)
	}
	if len(discovery.Issues) != 1 || discovery.Issues[0].DiscID != "disc-b" || discovery.Issues[0].File != "00009.MPLS" {
		t.Fatalf("issues = %#v, want disc-b/00009.MPLS", discovery.Issues)
	}
	if repo.playlistSelectionSaveCalls != 0 {
		t.Fatalf("partial selection saved %d time(s)", repo.playlistSelectionSaveCalls)
	}
}

func TestResolveBDMVPlaylistSelectionDiscoveryNotFoundStaysTerminal(t *testing.T) {
	originalDiscover := discoverBDMVPlaylists
	t.Cleanup(func() { discoverBDMVPlaylists = originalDiscover })
	discoverBDMVPlaylists = func(_ context.Context, _ string) (filesystem.PlaylistReport, error) {
		return filesystem.PlaylistReport{}, internalerrors.ErrNotFound
	}

	_, err := (&Service{repo: &fakeRepo{}}).resolveBDMVPlaylistSelection(context.Background(), preparationstate.Request{
		Layout: diagnosticBDMVLayout("Disc", "disc-test"),
	})
	if !errors.Is(err, internalerrors.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, ok := errors.AsType[*api.PlaylistDiscoveryError](err); ok {
		t.Fatalf("missing PLAYLIST directory must not become a diagnostic error: %v", err)
	}
}
