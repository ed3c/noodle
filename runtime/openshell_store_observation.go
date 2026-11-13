package runtime

import (
	"context"
	"errors"
	"fmt"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

// ObserveOpenShellStoreCount reads every workspace/page without publishing
// sandbox specs or environments. Store absence alone is not physical absence.
func ObserveOpenShellStoreCount(ctx context.Context, gateway OpenShellGatewaySpec) (count int, err error) {
	conn, err := newOpenShellConnection(gateway.Binding.Gateway, gateway.TLS)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	client := pb.NewOpenShellClient(conn)
	token, seen := "", map[string]bool{}
	for {
		response, err := client.ListSandboxes(ctx, &pb.ListSandboxesRequest{WorkspaceScope: &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_AllWorkspaces{AllWorkspaces: &dm.AllWorkspaces{}}}, PageSize: 100, PageToken: token})
		if err != nil {
			return 0, err
		}
		count += len(response.GetSandboxes())
		token = response.GetNextPageToken()
		if token == "" {
			return count, nil
		}
		if seen[token] {
			return 0, fmt.Errorf("Source store inventory repeated a pagination token")
		}
		seen[token] = true
	}
}
