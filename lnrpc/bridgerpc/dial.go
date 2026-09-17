//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// maxMessage is the largest reply accepted.
//
// lnd's own defaults are generous and a node with many channels can exceed
// gRPC's 4 MiB default on calls the bridge does not make. Allowing more costs
// nothing here.
const maxMessage = 50 * 1024 * 1024

// dialTimeout bounds setting the connection up, not reaching the node.
const dialTimeout = 30 * time.Second

// macaroonCredential presents a macaroon on every call.
//
// lnd expects it as hex in the "macaroon" metadata key. That is the whole of
// what is needed here, so it is written out rather than pulling in the
// macaroon service, which exists to mint and check them rather than to send
// one.
type macaroonCredential struct {
	hex string
}

// GetRequestMetadata returns the macaroon header.
func (m macaroonCredential) GetRequestMetadata(context.Context, ...string) (
	map[string]string, error) {

	return map[string]string{"macaroon": m.hex}, nil
}

// RequireTransportSecurity is true because a macaroon is a bearer token. Sent
// in the clear it is the whole of that node's authority, handed to whoever is
// listening.
func (m macaroonCredential) RequireTransportSecurity() bool { return true }

// dialSHA256Node connects to the SHA256 node.
//
// It returns as soon as the configuration is usable, which is not the same as
// the node being reachable: gRPC connects lazily, so a wrong address or a node
// that is down shows up on the first call rather than here. Remote.Check is
// what turns that into a startup failure instead of a failed swap.
func dialSHA256Node(cfg *Config) (*grpc.ClientConn, error) {
	if cfg.SHA256RPCHost == "" {
		return nil, fmt.Errorf("%w: no SHA256 node address",
			ErrConfig)
	}

	// The node's own self-signed certificate is the only root accepted.
	// Using the system pool instead would accept any certificate a public
	// authority issued for that name, which for a loopback or LAN address
	// is a weaker thing than it sounds.
	creds, err := credentials.NewClientTLSFromFile(
		cfg.SHA256TLSCertPath, "",
	)
	if err != nil {
		return nil, fmt.Errorf("%w: reading the SHA256 node's TLS "+
			"certificate %s: %w", ErrConfig, cfg.SHA256TLSCertPath,
			err)
	}

	mac, err := os.ReadFile(cfg.SHA256MacaroonPath)
	if err != nil {
		return nil, fmt.Errorf("%w: reading the SHA256 node's "+
			"macaroon %s: %w", ErrConfig, cfg.SHA256MacaroonPath,
			err)
	}
	if len(mac) == 0 {
		return nil, fmt.Errorf("%w: the SHA256 node's macaroon %s is "+
			"empty", ErrConfig, cfg.SHA256MacaroonPath)
	}

	conn, err := grpc.NewClient(cfg.SHA256RPCHost,
		grpc.WithTransportCredentials(creds),
		grpc.WithPerRPCCredentials(macaroonCredential{
			hex: hex.EncodeToString(mac),
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxMessage),
			grpc.MaxCallSendMsgSize(maxMessage),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: dialling the SHA256 node at %s: "+
			"%w", ErrConfig, cfg.SHA256RPCHost, err)
	}

	return conn, nil
}
