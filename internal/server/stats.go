package server

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/SteliosSpanos/spotd/gen/spotd/v1"
	"github.com/SteliosSpanos/spotd/internal/store"
)

const (
	// Safety limits so a client can't crash the server
	defaultLimit = 10
	maxLimit     = 100
)

type StatsStore interface {
	TopArtists(ctx context.Context, since time.Time, limit int) ([]store.ArtistCount, error)
	TopTracks(ctx context.Context, since time.Time, limit int) ([]store.TrackCount, error)
	ListeningByDay(ctx context.Context, since time.Time) ([]store.DayCount, error)
}

// Stats is the server struct handling incoming gRPC network requests
type Stats struct {
	pb.UnimplementedStatsServiceServer
	store StatsStore
	now   func() time.Time // Allows injecting a fake clock for testing
}

func NewStats(s StatsStore) *Stats {
	return &Stats{store: s, now: time.Now}
}

// since() is a helper that converts the Protobuf enum into an exact Go time.Time object by substracting days from the current clock
func (s *Stats) since(r pb.TimeRange) (time.Time, error) {
	switch r {
	case pb.TimeRange_TIME_RANGE_LAST_7_DAYS:
		return s.now().AddDate(0, 0, -7), nil
	case pb.TimeRange_TIME_RANGE_LAST_30_DAYS:
		return s.now().AddDate(0, 0, -30), nil
	case pb.TimeRange_TIME_RANGE_UNSPECIFIED, pb.TimeRange_TIME_RANGE_ALL_TIME:
		return time.Time{}, nil
	default:
		return time.Time{}, status.Errorf(codes.InvalidArgument, "unknown time range %d", r)
	}
}

func clampLimit(n int32) (int, error) {
	switch {
	case n < 0:
		return 0, status.Error(codes.InvalidArgument, "limit must not be negative")
	case n == 0:
		return defaultLimit, nil
	case n > maxLimit:
		return maxLimit, nil
	default:
		return int(n), nil
	}
}

func (s *Stats) TopArtists(ctx context.Context, req *pb.TopArtistsRequest) (*pb.TopArtistsResponse, error) {
	since, err := s.since(req.GetRange())
	if err != nil {
		return nil, err
	}

	limit, err := clampLimit(req.GetLimit())
	if err != nil {
		return nil, err
	}

	rows, err := s.store.TopArtists(ctx, since, limit)
	if err != nil {
		return nil, err
	}

	out := make([]*pb.ArtistCount, 0, len(rows))
	for _, r := range rows {
		out = append(out, &pb.ArtistCount{Id: r.ID, Name: r.Name, Plays: r.Plays})
	}

	return &pb.TopArtistsResponse{Artists: out}, nil
}

func (s *Stats) TopTracks(ctx context.Context, req *pb.TopTracksRequest) (*pb.TopTracksResponse, error) {
	since, err := s.since(req.GetRange())
	if err != nil {
		return nil, err
	}

	limit, err := clampLimit(req.GetLimit())
	if err != nil {
		return nil, err
	}

	rows, err := s.store.TopTracks(ctx, since, limit)
	if err != nil {
		return nil, err
	}

	out := make([]*pb.TrackCount, 0, len(rows))
	for _, r := range rows {
		out = append(out, &pb.TrackCount{Id: r.ID, Title: r.Title, Artists: r.Artists, Album: r.Album, Plays: r.Plays})
	}

	return &pb.TopTracksResponse{Tracks: out}, nil
}

func (s *Stats) ListeningByDay(ctx context.Context, req *pb.ListeningByDayRequest) (*pb.ListeningByDayResponse, error) {
	since, err := s.since(req.GetRange())
	if err != nil {
		return nil, err
	}

	rows, err := s.store.ListeningByDay(ctx, since)
	if err != nil {
		return nil, err
	}

	out := make([]*pb.DayCount, 0, len(rows))
	for _, r := range rows {
		out = append(out, &pb.DayCount{Date: r.Date, Plays: r.Plays, ListenedMs: r.ListenedMs})
	}

	return &pb.ListeningByDayResponse{Days: out}, nil
}
