package server

import (
	"context"
	"errors"
	"net/http"

	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/SteliosSpanos/spotd/internal/auth"
	"github.com/SteliosSpanos/spotd/internal/jobs"
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
	var retrieveErr *oauth2.RetrieveError

	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "spotify timed out")
	case errors.Is(err, auth.ErrNotLoggedIn):
		return status.Error(codes.Unauthenticated, "run `spotd login`")
	case errors.Is(err, jobs.ErrBusy):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, jobs.ErrClosed):
		return status.Error(codes.Unavailable, "daemon is shutting down")
	case errors.Is(err, jobs.ErrNotFound):
		return status.Error(codes.NotFound, "unknown job")
	case errors.As(err, &retrieveErr):
		// A revoked or expired refresh token comes back from the token endpoint as invalid_grant
		if retrieveErr.ErrorCode == "invalid_grant" || retrieveErr.Response != nil &&
			(retrieveErr.Response.StatusCode == http.StatusBadRequest || retrieveErr.Response.StatusCode == http.StatusUnauthorized) {
			return status.Error(codes.Unauthenticated, "Spotify authorization expired or revoked: run `spotd login`")
		}
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
		case apiErr.Status == http.StatusNotFound:
			return status.Error(codes.NotFound, "not found on Spotify")
		case apiErr.Status == http.StatusBadRequest:
			return status.Error(codes.InvalidArgument, "Spotify rejected the request")
		}
	}

	return status.Error(codes.Internal, "internal error")
}
