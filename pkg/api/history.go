// Copyright (c) 2025-2026, Audionut and the autobrr contributors.
// SPDX-License-Identifier: GPL-2.0-or-later

package api

import (
	"strings"
	"time"
)

type HistoryEntry struct {
	SourcePath         string
	ReleaseTitle       string
	ReleaseSource      string
	ReleaseResolution  string
	MetadataUpdatedAt  time.Time `ts_type:"string"`
	LatestUploadStatus string
	LatestUploadAt     time.Time `ts_type:"string"`
	RuleFailureCount   int
	// RuleAdvisoryCount contains non-blocking advisory rule results.
	RuleAdvisoryCount int
}

// HistoryRecord is the persistence-facing history aggregate.
type HistoryRecord struct {
	Corrections          ReleaseCorrectionsSnapshot
	SourcePath           string
	ReleaseTitle         string
	ReleaseSource        string
	ReleaseResolution    string
	MetadataUpdatedAt    time.Time `ts_type:"string"`
	LatestUploadStatus   string
	LatestUploadAt       time.Time `ts_type:"string"`
	Metadata             FileMetadata
	PreparedRelease      *PreparedRelease
	ReleaseNameOverrides ReleaseNameOverrides
	DescriptionOverride  DescriptionOverride
	DescriptionOverrides []DescriptionOverride
	PlaylistSelection    PlaylistSelection
	TrackerMetadata      []TrackerMetadata
	TrackerRuleFailures  []TrackerRuleFailure
	Screenshots          []Screenshot
	FinalSelections      []ScreenshotFinalSelection
	UploadedImages       []UploadedImageLink
	UploadHistory        []UploadRecord
}

// HistoryOverview is the transport projection for one stored release.
type HistoryOverview struct {
	SourcePath           string
	ReleaseTitle         string
	ReleaseSource        string
	ReleaseResolution    string
	MetadataUpdatedAt    time.Time `ts_type:"string"`
	LatestUploadStatus   string
	LatestUploadAt       time.Time `ts_type:"string"`
	StatusLabel          string
	Metadata             FileMetadata
	Release              ReleaseRef
	Identity             ExternalIdentity
	Display              PreparedReleaseDisplay
	ReleaseNameOverrides ReleaseNameOverrides
	DescriptionOverride  DescriptionOverride
	DescriptionOverrides []DescriptionOverride
	PlaylistSelection    PlaylistSelection
	TrackerMetadata      []TrackerMetadata
	TrackerRuleFailures  []TrackerRuleFailure
	Screenshots          []Screenshot
	FinalSelections      []ScreenshotFinalSelection
	UploadedImages       []UploadedImageLink
	UploadHistory        []UploadRecord
}

// HistoryDeleteSkipReason is the stable, secret-free code explaining why one
// stored source is excluded from a full-history cleanup plan.
type HistoryDeleteSkipReason string

const (
	// HistoryDeleteSkipActiveInput marks a source related to a currently
	// active input or active workflow work.
	HistoryDeleteSkipActiveInput HistoryDeleteSkipReason = "active_input"
	// HistoryDeleteSkipPathProtected marks a source whose cleanup targets are
	// protected by another source's managed paths or active-input roots.
	HistoryDeleteSkipPathProtected HistoryDeleteSkipReason = "path_protected"
)

// HistoryDeletePreviewSkip identifies one stored source that a full-history
// cleanup would leave in place. SourcePath is the same user-owned path shown
// by ListHistory; reasons never carry managed artifact paths or raw errors.
type HistoryDeletePreviewSkip struct {
	SourcePath string
	Reason     HistoryDeleteSkipReason
}

// HistoryDeleteAllPreview is the read-only summary of a full-history cleanup.
// Counts describe the sequential deletion outcome computed with the same
// source association, shared-artifact and path-protection decisions as the
// real cleanup, without modifying any state.
type HistoryDeleteAllPreview struct {
	// Sources is the number of stored roots the cleanup would delete.
	Sources int
	// ChildSources is the number of additional stored sources removed
	// through a parent source's association.
	ChildSources int
	// Files is the number of distinct managed files (tmp, cache and nfo
	// artifacts) the cleanup would remove.
	Files int
	// SharedFilesKept is the number of distinct artifact files retained
	// because a source outside the deletion batch still references them.
	SharedFilesKept int
	// PrivateWorkflows is the number of distinct private workflow scopes
	// whose vault artifacts and durable state would be closed.
	PrivateWorkflows int
	// SkippedSources lists sources left in place in stable stored order.
	SkippedSources []HistoryDeletePreviewSkip
}

// HistoryStatusLabel returns the display label for a persisted upload status,
// falling back to rule-issue or stored state when no status is available.
func HistoryStatusLabel(rawStatus string, ruleFailureCount int) string {
	status := strings.TrimSpace(strings.ToLower(rawStatus))
	switch status {
	case "pending":
		return "Pending"
	case "pending-internal":
		return "Pending Internal"
	case "uploaded", "success", "completed":
		return "Uploaded"
	case "failed", "error":
		return "Failed"
	}
	if status != "" {
		normalized := strings.ReplaceAll(status, "-", " ")
		words := strings.Fields(normalized)
		for idx, word := range words {
			if word == "" {
				continue
			}
			words[idx] = strings.ToUpper(word[:1]) + word[1:]
		}
		return strings.Join(words, " ")
	}
	if ruleFailureCount > 0 {
		return "Rule Issues"
	}
	return "Stored"
}
