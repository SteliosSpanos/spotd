package server

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// This file implements gRPC interceptors, which is the gRPC term for Middleware
// Instead of writing logging, error translation and crash-prevention inside every API endpoint, we write it once here
// They wrap around the server, and every incoming request flows through them before hitting our logic, and every response flows back through them

// Writes one line per finished RPC
func logRPC(ctx context.Context, l *slog.Logger, method string, start time.Time, err error) {
	code := status.Code(toStatus(err))

	// Default to INFO logs, but escalate to ERROR if something broke internally
	level := slog.LevelInfo
	if code == codes.Internal || code == codes.Unknown {
		level = slog.LevelError
	}

	attrs := []slog.Attr{
		slog.String("method", method),
		slog.String("code", code.String()),
		slog.Duration("dur", time.Since(start)),
	}

	if err != nil {
		attrs = append(attrs, slog.String("err", err.Error()))
	}

	l.LogAttrs(ctx, level, "rpc", attrs...)
}

// It's middleware for standard request/response endpoints (Play, Pause)
func statusUnary(l *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		start := time.Now()

		// h(ctx, req) calls our actual endpoint logic and waits for it to finish
		resp, err := h(ctx, req)

		// Log the result
		logRPC(ctx, l, info.FullMethod, start, err)

		return resp, toStatus(err)
	}
}

// It's the exact same logic, but for streaming endpoints (WatchEvents)
func statusStream(l *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
		start := time.Now()
		err := h(srv, ss)
		logRPC(ss.Context(), l, info.FullMethod, start, err)
		return toStatus(err)
	}
}

// Prevents a single buggy endpoint from crashing the entire background daemon
func recoverUnary(l *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
		// defer ensures this anonymous function runs right before the parent function exits, even if the parent function crashes
		defer func() {
			// recover() catches a fatal panic and stops the program from dying
			if r := recover(); r != nil {
				l.Error("panic in handler", "method", info.FullMethod, "panic", r, "stack", string(debug.Stack()))
				resp, err = nil, status.Error(codes.Internal, "internal error")
			}
		}()

		return h(ctx, req)
	}
}

func recoverStream(l *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				l.Error("panic in handler", "method", info.FullMethod, "panic", r, "stack", string(debug.Stack()))
				err = status.Error(codes.Internal, "internal error")
			}
		}()

		return h(srv, ss)
	}
}
