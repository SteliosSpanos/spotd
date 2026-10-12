package server

import (
	"context"
	"errors"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/SteliosSpanos/spotd/internal/auth"
	"github.com/SteliosSpanos/spotd/internal/spotify"
)

// gRPC clients don't understand plain text errors
// They expect structured gRPC Status Codes
func toStatus(err error) error {
	if err == nil {
		return nil
	}

	// If the error is already a gRPC error we let it through
	if _, ok := status.FromError(err); ok {
		return err
	}

	var apiErr *spotify.APIError

	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "spotify timed out")
	case errors.Is(err, auth.ErrNotLoggedIn):
		return status.Error(codes.Unauthenticated, "run `spotd login`")
	case errors.As(err, &apiErr):
		switch {
		case apiErr.Reason == "NO_ACTIVE_DEVICE":
			return status.Error(codes.FailedPrecondition, "no active Spotify device")
		case apiErr.Status == http.StatusUnauthorized:
			return status.Error(codes.Unauthenticated, "run `spotd login`")
		case apiErr.Status == http.StatusTooManyRequests:
			return status.Error(codes.ResourceExhausted, "rate limited by Spotify")
		case apiErr.Status == http.StatusForbidden:
			return status.Error(codes.PermissionDenied, apiErr.Message)
		}
	}

	return status.Error(codes.Internal, "internal error")
}
