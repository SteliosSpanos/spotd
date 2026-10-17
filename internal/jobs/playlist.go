package jobs

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/SteliosSpanos/spotd/internal/spotify"
)

var ErrNoBackup = errors.New("no backup found for this playlist")

type PlaylistAPI interface {
	Playlist(ctx context.Context, id string) (*spotify.Playlist, error)
	PlaylistItems(ctx context.Context, id string) ([]spotify.PlaylistItem, error)
	CreatePlaylist(ctx context.Context, name, description string) (*spotify.Playlist, error)
	AddItems(ctx context.Context, id string, uris []string, position int) (string, error)
	RemoveItems(ctx context.Context, id string, uris []string, snapshotID string) (string, error)
	ReplaceItems(ctx context.Context, id string, uris []string) (string, error)
}

// Playlists builds our Runner functions
type Playlists struct {
	api       PlaylistAPI
	backupDir string
	now       func() time.Time
}

func NewPlaylists(api PlaylistAPI, backupDir string) *Playlists {
	return &Playlists{api: api, backupDir: backupDir, now: time.Now}
}

func (p *Playlists) Runner(kind Kind, playlistID string) (Runner, error) {
	// The ID ends up in backup file paths and glob patterns, so only allow Spotify's base62 IDs
	if !validID(playlistID) {
		return nil, fmt.Errorf("invalid playlist id %q", playlistID)
	}

	switch kind {
	case Backup:
		return func(ctx context.Context, report Report) error {
			_, err := p.backup(ctx, playlistID, report)
			return err
		}, nil
	case Restore:
		return func(ctx context.Context, report Report) error {
			return p.restore(ctx, playlistID, report)
		}, nil
	case Dedupe:
		return func(ctx context.Context, report Report) error {
			return p.dedupe(ctx, playlistID, report)
		}, nil
	case Sort:
		return func(ctx context.Context, report Report) error {
			return p.sort(ctx, playlistID, report)
		}, nil
	default:
		return nil, fmt.Errorf("unknown job kind %v", kind)
	}
}

type backupFile struct {
	PlaylistID string       `json:"playlist_id"`
	Name       string       `json:"name"`
	SnapshotID string       `json:"snapshot_id"`
	CreatedAt  time.Time    `json:"created_at"`
	Items      []backupItem `json:"items"`
}

type backupItem struct {
	URI     string    `json:"uri"`
	Name    string    `json:"name"`
	AddedAt time.Time `json:"added_at"`
}

// backup saves the playlist to <backupDir>/<id>-<timestamp>.json
// It returns the items it fetched so dedupe/sort don't need to fetch them twice
func (p *Playlists) backup(ctx context.Context, id string, report Report) ([]spotify.PlaylistItem, error) {
	report(0, 0, "fetching playlist")

	meta, err := p.api.Playlist(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get playlist: %w", err)
	}

	items, err := p.api.PlaylistItems(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get items: %w", err)
	}

	b := backupFile{PlaylistID: id, Name: meta.Name, SnapshotID: meta.SnapshotID, CreatedAt: p.now().UTC()}
	for _, it := range items {
		if it.Item == nil {
			continue // Unavailable track, nothing to save
		}
		b.Items = append(b.Items, backupItem{URI: it.Item.URI, Name: it.Item.Name, AddedAt: it.AddedAt})
	}

	data, err := json.MarshalIndent(b, "", " ")
	if err != nil {
		return nil, err
	}

	// The timestamp format sorts alphabetically in time order, restore relies on that
	name := fmt.Sprintf("%s-%s.json", id, b.CreatedAt.Format("20060102T150405Z"))
	if err := writeFileAtomic(filepath.Join(p.backupDir, name), data); err != nil {
		return nil, fmt.Errorf("write backup: %w", err)
	}

	report(len(b.Items), len(b.Items), "backup saved: "+name)
	return items, nil
}

// writeFileAtomic writes to a temp file in the same directory and renames it over the target
// Rename is atomic on the same filesystem, so a crash can never leave a half-written backup
func writeFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	f, err := os.CreateTemp(dir, ".tmp-*") // CreateTemp already uses 0600 permissions
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()

	if _, err = f.Write(data); err != nil {
		return err
	}

	// Sync flushes to the disk before the rename, otherwise a power cut could leave an empty file
	if err = f.Sync(); err != nil {
		return err
	}

	if err = f.Close(); err != nil {
		return err
	}

	return os.Rename(f.Name(), path)
}

// restore recreates the newest backup of a playlist as a new playlist
// It never overwrites the original, so a bad restore can't destroy anything
func (p *Playlists) restore(ctx context.Context, id string, report Report) error {
	matches, err := filepath.Glob(filepath.Join(p.backupDir, id+"-*.json"))
	if err != nil {
		return err
	}

	if len(matches) == 0 {
		return ErrNoBackup
	}

	slices.Sort(matches)
	// The restore function will always use the most up-to-date backup file
	path := matches[len(matches)-1]

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var b backupFile
	if err := json.Unmarshal(data, &b); err != nil {
		return fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}

	// Local files can't be added through the API
	uris := make([]string, 0, len(b.Items))
	for _, it := range b.Items {
		if !strings.HasPrefix(it.URI, "spotify:local:") {
			uris = append(uris, it.URI)
		}
	}

	desc := "Restored by spotd from backup of " + b.CreatedAt.Format(time.DateOnly)
	pl, err := p.api.CreatePlaylist(ctx, b.Name+" (restored)", desc)
	if err != nil {
		return fmt.Errorf("create playlist: %w", err)
	}

	return addAll(ctx, p.api, pl.ID, uris, report)
}

// addAll appends uris in chunks of 100, reporting after every chunk
func addAll(ctx context.Context, api PlaylistAPI, id string, uris []string, report Report) error {
	report(0, len(uris), "adding tracks")

	done := 0
	for chunk := range slices.Chunk(uris, spotify.MaxItemsPerRequest) {
		if err := ctx.Err(); err != nil {
			return err
		}

		if _, err := api.AddItems(ctx, id, chunk, -1); err != nil {
			return fmt.Errorf("add items: %w", err)
		}

		done += len(chunk)
		report(done, len(uris), "adding tracks")
	}

	return nil
}

func (p *Playlists) dedupe(ctx context.Context, id string, report Report) error {
	items, err := p.backup(ctx, id, report)
	if err != nil {
		return err
	}

	seen := make(map[string]bool)
	var dups []string
	isDup := make(map[string]bool)

	for _, it := range items {
		if it.Item == nil || it.IsLocal {
			continue
		}

		uri := it.Item.URI
		if seen[uri] {
			if !isDup[uri] {
				isDup[uri] = true
				dups = append(dups, uri)
			}
			continue
		}

		seen[uri] = true
	}

	if len(dups) == 0 {
		report(0, 0, "no duplicates found")
		return nil
	}

	meta, err := p.api.Playlist(ctx, id)
	if err != nil {
		return err
	}

	total := len(dups) * 2
	done := 0
	snap := meta.SnapshotID

	// When you tell Spotify to delete a URI, it deletes every copy of it
	for chunk := range slices.Chunk(dups, spotify.MaxItemsPerRequest) {
		if snap, err = p.api.RemoveItems(ctx, id, chunk, snap); err != nil {
			return fmt.Errorf("removing duplicates: %w", err)
		}
		done += len(chunk)
		report(done, total, "removing duplicate clusters")
	}

	// Because all copies were wiped out, we must re-add exactly one copy of each
	return addAll(ctx, p.api, id, dups, func(added, _ int, msg string) {
		report(done+added, total, "re-adding one copy of each")
	})
}

func (p *Playlists) sort(ctx context.Context, id string, report Report) error {
	items, err := p.backup(ctx, id, report)
	if err != nil {
		return err
	}

	for _, it := range items {
		if it.Item == nil || it.IsLocal {
			return errors.New("playlist has local or unavailable tracks, sorting would drop them")
		}
	}

	slices.SortStableFunc(items, func(a, b spotify.PlaylistItem) int {
		return cmp.Or(
			cmp.Compare(primaryArtist(a.Item), primaryArtist(b.Item)),
			cmp.Compare(strings.ToLower(a.Item.Album.Name), strings.ToLower(b.Item.Album.Name)),
			cmp.Compare(strings.ToLower(a.Item.Name), strings.ToLower(b.Item.Name)),
		)
	})

	uris := make([]string, 0, len(items))
	for _, it := range items {
		uris = append(uris, it.Item.URI)
	}

	// Wipe the playlist and replace it with the first 100 sorted songs
	first := uris[:min(len(uris), spotify.MaxItemsPerRequest)]
	if _, err := p.api.ReplaceItems(ctx, id, first); err != nil {
		return fmt.Errorf("replace items: %w", err)
	}

	report(len(first), len(uris), "writing sorted order")

	// Append the rest of the 100-chunk batches to the bottom
	rest := uris[len(first):]
	return addAll(ctx, p.api, id, rest, func(done, _ int, msg string) {
		report(len(first)+done, len(uris), msg)
	})
}

func primaryArtist(t *spotify.Track) string {
	if len(t.Artists) == 0 {
		return ""
	}

	return strings.ToLower(t.Artists[0].Name)
}

func validID(id string) bool {
	if id == "" {
		return false
	}

	for _, r := range id {
		if !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9') {
			return false
		}
	}

	return true
}
