package server

import (
	"log/slog"

	pb "github.com/SteliosSpanos/spotd/gen/spotd/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func New(l *slog.Logger, player pb.PlayerServiceServer, stats pb.StatsServiceServer, playlists pb.PlaylistServiceServer) *grpc.Server {
	s := grpc.NewServer(
		Stop waits for running handlers, so none can start a job after shutdown
		grpc.WaitForHandlers(true),
		grpc.ChainUnaryInterceptor(recoverUnary(l), statusUnary(l)),
		grpc.ChainStreamInterceptor(recoverStream(l), statusStream(l)),
	)

	pb.RegisterPlayerServiceServer(s, player)
	pb.RegisterStatsServiceServer(s, stats)
	pb.RegisterPlaylistServiceServer(s, playlists)

	healthpb.RegisterHealthServer(s, health.NewServer())

	return s
}
