// Copyright (c) 2025-2026, Audionut and the autobrr contributors.
// SPDX-License-Identifier: GPL-2.0-or-later

package api

// PlaylistInfo represents a discovered playlist with an opaque backend ID.
// DiscID scopes duplicate filenames; File and DiscName are display evidence only.
type PlaylistInfo struct {
	ID       string         `json:"id"`
	DiscID   string         `json:"discId"`
	DiscName string         `json:"discName"`
	File     string         `json:"file"`
	Duration float64        `json:"duration"`
	Items    []PlaylistItem `json:"items"`
	Score    float64        `json:"score"`
	Edition  string         `json:"edition"`
}

// PlaylistItem represents a single file reference within a playlist.
type PlaylistItem struct {
	File string `json:"file"`
	Size int64  `json:"size"`
}

// Playlist issue stages reported by diagnostic BDMV playlist discovery. File
// and disc names are display evidence only; reasons never contain paths.
const (
	PlaylistIssueStageOpen        = "open"
	PlaylistIssueStageHeader      = "header"
	PlaylistIssueStagePlayItems   = "play_items"
	PlaylistIssueStageStreamFiles = "stream_files"
)

// PlaylistIssue describes one MPLS playlist file that could not be parsed.
type PlaylistIssue struct {
	// DiscID scopes the issue on multi-disc sources.
	DiscID string `json:"discId,omitempty"`
	// DiscName is display evidence for the owning disc.
	DiscName string `json:"discName,omitempty"`
	// File is the unreadable playlist basename.
	File string `json:"file"`
	// Stage locates the failure, one of the PlaylistIssueStage* constants.
	Stage string `json:"stage"`
	// Reason is a readable, path-free explanation.
	Reason string `json:"reason"`
}
