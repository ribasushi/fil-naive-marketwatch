package main

import (
	"context"
	"fmt"
	"time"

	cborutil "github.com/filecoin-project/go-cbor-util"
	lotusbuild "github.com/filecoin-project/lotus/build"
	lp2p "github.com/libp2p/go-libp2p"
	lp2phost "github.com/libp2p/go-libp2p/core/host"
	lp2pnet "github.com/libp2p/go-libp2p/core/network"
	lp2ppeer "github.com/libp2p/go-libp2p/core/peer"
	lp2pproto "github.com/libp2p/go-libp2p/core/protocol"
	lp2pconnmgr "github.com/libp2p/go-libp2p/p2p/net/connmgr"
	lp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
	lp2ptcp "github.com/libp2p/go-libp2p/p2p/transport/tcp"
	cmn "github.com/ribasushi/fil-naive-marketwatch/common"
	"github.com/ribasushi/fil-naive-marketwatch/infomempeerstore"
)

func newLp2pNode(withTimeout time.Duration) (lp2phost.Host, *infomempeerstore.PeerStore, error) {
	ps, err := infomempeerstore.NewPeerstore()
	if err != nil {
		return nil, nil, cmn.WrErr(err)
	}

	connmgr, err := lp2pconnmgr.NewConnManager(8192, 16384) // effectively deactivate
	if err != nil {
		return nil, nil, cmn.WrErr(err)
	}

	nodeHost, err := lp2p.New(
		lp2p.Peerstore(ps),  // allows us collect random on-connect data
		lp2p.RandomIdentity, // *NEVER* reuse a peerid
		lp2p.DisableRelay(),
		lp2p.ResourceManager(lp2pnet.NullResourceManager),
		lp2p.ConnectionManager(connmgr),
		lp2p.Ping(false),
		lp2p.NoListenAddrs,
		lp2p.NoTransports,
		lp2p.Transport(lp2ptcp.NewTCPTransport, lp2ptcp.WithConnectionTimeout(withTimeout+100*time.Millisecond)),
		lp2p.Security(lp2ptls.ID, lp2ptls.New),
		lp2p.UserAgent("lotus-"+lotusbuild.BuildVersion+lotusbuild.BuildTypeString()),
		lp2p.WithDialTimeout(withTimeout),
	)
	if err != nil {
		return nil, nil, cmn.WrErr(err)
	}

	return nodeHost, ps, nil
}

func lp2pRPC(
	ctx context.Context,
	host lp2phost.Host, peer lp2ppeer.ID,
	proto lp2pproto.ID, args interface{}, resp interface{},
) error {

	st, err := host.NewStream(ctx, peer, proto)
	if err != nil {
		return fmt.Errorf("error while opening %s stream: %w", proto, err)
	}
	defer st.Close() //nolint:errcheck
	if d, hasDline := ctx.Deadline(); hasDline {
		st.SetDeadline(d)                 //nolint:errcheck
		defer st.SetDeadline(time.Time{}) //nolint:errcheck
	}
	if args != nil {
		if err := cborutil.WriteCborRPC(st, args); err != nil {
			return fmt.Errorf("error while writing to %s stream: %w", proto, err)
		}
	}

	if err := cborutil.ReadCborRPC(st, resp); err != nil {
		return fmt.Errorf("error while reading %s response: %w", proto, err)
	}

	return nil
}
