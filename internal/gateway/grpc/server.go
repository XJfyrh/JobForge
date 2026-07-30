// Package grpc implements the Worker Gateway gRPC service. It bridges Worker
// Runtime processes to the PostgreSQL store, handling registration, polling,
// heartbeat, completion and failure reporting.
//
// The Gateway does not contain state transition logic; all transitions are
// delegated to the store/domain layer. It adds long-poll waiting (ADR-0003),
// deadline enforcement and error code mapping (ADR-0002).
package grpc

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

// Server wraps the gRPC server and its dependencies.
type Server struct {
	grpcServer *grpc.Server
	service    *WorkerService
	logger     *slog.Logger
}

// NewServer creates a gRPC server with the WorkerService registered.
func NewServer(service *WorkerService, logger *slog.Logger) *Server {
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(deadlineInterceptor(logger)),
	)
	workerv1.RegisterWorkerServiceServer(grpcServer, service)

	return &Server{
		grpcServer: grpcServer,
		service:    service,
		logger:     logger,
	}
}

// Serve starts the gRPC server on the given address. Blocks until stopped.
func (s *Server) Serve(addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	s.logger.Info("gRPC gateway listening", "addr", addr)
	return s.grpcServer.Serve(lis)
}

// GracefulStop gracefully stops the gRPC server.
func (s *Server) GracefulStop() {
	s.grpcServer.GracefulStop()
}

// deadlineInterceptor rejects RPCs without a deadline and logs slow calls.
func deadlineInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		// All Worker RPCs must carry a deadline per PRD 10.2.
		if _, ok := ctx.Deadline(); !ok {
			return nil, status.Error(codes.InvalidArgument, "RPC must carry a deadline")
		}

		resp, err := handler(ctx, req)
		if err != nil {
			st, _ := status.FromError(err)
			logger.Warn("rpc error",
				"method", info.FullMethod,
				"code", st.Code().String(),
				"message", st.Message(),
			)
		}
		return resp, err
	}
}
