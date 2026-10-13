package server

import (
	"context"
	"regexp"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/SteliosSpanos/spotd/gen/spotd/v1"
	"github.com/SteliosSpanos/spotd/internal/events"
)

type PlayerAPI interface {
	Play(ctx context.Context, uri string) error
	Pause(ctx context.Context) error
	Next(ctx context.Context) error
	Queue(ctx context.Context, uri string) error
}

type EventSource interface {
	Subscribe() (<-chan events.Event, func())
}

// Player is the actual server struct that fulfills the generated gRPC interfaces
type Player struct {
	// gRPC requires to embed this
	// It ensures that if you add a new RPC to the .proto file but forget to write the code for it, the server won't crash and it will return an error to the client
	pb.UnimplementedPlayerServiceServer
	api PlayerAPI
	bus EventSource
}

func NewPlayer(api PlayerAPI, bus EventSource) *Player {
	return &Player{api: api, bus: bus}
}

// Pre-compile the regex for performance
var trackURI = regexp.MustCompile(`^spotify:track:[A-Za-z0-9]{22}$`)

func validateTrackURI(uri string) error {
	if !trackURI.MatchString(uri) {
		return status.Error(codes.InvalidArgument, "uri must look like spotify:track:<22 char>")
	}

	return nil
}

func (p *Player) Play(ctx context.Context, req *pb.PlayRequest) (*pb.PlayResponse, error) {
	uri := req.GetUri()

	if uri != "" {
		if err := validateTrackURI(uri); err != nil {
			return nil, err // The interceptor will translate this error
		}
	}

	// Call the actual Spotify HTTP client
	if err := p.api.Play(ctx, uri); err != nil {
		return nil, err
	}

	return &pb.PlayResponse{}, nil
}

func (p *Player) Pause(ctx context.Context, _ *pb.PauseRequest) (*pb.PauseResponse, error) {
	if err := p.api.Pause(ctx); err != nil {
		return nil, err
	}

	return &pb.PauseResponse{}, nil
}

func (p *Player) Next(ctx context.Context, _ *pb.NextRequest) (*pb.NextResponse, error) {
	if err := p.api.Next(ctx); err != nil {
		return nil, err
	}

	return &pb.NextResponse{}, nil
}

func (p *Player) Queue(ctx context.Context, req *pb.QueueRequest) (*pb.QueueResponse, error) {
	uri := req.GetUri()

	// Unlike Play, an empty uri has no meaning here
	if err := validateTrackURI(uri); err != nil {
		return nil, err
	}

	if err := p.api.Queue(ctx, uri); err != nil {
		return nil, err
	}

	return &pb.QueueResponse{}, nil
}

// WatchEvents implements the streaming RPC
// There is no return response and it instead receives a stream object that stays open indefinitely
func (p *Player) WatchEvents(_ *pb.WatchEventsRequest, stream grpc.ServerStreamingServer[pb.WatchEventsResponse]) error {
	ch, cancel := p.bus.Subscribe()
	defer cancel()

	ctx := stream.Context()

	for {
		select {
		case <-ctx.Done():
			// The client disconnected so we shut down their stream
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				// The broadcaster was closed
				return status.Error(codes.Unavailable, "event stream closed")
			}

			if err := stream.Send(eventToProto(ev)); err != nil {
				return err
			}
		}
	}
}

func eventToProto(ev events.Event) *pb.WatchEventsResponse {
	switch {
	case ev.NowPlaying != nil:
		np := ev.NowPlaying
		return &pb.WatchEventsResponse{Event: &pb.WatchEventsResponse_NowPlaying{NowPlaying: &pb.NowPlaying{
			TrackId:    np.TrackID,
			Title:      np.Title,
			Artists:    np.Artists,
			Album:      np.Album,
			IsPlaying:  np.IsPlaying,
			ProgressMs: int64(np.ProgressMs),
			DurationMs: int64(np.DurationMs),
			FetchedAt:  timestamppb.New(np.FetchedAt),
		}}}
	case ev.HistoryUpdated != nil:
		return &pb.WatchEventsResponse{Event: &pb.WatchEventsResponse_HistoryUpdated{
			HistoryUpdated: &pb.HistoryUpdated{NewPlays: ev.HistoryUpdated.New},
		}}
	default:
		// The poller publishes an event with both fields nil when playback stops
		return &pb.WatchEventsResponse{Event: &pb.WatchEventsResponse_Stopped{Stopped: &pb.Stopped{}}}
	}
}
