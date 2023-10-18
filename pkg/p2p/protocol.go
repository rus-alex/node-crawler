package p2p

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/Fantom-foundation/go-opera/gossip"
	"github.com/Fantom-foundation/go-opera/inter"
	"github.com/Fantom-foundation/go-opera/opera"
	"github.com/Fantom-foundation/lachesis-base/hash"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discover/discfilter"
	"github.com/ethereum/go-ethereum/p2p/dnsdisc"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/ethereum/node-crawler/pkg/common"
)

type ProbeBackend struct {
	Progress gossip.PeerProgress
	NodeInfo *gossip.NodeInfo
	Opera    *opera.Rules
	Chain    *params.ChainConfig

	peers  *peerSet
	output chan *common.NodeJSON

	wg       sync.WaitGroup
	quitSync chan struct{}
}

func NewProbeBackend(output chan *common.NodeJSON) *ProbeBackend {
	return &ProbeBackend{
		output:   output,
		peers:    newPeerSet(),
		quitSync: make(chan struct{}),
	}
}

func (b *ProbeBackend) Close() {
	close(b.quitSync)
	b.wg.Wait()
}

// protocolLengths are the number of implemented message corresponding to different protocol versions.
// TODO: make gossip.protocolLengths public instead.
var gossipProtocolLengths = map[uint]uint64{gossip.FTM62: gossip.EventsStreamResponse + 1, gossip.FTM63: gossip.EPsStreamResponse + 1}

func ProbeProtocols(backend *ProbeBackend) []p2p.Protocol {
	protocols := make([]p2p.Protocol, len(gossip.ProtocolVersions))
	for i, version := range gossip.ProtocolVersions {
		version := version // closure

		protocols[i] = p2p.Protocol{
			Name:    gossip.ProtocolName,
			Version: version,
			Length:  gossipProtocolLengths[version],
			Run: func(p *p2p.Peer, rw p2p.MsgReadWriter) error {
				peer := newPeer(version, p, rw)
				defer peer.Close()

				select {
				case <-backend.quitSync:
					return p2p.DiscQuitting
				default:
					backend.wg.Add(1)
					defer backend.wg.Done()
					return backend.handle(peer)
				}
			},
			NodeInfo: func() interface{} {
				return backend.NodeInfo
			},
			PeerInfo: func(id enode.ID) interface{} {
				if p := backend.peers.Peer(id.String()); p != nil {
					return p.Info()
				}
				return nil
			},
			Attributes:     []enr.Entry{currentENREntry(backend)},
			DialCandidates: operaDialCandidates(),
		}
	}

	return protocols
}

func (b *ProbeBackend) handle(p *peer) error {
	defer p.Disconnect(p2p.DiscUselessPeer)
	// defer discfilter.Ban(p.ID()) // don't connect again

	// Check useless
	useless := discfilter.Banned(p.Node().ID(), p.Node().Record())
	if !strings.Contains(strings.ToLower(p.Name()), "opera") {
		useless = true
	}
	if !p.Peer.Info().Network.Trusted && useless {
		return p2p.DiscUselessPeer
	}

	// Execute the handshake
	if err := p.Handshake(b.NodeInfo.Network, b.Progress, b.NodeInfo.Genesis); err != nil {
		p.Log().Debug("Handshake failed", "err", err)
		return err
	}

	// Register the peer locally
	if err := b.peers.RegisterPeer(p); err != nil {
		p.Log().Warn("Peer registration failed", "err", err)
		return err
	}
	defer b.unregisterPeer(p.id)

	// Handle incoming messages until the connection is torn down
	for {
		err := b.handleMsg(p)
		if err != nil {
			p.Log().Debug("Message handling failed", "err", err)
			return err
		}

		switch p.Status {
		case PeerUseless, PeerEvil:
			// TODO: fill the all
			b.output <- &common.NodeJSON{
				N: p.Node(),
				Info: &common.ClientInfo{
					Blockheight: strconv.FormatUint(uint64(p.progress.LastBlockIdx), 10),
				},
			}
			return nil
			/*
				case PeerEvil:
					p.Status = PeerFetching
					leecher := b.startDagLeecher(p.progress.Epoch)
					leecher.Start()
					defer leecher.Stop()
					p.Log().Info("PEER fetching", "epoch", p.progress.Epoch)
			*/
		}
	}

	return nil
}

// handleMsg is invoked whenever an inbound message is received from a remote
// peer. The remote connection is torn down upon returning any error.
func (b *ProbeBackend) handleMsg(p *peer) error {
	// Read the next message from the remote peer, and ensure it's fully consumed
	msg, err := p.rw.ReadMsg()
	if err != nil {
		return err
	}
	if msg.Size > protocolMaxMsgSize {
		return errResp(gossip.ErrMsgTooLarge, "%v > %v", msg.Size, protocolMaxMsgSize)
	}
	defer msg.Discard()

	// Handle the message depending on its contents
	switch {
	case msg.Code == gossip.HandshakeMsg:
		// Status messages should never arrive after the handshake
		return errResp(gossip.ErrExtraStatusMsg, "uncontrolled status message")

	case msg.Code == gossip.ProgressMsg:
		var progress gossip.PeerProgress
		if err := msg.Decode(&progress); err != nil {
			return errResp(gossip.ErrDecode, "%v: %v", msg, err)
		}
		p.progress = progress
		p.Log().Info("PEER progress", "epoch", progress.Epoch, "block", progress.LastBlockIdx, "atropos", progress.LastBlockAtropos)
		fmt.Printf("\"%s\" // %d %d %s (%s)\n", p.Node().URLv4(), progress.Epoch, progress.LastBlockIdx, progress.LastBlockAtropos, p.Fullname())

		if progress.Epoch > progress.LastBlockAtropos.Epoch() {
			p.Status = PeerEvil
		} else {
			p.Status = PeerUseless
		}

	case msg.Code == gossip.EvmTxsMsg:
		break

	case msg.Code == gossip.NewEvmTxHashesMsg:
		break

	case msg.Code == gossip.GetEvmTxsMsg:
		break

	case msg.Code == gossip.EventsMsg:
		var events inter.EventPayloads
		if err := msg.Decode(&events); err != nil {
			return errResp(gossip.ErrDecode, "%v: %v", msg, err)
		}
		if err := checkLenLimits(len(events), events); err != nil {
			return err
		}
		p.Log().Info("PEER brings", "events", events)
		//h.handleEvents(p, events.Bases(), events.Len() > 1)

	case msg.Code == gossip.NewEventIDsMsg:
		// Fresh events arrived, make sure we have a valid and fresh graph to handle them
		var announces hash.Events
		if err := msg.Decode(&announces); err != nil {
			return errResp(gossip.ErrDecode, "%v: %v", msg, err)
		}
		if err := checkLenLimits(len(announces), announces); err != nil {
			return err
		}
		p.Log().Info("PEER knows", "events", announces)
		//h.handleEventHashes(p, announces)

	case msg.Code == gossip.GetEventsMsg:
		var requests hash.Events
		if err := msg.Decode(&requests); err != nil {
			return errResp(gossip.ErrDecode, "%v: %v", msg, err)
		}
		if err := checkLenLimits(len(requests), requests); err != nil {
			return err
		}

		p.Log().Info("PEER wants", "events", requests)

	case msg.Code == gossip.RequestEventsStream:
		break
	case msg.Code == gossip.EventsStreamResponse:
		var chunk dagChunk
		if err := msg.Decode(&chunk); err != nil {
			return errResp(gossip.ErrDecode, "%v: %v", msg, err)
		}

		if (len(chunk.Events) < 1) && (len(chunk.IDs) < 1) {
			return errors.New("expected either events or event hashes")
		}
		if (len(chunk.Events) > 0) && (len(chunk.IDs) > 0) {
			return errors.New("expected either events or event hashes")
		}

		if len(chunk.IDs) > 0 {
			p.Log().Info("PEER knows", "events", chunk.IDs)
		}
		if len(chunk.Events) > 0 {
			for i, e := range chunk.Events {
				str := &strings.Builder{}
				json.NewEncoder(str).Encode(e.BaseEvent)
				p.Log().Info("PEER knows", "n", i, "event", str.String())
			}
			p.Status = PeerUseless
		}

	case msg.Code == gossip.RequestBVsStream:
		break

	case msg.Code == gossip.BVsStreamResponse:
		break

	case msg.Code == gossip.RequestBRsStream:
		break

	case msg.Code == gossip.BRsStreamResponse:
		break

	case msg.Code == gossip.RequestEPsStream:
		break

	case msg.Code == gossip.EPsStreamResponse:
		break

	default:
		return errResp(gossip.ErrInvalidMsgCode, "%v", msg.Code)
	}
	return nil
}

func (b *ProbeBackend) unregisterPeer(id string) {
	// Short circuit if the peer was already removed
	peer := b.peers.Peer(id)
	if peer == nil {
		return
	}
	log.Debug("Removing peer", "peer", id)

	if err := b.peers.UnregisterPeer(id); err != nil {
		log.Error("Peer removal failed", "peer", id, "err", err)
	}
}

func checkLenLimits(size int, v interface{}) error {
	if size <= 0 {
		return errResp(gossip.ErrEmptyMessage, "%v", v)
	}
	if size > hardLimitItems {
		return errResp(gossip.ErrMsgTooLarge, "%v", v)
	}
	return nil
}

// ENR

// enrEntry is the ENR entry which advertises `eth` protocol on the discovery.
type enrEntry struct {
	ForkID forkid.ID // Fork identifier per EIP-2124

	// Ignore additional fields (for forward compatibility).
	Rest []rlp.RawValue `rlp:"tail"`
}

// ENRKey implements enr.Entry.
func (enrEntry) ENRKey() string {
	return "opera"
}

func currentENREntry(b *ProbeBackend) enr.Entry {
	info := b.NodeInfo
	e := &enrEntry{
		ForkID: forkid.NewID(b.Chain, info.Genesis, uint64(info.NumOfBlocks)),
	}

	return e
}

// Dial candidates

func operaDialCandidates() enode.Iterator {
	var config gossip.Config

	dnsclient := dnsdisc.NewClient(dnsdisc.Config{})

	urls := config.OperaDiscoveryURLs
	it, err := dnsclient.NewIterator(urls...)
	if err != nil {
		panic(err)
	}

	return it
}
