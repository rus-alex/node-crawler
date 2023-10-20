package p2p

import (
	"crypto/ecdsa"
	"database/sql"
	"math"
	"strings"
	"time"

	"github.com/Fantom-foundation/go-opera/cmd/opera/launcher"
	"github.com/Fantom-foundation/go-opera/opera/genesisstore"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/oschwald/geoip2-golang"

	"github.com/ethereum/node-crawler/pkg/common"
)

type Crawler struct {
	server  *p2p.Server
	backend *ProbeBackend

	nodes chan *common.NodeJSON
	done  chan struct{}
}

func NewCrawler(
	genesis *genesisstore.Store,
	_ string,
	listenAddr string,
	nodeKey string,
	bootnodes []string,
	_ time.Duration,
	workers uint64,
	db *sql.DB,
	geoipDB *geoip2.Reader,
	nodeDB *enode.DB,
) *Crawler {
	c := &Crawler{
		nodes: make(chan *common.NodeJSON, workers),
		done:  make(chan struct{}),
	}

	c.backend = NewProbeBackend(c.nodes)
	c.backend.LoadGenesis(genesis)

	cfg := launcher.NodeDefaultConfig.P2P
	cfg.Protocols = ProbeProtocols(c.backend)
	cfg.ListenAddr = listenAddr
	cfg.PrivateKey = parseKey(nodeKey)

	for _, url := range bootnodes {
		node := eNode(url)
		cfg.BootstrapNodes = append(cfg.BootstrapNodes, node)
		cfg.BootstrapNodesV5 = append(cfg.BootstrapNodesV5, node)
	}

	cfg.MaxPeers = math.MaxInt

	c.server = &p2p.Server{
		Config: cfg,
	}
	return c
}

func (c *Crawler) Start(input common.NodeSet, onUpdatedSet func(common.NodeSet)) {
	known := make([]*enode.Node, 0, len(input))
	for _, n := range input {
		known = append(known, n.N)
	}
	c.server.Config.BootstrapNodes = append(c.server.Config.BootstrapNodes, known...)
	c.server.Config.BootstrapNodesV5 = append(c.server.Config.BootstrapNodesV5, known...)

	err := c.server.Start()
	if err != nil {
		panic(err)
	}
	// process new nodes
	go func() {
		// Copy input to output initially. Any nodes that fail validation
		// will be dropped from output during the run.
		output := make(common.NodeSet, len(input))
		for id, n := range input {
			output[id] = n
		}

		updated := 0
		for {
			select {
			case n := <-c.nodes:
				// process the node
				c.updateNode(output, n.N, nil) // TODO: valid error
				if updated%10 == 0 {
					onUpdatedSet(output)
				}
			case <-c.done:
				onUpdatedSet(output)
				return
			}
		}
	}()
}

func (c *Crawler) Stop() {
	c.server.Stop()
	c.backend.Close()
	close(c.done)
}

func parseKey(s string) (key *ecdsa.PrivateKey) {
	var err error
	if s != "" {
		key, err = crypto.HexToECDSA(s)
	} else {
		key, err = crypto.GenerateKey()
	}

	if err != nil {
		panic(err)
	}
	return
}

func eNode(url string) *enode.Node {
	if !strings.HasPrefix(url, "enode://") {
		url = "enode://" + url
	}
	n, err := enode.Parse(enode.ValidSchemes, url)
	if err != nil {
		panic(err)
	}
	return n
}
