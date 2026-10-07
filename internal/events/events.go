package events

import "time"

type NowPlaying struct {
	TrackID    string
	IsPlaying  bool
	ProgressMs int
	DurationMs int
	FetchedAt  time.Time
}

type HistoryUpdated struct {
	New int64 // How many new tracks were just saved to the database
}

// When published, it will contain either NowPlaying or HistoryUpdated
type Event struct {
	NowPlaying     *NowPlaying
	HistoryUpdated *HistoryUpdated
}
