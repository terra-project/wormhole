package guardiand

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/certusone/wormhole/bridge/pkg/common"
	nodev1 "github.com/certusone/wormhole/bridge/pkg/proto/node/v1"
	"github.com/certusone/wormhole/bridge/pkg/supervisor"
	"github.com/certusone/wormhole/bridge/pkg/vaa"
)

type nodePrivilegedService struct {
	nodev1.UnimplementedNodePrivilegedServer
	injectC chan<- *vaa.VAA
	logger  *zap.Logger
}

// adminGuardianSetUpdateToVAA converts a nodev1.GuardianSetUpdate message to its canonical VAA representation.
// Returns an error if the data is invalid.
func adminGuardianSetUpdateToVAA(req *nodev1.GuardianSetUpdate) (*vaa.VAA, error) {
	if len(req.Guardians) == 0 {
		return nil, errors.New("empty guardian set specified")
	}

	if len(req.Guardians) > common.MaxGuardianCount {
		return nil, fmt.Errorf("too many guardians - %d, maximum is %d", len(req.Guardians), common.MaxGuardianCount)
	}

	addrs := make([]ethcommon.Address, len(req.Guardians))
	for i, g := range req.Guardians {
		if !ethcommon.IsHexAddress(g.Pubkey) {
			return nil, fmt.Errorf("invalid pubkey format at index %d (%s)", i, g.Name)
		}

		addrs[i] = ethcommon.HexToAddress(g.Pubkey)
	}

	v := &vaa.VAA{
		Version:          vaa.SupportedVAAVersion,
		GuardianSetIndex: req.CurrentSetIndex,
		Timestamp:        time.Unix(int64(req.Timestamp), 0),
		Payload: &vaa.BodyGuardianSetUpdate{
			Keys:     addrs,
			NewIndex: req.CurrentSetIndex + 1,
		},
	}

	return v, nil
}

func (s *nodePrivilegedService) SubmitGuardianSetVAA(ctx context.Context, req *nodev1.SubmitGuardianSetVAARequest) (*nodev1.SubmitGuardianSetVAAResponse, error) {
	s.logger.Info("guardian set injected via admin socket", zap.String("request", req.String()))

	v, err := adminGuardianSetUpdateToVAA(req.GuardianSet)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// Generate digest of the unsigned VAA.
	digest, err := v.SigningMsg()
	if err != nil {
		panic(err)
	}

	s.logger.Info("guardian set VAA constructed",
		zap.Any("vaa", v),
		zap.String("digest", digest.String()),
	)

	s.injectC <- v

	return &nodev1.SubmitGuardianSetVAAResponse{Digest: digest.Bytes()}, nil
}

func adminServiceRunnable(logger *zap.Logger, socketPath string, injectC chan<- *vaa.VAA) (supervisor.Runnable, error) {
	// Delete existing UNIX socket, if present.
	fi, err := os.Stat(socketPath)
	if err == nil {
		fmode := fi.Mode()
		if fmode&os.ModeType == os.ModeSocket {
			err = os.Remove(socketPath)
			if err != nil {
				return nil, fmt.Errorf("failed to remove existing socket at %s: %w", socketPath, err)
			}
		} else {
			return nil, fmt.Errorf("%s is not a UNIX socket", socketPath)
		}
	}

	// Create a new UNIX socket and listen to it.

	// The socket is created with the default umask. We set a restrictive umask in setRestrictiveUmask
	// to ensure that any files we create are only readable by the user - this is much harder to mess up.
	// The umask avoids a race condition between file creation and chmod.

	laddr, err := net.ResolveUnixAddr("unix", socketPath)
	l, err := net.ListenUnix("unix", laddr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", socketPath, err)
	}

	logger.Info("admin server listening on", zap.String("path", socketPath))

	nodeService := &nodePrivilegedService{
		injectC: injectC,
		logger:  logger.Named("adminservice"),
	}

	grpcServer := grpc.NewServer()
	nodev1.RegisterNodePrivilegedServer(grpcServer, nodeService)
	return supervisor.GRPCServer(grpcServer, l, false), nil
}
