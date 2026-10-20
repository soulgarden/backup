package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
)

const VerificationScope = "archive_read_and_remote_sha256"

var runPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[a-f0-9]{32}$`)

type Checks struct {
	TOC             TOCResult `json:"toc"`
	FullArchiveRead bool      `json:"full_archive_read"`
	RemoteSHA256    bool      `json:"remote_sha256"`
}
type Manifest struct {
	FormatVersion            int     `json:"format_version"`
	RunID                    string  `json:"run_id"`
	Database                 string  `json:"database"`
	ArchiveKey               string  `json:"archive_key"`
	ArchiveBytes             int64   `json:"archive_bytes"`
	ArchiveSHA256            string  `json:"archive_sha256"`
	DumpStartedAt            string  `json:"dump_started_at"`
	DumpFinishedAt           string  `json:"dump_finished_at"`
	VerificationFinishedAt   string  `json:"verification_finished_at"`
	ImageVersion             string  `json:"image_version"`
	DumpMajor                int     `json:"dump_major"`
	RequiredRestoreMajor     int     `json:"required_restore_major"`
	TargetServerMajor        int     `json:"target_server_major"`
	ConfigurationFingerprint string  `json:"configuration_fingerprint"`
	Filters                  Filters `json:"filters"`
	Checks                   Checks  `json:"checks"`
	VerificationScope        string  `json:"verification_scope"`
	DatabaseRestoreTested    bool    `json:"database_restore_tested"`
	Status                   string  `json:"status"`
	Versions
}
type Run struct {
	ID       string
	Objects  map[string]Object
	Manifest Manifest
}

func ValidateManifest(m Manifest, prefix, run string) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err = decode(raw, manifestSchema, &Manifest{}); err != nil {
		return errors.New("AMBIGUOUS_MANIFEST")
	}
	if m.RunID != run || m.ArchiveKey != prefix+run+"/backup.dump" {
		return errors.New("AMBIGUOUS_MANIFEST")
	}
	return nil
}
func ListRuns(ctx context.Context, s Store, prefix string) (complete, incomplete []Run, err error) {
	objects, err := s.List(ctx, prefix)
	if err != nil {
		return nil, nil, err
	}
	groups := map[string]map[string]Object{}
	for _, o := range objects {
		if !strings.HasPrefix(o.Key, prefix) {
			return nil, nil, errors.New("AMBIGUOUS_LAYOUT")
		}
		parts := strings.Split(strings.TrimPrefix(o.Key, prefix), "/")
		if len(parts) != 2 || !runPattern.MatchString(parts[0]) || (parts[1] != "backup.dump" && parts[1] != "manifest.json") {
			return nil, nil, errors.New("AMBIGUOUS_LAYOUT")
		}
		if groups[parts[0]] == nil {
			groups[parts[0]] = map[string]Object{}
		}
		groups[parts[0]][parts[1]] = o
	}
	ids := []string{}
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		files := groups[id]
		r := Run{ID: id, Objects: files}
		marker := prefix + id + "/manifest.json"
		if _, ok := files["manifest.json"]; !ok {
			absent, e := s.Absent(ctx, marker)
			if e != nil {
				return nil, nil, e
			}
			if !absent {
				return nil, nil, errors.New("CONCURRENT_MANIFEST")
			}
			incomplete = append(incomplete, r)
			continue
		}
		r.Manifest, err = s.ReadManifest(ctx, marker)
		if err != nil {
			return nil, nil, err
		}
		if err = ValidateManifest(r.Manifest, prefix, id); err != nil {
			return nil, nil, err
		}
		size, e := s.Head(ctx, r.Manifest.ArchiveKey)
		if e != nil {
			return nil, nil, e
		}
		if _, ok := files["backup.dump"]; !ok || size != r.Manifest.ArchiveBytes {
			return nil, nil, errors.New("AMBIGUOUS_ARCHIVE")
		}
		complete = append(complete, r)
	}
	sort.Slice(complete, func(i, j int) bool {
		a, b := complete[i], complete[j]
		at, bt := a.Objects["manifest.json"].Modified, b.Objects["manifest.json"].Modified
		if at.Equal(bt) {
			return a.ID > b.ID
		}
		return at.After(bt)
	})
	return complete, incomplete, nil
}

type Candidate struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
	DryRun bool   `json:"dry_run"`
}

func Rotate(ctx context.Context, s Store, c Config, current string, now time.Time) ([]Candidate, error) {
	p := c.Retention
	if !p.Enabled {
		return nil, nil
	}
	if p.MaxAgeDays < 1 || p.MaxAgeDays > 36500 || p.IncompleteRunMaxAgeDays < 1 || p.IncompleteRunMaxAgeDays > 36500 || p.KeepLast < 1 {
		return nil, errors.New("INVALID_RETENTION_DURATION")
	}
	version, err := s.Versioning(ctx)
	if err != nil {
		return nil, err
	}
	if version != "" {
		return nil, errors.New("VERSIONING_UNSUPPORTED")
	}
	complete, incomplete, err := ListRuns(ctx, s, c.S3.Prefix)
	if err != nil {
		return nil, err
	}
	protected := []Run{}
	found := false
	candidates := []Candidate{}
	for i, r := range complete {
		if r.ID == current {
			found = true
		}
		if i < p.KeepLast || r.ID == current {
			protected = append(protected, r)
			continue
		}
		if now.Sub(r.Objects["manifest.json"].Modified) > time.Duration(p.MaxAgeDays)*24*time.Hour {
			for _, name := range []string{"manifest.json", "backup.dump"} {
				candidates = append(candidates, Candidate{r.Objects[name].Key, "completed_older_than_retention", p.DryRun})
			}
		}
	}
	if !found {
		return nil, errors.New("CURRENT_MANIFEST_MISSING")
	}
	orphans := []Run{}
	for _, r := range incomplete {
		newest := time.Time{}
		for _, o := range r.Objects {
			if o.Modified.After(newest) {
				newest = o.Modified
			}
		}
		if now.Sub(newest) > time.Duration(p.IncompleteRunMaxAgeDays)*24*time.Hour {
			orphans = append(orphans, r)
			for _, o := range r.Objects {
				candidates = append(candidates, Candidate{o.Key, "incomplete_run_expired", p.DryRun})
			}
		}
	}
	if p.Legacy.Enabled && len(complete) >= p.Legacy.MinVerifiedV2Sets {
		objects, e := s.List(ctx, p.Legacy.Prefix)
		if e != nil {
			return nil, e
		}
		pattern, e := regexp.Compile(p.Legacy.FilenamePattern)
		if e != nil {
			return nil, e
		}
		for _, o := range objects {
			if strings.HasPrefix(o.Key, c.S3.Prefix) {
				continue
			}
			name := strings.TrimPrefix(o.Key, p.Legacy.Prefix)
			if strings.HasPrefix(o.Key, p.Legacy.Prefix) && !strings.Contains(name, "/") && pattern.MatchString(name) && now.Sub(o.Modified) > time.Duration(p.MaxAgeDays)*24*time.Hour {
				candidates = append(candidates, Candidate{o.Key, "legacy_older_than_retention", p.DryRun || p.Legacy.DryRun})
			}
		}
	}
	if p.DryRun {
		return candidates, nil
	}
	if !p.DeletionApproved {
		return nil, errors.New("DELETION_NOT_APPROVED")
	}
	for _, r := range protected {
		m, e := s.ReadManifest(ctx, r.Objects["manifest.json"].Key)
		if e != nil {
			return nil, e
		}
		a, _ := json.Marshal(m)
		b, _ := json.Marshal(r.Manifest)
		if !bytes.Equal(a, b) {
			return nil, errors.New("PROTECTED_MANIFEST_CHANGED")
		}
		size, e := s.Head(ctx, m.ArchiveKey)
		if e != nil {
			return nil, e
		}
		if size != m.ArchiveBytes {
			return nil, errors.New("PROTECTED_ARCHIVE_CHANGED")
		}
	}
	// Recheck orphan markers before starting any deletion.
	for _, r := range orphans {
		absent, e := s.Absent(ctx, c.S3.Prefix+r.ID+"/manifest.json")
		if e != nil {
			return nil, e
		}
		if !absent {
			return nil, errors.New("CONCURRENT_MANIFEST")
		}
	}
	for _, candidate := range candidates {
		if !candidate.DryRun {
			if err = s.Delete(ctx, candidate.Key); err != nil {
				return nil, err
			}
		}
	}
	return candidates, nil
}
func SizeAnomaly(runs []Run, m Manifest, p SizePolicy) bool {
	samples := []int64{}
	for _, r := range runs {
		if r.ID != m.RunID && r.Manifest.ConfigurationFingerprint == m.ConfigurationFingerprint {
			samples = append(samples, r.Manifest.ArchiveBytes)
			if len(samples) == p.HistorySamples {
				break
			}
		}
	}
	if len(samples) < p.MinSamples {
		return false
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	median := float64(samples[len(samples)/2])
	if len(samples)%2 == 0 {
		median = (median + float64(samples[len(samples)/2-1])) / 2
	}
	ratio := float64(m.ArchiveBytes) / median
	return ratio < p.MinRatio || ratio > p.MaxRatio
}
