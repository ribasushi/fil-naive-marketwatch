package main

import (
	"context"
	"fmt"
	"io"
	"math/bits"
	"sort"
	"sync/atomic"
	"time"

	filaddr "github.com/filecoin-project/go-address"
	lotustypes "github.com/filecoin-project/lotus/chain/types"
	"github.com/fxamacker/cbor/v2"
	"github.com/georgysavva/scany/pgxscan"
	"github.com/multiformats/go-multiaddr"
	infomempeerstore "github.com/ribasushi/go-libp2p-infomempeerstore"
	"github.com/ribasushi/go-toolbox-interplanetary/fil"
	"github.com/ribasushi/go-toolbox-interplanetary/lp2p"
	"github.com/ribasushi/go-toolbox/cmn"
	"github.com/ribasushi/go-toolbox/ufcli"
	"golang.org/x/sync/errgroup"
)

const (
	retrievalTransports = "/fil/retrieval/transports/1.0.0" // this is boost-specific, do not bring extra dependency
	storageProposalV120 = "/fil/storage/mk/1.2.0"
)

type spInfo struct {
	Errors             []string                         `json:"errors,omitempty"`
	SectorLog2Size     uint8                            `json:"sector_log2_size"`
	PeerID             *lp2p.PeerID                     `json:"peerid"`
	MultiAddrs         []multiaddr.Multiaddr            `json:"multiaddrs"`
	PeerInfo           *infomempeerstore.PeerData       `json:"peer_info,omitempty"`
	RetrievalProtocols map[string][]multiaddr.Multiaddr `json:"retrieval_protocols,omitempty"`
	localPeerID        *string
	dialTookMsecs      *int64
}

// copy of https://github.com/filecoin-project/boost/blob/v1.5.0/retrievalmarket/types/transports.go#L12-L21
type retrievalTransports100RawResponse struct {
	Protocols []struct {
		Name      string
		Addresses [][]byte
	}
}

func (rs *retrievalTransports100RawResponse) UnmarshalCBOR(r io.Reader) error {
	return cbor.NewDecoder(r).Decode(rs)
}

var (
	pollRequeryAll  bool
	pollConcurrency int
	pollTimeout     int
)

var pollProviders = &ufcli.Command{
	Usage: "Query metadata of recently-seen storage providers",
	Name:  "poll-providers",
	Flags: []ufcli.Flag{
		&ufcli.BoolFlag{
			Name:        "requery-all",
			Usage:       "Query every SP that is known to the app",
			Destination: &pollRequeryAll,
		},
		&ufcli.IntFlag{
			Name:        "query-concurrency",
			Usage:       "How many SPs to query concurrently",
			Value:       64,
			Destination: &pollConcurrency,
		},
		&ufcli.IntFlag{
			Name:        "query-timeout",
			Usage:       "Query timeout in seconds",
			Value:       10,
			Destination: &pollTimeout,
		},
	},
	Action: func(cctx *ufcli.Context) error {
		ctx, log, db, _ := UnpackCtx(cctx.Context)

		allSPs := make([]fil.ActorID, 0, 2<<10)
		if err := pgxscan.Select(
			ctx,
			db,
			&allSPs,
			`
			SELECT p.provider_id
				FROM naive.providers p
				LEFT JOIN naive.providers_info pi USING ( provider_id )
			WHERE
				pi.provider_last_polled IS NULL
					OR
				pi.provider_last_polled < NOW() - '3 hours'::INTERVAL
			ORDER BY RANDOM()
			LIMIT 128
			`,
		); err != nil {
			return cmn.WrErr(err)
		}

		totals := struct {
			totalQueried  int
			unaddressable *int32
			undialable    *int32
			lacksV120     *int32
		}{
			totalQueried:  len(allSPs),
			unaddressable: new(int32),
			undialable:    new(int32),
			lacksV120:     new(int32),
		}
		defer func() {
			log.Infow("summary",
				"totalQueried", totals.totalQueried,
				"unaddressable", atomic.LoadInt32(totals.unaddressable),
				"undialable", atomic.LoadInt32(totals.undialable),
				"lacksV120", atomic.LoadInt32(totals.lacksV120),
			)
		}()

		log.Infof("about to query state of %d SPs", len(allSPs))

		eg, ctx := errgroup.WithContext(ctx)
		eg.SetLimit(pollConcurrency)
		for _, spid := range allSPs {
			spid := spid
			eg.Go(func() error {
				spi, err := getSPInfo(
					ctx,
					spid.AsFilAddr(),
					time.Duration(pollTimeout)*time.Second,
				)
				if err != nil {
					return err
				}

				switch {
				case len(spi.MultiAddrs) == 0:
					atomic.AddInt32(totals.unaddressable, 1)
				case spi.PeerInfo == nil:
					atomic.AddInt32(totals.undialable, 1)
				case func() bool { _, can := spi.PeerInfo.Protos[storageProposalV120]; return !can }():
					atomic.AddInt32(totals.lacksV120, 1)
				}

				_, err = db.Exec(
					ctx,
					`
					INSERT INTO naive.providers_info
						( provider_id, provider_last_polled, info_dialing_took_msecs, info_dialing_peerid, info )
						VALUES ( $1, NOW(), $2, $3, $4 )
					ON CONFLICT ( provider_id ) DO UPDATE SET
						provider_last_polled = EXCLUDED.provider_last_polled,
						info_dialing_took_msecs = EXCLUDED.info_dialing_took_msecs,
						info_dialing_peerid = EXCLUDED.info_dialing_peerid,
						info = EXCLUDED.info
					`,
					spid,
					spi.dialTookMsecs,
					spi.localPeerID,
					spi,
				)
				return err
			})
		}
		return eg.Wait()
	},
}

func getSPInfo(ctx context.Context, sp filaddr.Address, timeOut time.Duration) (spInfo, error) {
	_, log, _, lapi := UnpackCtx(ctx)

	ctx, ctxCloser := context.WithTimeout(ctx, timeOut)
	defer ctxCloser()

	mi, err := lapi.StateMinerInfo(ctx, sp, lotustypes.EmptyTSK)
	if err != nil {
		return spInfo{}, cmn.WrErr(err)
	}

	var spi spInfo
	if bits.OnesCount64(uint64(mi.SectorSize)) != 1 {
		spi.Errors = append(spi.Errors, fmt.Sprintf("the SectorSize value %d is not a power of 2", mi.SectorSize))
		return spi, nil
	}
	spi.SectorLog2Size = uint8(bits.TrailingZeros64(uint64(mi.SectorSize)))

	if mi.PeerId == nil {
		spi.Errors = append(spi.Errors, "the PeerID field in MinerInfo is not set")
		return spi, nil
	}
	spi.PeerID = mi.PeerId

	spi.MultiAddrs = make([]multiaddr.Multiaddr, 0, len(mi.Multiaddrs))
	for i, encMA := range mi.Multiaddrs {
		ma, err := multiaddr.NewMultiaddrBytes(encMA)
		if err != nil {
			w := fmt.Sprintf("multiaddress entry '%x' (#%d) within the MinerInfo of SP %s is malformed: %s", encMA, i, sp.String(), err)
			log.Warn(w)
			spi.Errors = append(spi.Errors, w)
		} else {
			spi.MultiAddrs = append(spi.MultiAddrs, ma)
		}
	}
	if len(spi.MultiAddrs) == 0 {
		spi.Errors = append(spi.Errors, "no usable multiaddrs in MinerInfo")
		return spi, nil
	}

	nodeHost, peerStore, err := lp2p.NewPlainNodeTCP(timeOut)
	if err != nil {
		return spInfo{}, cmn.WrErr(err)
	}
	lpid := nodeHost.ID().String()
	spi.localPeerID = &lpid
	defer func() {
		if err := nodeHost.Close(); err != nil {
			log.Warnf("unexpected error shutting down node %s: %s", spi.localPeerID, err)
		}
	}()

	pTag := "provider-poll"
	nodeHost.ConnManager().Protect(*spi.PeerID, pTag)
	defer nodeHost.ConnManager().Unprotect(*spi.PeerID, pTag)
	t0 := time.Now()
	err = nodeHost.Connect(ctx, lp2p.AddrInfo{
		ID:    *spi.PeerID,
		Addrs: spi.MultiAddrs,
	})
	dtm := time.Since(t0).Milliseconds()
	spi.dialTookMsecs = &dtm
	if err != nil {
		spi.Errors = append(spi.Errors, err.Error())
		return spi, nil
	}

	pd := peerStore.GetPeerData(*spi.PeerID)
	spi.PeerInfo = &pd

	eg, ctxEg := errgroup.WithContext(ctx)

	// TODO figure out how to ask for random CIDs in the future ( real-random doesn't work )
	/*
		resRetAsk := new(gfmretrieval.QueryResponse)
		excRetAsk := make([]string, 0, 4)
		eg.Go(func() error {
			// does not support the query protocol - bail
			if _, canQueryProto := spi.PeerInfo.Protos[retrievalQueryAsk]; !canQueryProto {
				return nil
			}

			randMh := make([]byte, 1+1+32)
			randMh[0] = multihash.SHA2_256
			randMh[1] = 32
			rand.Read(randMh[2:]) //nolint:errcheck

			if err := lp2pRPC(
				ctxEg,
				nodeHost,
				spi.PeerID,
				retrievalQueryAsk,
				gfmretrieval.Query{
					PayloadCID: cid.NewCidV1(cid.Raw, multihash.Multihash(randMh)),
				},
				resRetAsk,
			); err != nil {
				excRetAsk = append(excRetAsk, err.Error())
			}

			return nil
		})
	*/

	resRetProtos := make(map[string][]multiaddr.Multiaddr)
	excRetProtos := make([]string, 0, 4)
	eg.Go(func() error {
		// does not support the transports protocol - bail
		if _, canRetProto := spi.PeerInfo.Protos[retrievalTransports]; !canRetProto {
			return nil
		}

		respRetTrans := new(retrievalTransports100RawResponse)
		if err := lp2p.DoCborRPC(
			ctxEg,
			nodeHost,
			*spi.PeerID,
			retrievalTransports,
			nil,
			respRetTrans,
		); err != nil {
			excRetProtos = append(excRetProtos, err.Error())
			return nil
		}

		if len(respRetTrans.Protocols) == 0 {
			return nil
		}

		for _, p := range respRetTrans.Protocols {
			for i, a := range p.Addresses {
				ma, err := multiaddr.NewMultiaddrBytes(a)
				if err != nil {
					w := fmt.Sprintf(
						"multiaddress entry '%x' (#%d) for protocol %s is malformed in the RetrievalTransports response of SP %s: %s",
						a,
						i,
						p.Name,
						sp.String(),
						err.Error(),
					)
					log.Warn(w)
					excRetProtos = append(excRetProtos, w)
				} else {
					resRetProtos[p.Name] = append(resRetProtos[p.Name], ma)
				}
			}
		}
		for _, maddrs := range resRetProtos {
			sort.Slice(maddrs, func(i, j int) bool {
				return maddrs[i].String() < maddrs[j].String()
			})
		}
		return nil
	})

	if err := eg.Wait(); err != nil {
		return spInfo{}, cmn.WrErr(err)
	}

	spi.Errors = append(spi.Errors, excRetProtos...)
	spi.RetrievalProtocols = resRetProtos

	return spi, nil
}
