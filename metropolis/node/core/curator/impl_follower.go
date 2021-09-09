package curator

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cpb "source.monogon.dev/metropolis/node/core/curator/proto/api"
	apb "source.monogon.dev/metropolis/proto/api"
)

type curatorFollower struct {
}

func (f *curatorFollower) Watch(req *cpb.WatchRequest, srv cpb.Curator_WatchServer) error {
	return status.Error(codes.Unimplemented, "curator follower not implemented")
}

func (f *curatorFollower) Escrow(srv apb.AAA_EscrowServer) error {
	return status.Error(codes.Unimplemented, "curator follower not implemented")
}

func (f *curatorFollower) GetRegisterTicket(_ context.Context, _ *apb.GetRegisterTicketRequest) (*apb.GetRegisterTicketResponse, error) {
	return nil, status.Error(codes.Unimplemented, "curator follower not implemented")
}

func (f *curatorFollower) UpdateNodeStatus(ctx context.Context, req *cpb.UpdateNodeStatusRequest) (*cpb.UpdateNodeStatusResponse, error) {
	return nil, status.Error(codes.Unimplemented, "curator follower not implemented")
}
