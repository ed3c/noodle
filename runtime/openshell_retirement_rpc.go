package runtime

import (
	"context"
	"log/slog"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc"
)

const openShellReclamationTimeout = 30 * time.Second

// Borrow the original authenticated client for retirement only. Each actual
// native RPC gets its own budget; local proof checks and durable writes keep
// the actor context. Existing observeStopped retains its separate 5s limit.
type retirementRPCClient struct {
	pb.OpenShellClient
	identity OpenShellIdentity
}

func (c retirementRPCClient) ListSandboxes(ctx context.Context, request *pb.ListSandboxesRequest, opts ...grpc.CallOption) (response *pb.ListSandboxesResponse, err error) {
	ctx, finish := c.rpcContext(ctx, "native_retirement_list")
	defer func() { finish(err) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.OpenShellClient.ListSandboxes(ctx, request, opts...)
}

func (c retirementRPCClient) GetSandbox(ctx context.Context, request *pb.GetSandboxRequest, opts ...grpc.CallOption) (response *pb.SandboxResponse, err error) {
	ctx, finish := c.rpcContext(ctx, "native_retirement_get")
	defer func() { finish(err) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.OpenShellClient.GetSandbox(ctx, request, opts...)
}

func (c retirementRPCClient) DeleteSandbox(ctx context.Context, request *pb.DeleteSandboxRequest, opts ...grpc.CallOption) (response *pb.DeleteSandboxResponse, err error) {
	ctx, finish := c.rpcContext(ctx, "native_retirement_delete")
	defer func() { finish(err) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.OpenShellClient.DeleteSandbox(ctx, request, opts...)
}

func (c retirementRPCClient) rpcContext(parent context.Context, phase string) (context.Context, func(error)) {
	ctx, cancel := context.WithTimeout(parent, openShellReclamationTimeout)
	started := time.Now()
	logger := slog.Default().With("phase", phase, "order", c.identity.OrderID, "session", c.identity.SessionID)
	logger.Info("cleanup phase started", "timeout", openShellReclamationTimeout)
	return ctx, func(err error) {
		cancel()
		if err != nil {
			logger.Error("cleanup phase failed", "elapsed", time.Since(started), "error", err)
		} else {
			logger.Info("cleanup phase completed", "elapsed", time.Since(started))
		}
	}
}
