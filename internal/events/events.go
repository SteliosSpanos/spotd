package events

import "time"

type NowPlaying struct {
	TrackID    string
	Title string
	IsPlaying  bool
	Artists []string
	Album string
	ProgressMs int
	DurationMs int
	FetchedAt  time.Time
}

type HistoryUpdated struct {
	New int64 // How many new tracks were just saved to the database
}

// When published, it will contain either NowPlaying or HistoryUpdated
// Both nil means playback stopped
type Event struct {
	NowPlaying     *NowPlaying
	HistoryUpdated *HistoryUpdated
}
