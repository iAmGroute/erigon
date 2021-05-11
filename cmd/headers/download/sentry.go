package download

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/golang/protobuf/ptypes/empty"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_recovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/core/forkid"
	"github.com/ledgerwatch/turbo-geth/crypto"
	"github.com/ledgerwatch/turbo-geth/eth/protocols/eth"
	"github.com/ledgerwatch/turbo-geth/gointerfaces"
	proto_sentry "github.com/ledgerwatch/turbo-geth/gointerfaces/sentry"
	proto_types "github.com/ledgerwatch/turbo-geth/gointerfaces/types"
	"github.com/ledgerwatch/turbo-geth/log"
	"github.com/ledgerwatch/turbo-geth/metrics"
	"github.com/ledgerwatch/turbo-geth/p2p"
	"github.com/ledgerwatch/turbo-geth/p2p/dnsdisc"
	"github.com/ledgerwatch/turbo-geth/p2p/enode"
	"github.com/ledgerwatch/turbo-geth/p2p/nat"
	"github.com/ledgerwatch/turbo-geth/p2p/netutil"
	"github.com/ledgerwatch/turbo-geth/params"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/types/known/emptypb"
)

const (
	// handshakeTimeout is the maximum allowed time for the `eth` handshake to
	// complete before dropping the connection.= as malicious.
	handshakeTimeout  = 5 * time.Second
	maxPermitsPerPeer = 8 // How many outstanding requests per peer we may have
)

func nodeKey() *ecdsa.PrivateKey {
	keyfile := "nodekey"
	if key, err := crypto.LoadECDSA(keyfile); err == nil {
		return key
	}
	// No persistent key found, generate and store a new one.
	key, err := crypto.GenerateKey()
	if err != nil {
		log.Crit(fmt.Sprintf("Failed to generate node key: %v", err))
	}
	if err := crypto.SaveECDSA(keyfile, key); err != nil {
		log.Error(fmt.Sprintf("Failed to persist node key: %v", err))
	}
	return key
}

func makeP2PServer(
	ctx context.Context,
	nodeName string,
	readNodeInfo func() *eth.NodeInfo,
	natSetting string,
	port int,
	peers *sync.Map,
	peerHeightMap *sync.Map,
	peerPermitMap *sync.Map,
	peerRwMap *sync.Map,
	genesisHash common.Hash,
	statusFn func() *proto_sentry.StatusData,
	receiveCh chan<- StreamMsg,
	receiveUploadCh chan<- StreamMsg,
	receiveTxCh chan<- StreamMsg,
) (*p2p.Server, error) {
	client := dnsdisc.NewClient(dnsdisc.Config{})

	dns := params.KnownDNSNetwork(genesisHash, "all")
	dialCandidates, err := client.NewIterator(dns)
	if err != nil {
		return nil, fmt.Errorf("create discovery candidates: %v", err)
	}

	serverKey := nodeKey()
	p2pConfig := p2p.Config{}
	natif, err := nat.Parse(natSetting)
	if err != nil {
		return nil, fmt.Errorf("invalid nat option %s: %v", natSetting, err)
	}
	p2pConfig.NAT = natif
	p2pConfig.PrivateKey = serverKey

	p2pConfig.Name = nodeName
	p2pConfig.Logger = log.New()
	p2pConfig.MaxPeers = 100
	p2pConfig.Protocols = []p2p.Protocol{}
	p2pConfig.NodeDatabase = fmt.Sprintf("nodes_%x", genesisHash)
	p2pConfig.ListenAddr = fmt.Sprintf(":%d", port)
	var urls []string
	switch genesisHash {
	case params.MainnetGenesisHash:
		urls = params.MainnetBootnodes
	case params.RopstenGenesisHash:
		urls = params.RopstenBootnodes
	case params.GoerliGenesisHash:
		urls = params.GoerliBootnodes
	}
	p2pConfig.BootstrapNodes = make([]*enode.Node, 0, len(urls))
	for _, url := range urls {
		if url != "" {
			node, err := enode.Parse(enode.ValidSchemes, url)
			if err != nil {
				log.Crit("Bootstrap URL invalid", "enode", url, "err", err)
				continue
			}
			p2pConfig.BootstrapNodes = append(p2pConfig.BootstrapNodes, node)
		}
	}

	p2pConfig.Protocols = MakeProtocols(ctx, readNodeInfo, dialCandidates, peers, peerHeightMap, peerPermitMap, peerRwMap, statusFn, receiveCh, receiveUploadCh, receiveTxCh)
	return &p2p.Server{Config: p2pConfig}, nil
}

func MakeProtocols(ctx context.Context,
	readNodeInfo func() *eth.NodeInfo,
	dialCandidates enode.Iterator,
	peers *sync.Map,
	peerHeightMap *sync.Map,
	peerPermitMap *sync.Map,
	peerRwMap *sync.Map,
	statusFn func() *proto_sentry.StatusData,
	receiveCh chan<- StreamMsg,
	receiveUploadCh chan<- StreamMsg,
	receiveTxCh chan<- StreamMsg,
) []p2p.Protocol {
	return []p2p.Protocol{
		{
			Name:           eth.ProtocolName,
			Version:        eth.ProtocolVersions[0],
			Length:         17,
			DialCandidates: dialCandidates,
			Run: func(peer *p2p.Peer, rw p2p.MsgReadWriter) error {
				peerID := peer.ID().String()
				log.Debug(fmt.Sprintf("[%s] Start with peer", peerID))
				peers.Store(peerID, rw)
				peerRwMap.Store(peerID, rw)
				if err := runPeer(
					ctx,
					peerHeightMap,
					peerPermitMap,
					peerRwMap,
					peer,
					eth.ProtocolVersions[0], // version == eth66
					eth.ProtocolVersions[0], // minVersion == eth66
					statusFn(),
					receiveCh,
					receiveUploadCh,
					receiveTxCh,
				); err != nil {
					log.Debug(fmt.Sprintf("[%s] Error while running peer: %v", peerID, err))
				}
				peers.Delete(peerID)
				peerHeightMap.Delete(peerID)
				peerPermitMap.Delete(peerID)
				peerRwMap.Delete(peerID)
				return nil
			},
			NodeInfo: func() interface{} {
				return readNodeInfo()
			},
			PeerInfo: func(id enode.ID) interface{} {
				p, ok := peers.Load(id.String())
				if !ok {
					return nil
				}
				return p.(*p2p.Peer).Info()
			},
			//Attributes: []enr.Entry{eth.CurrentENREntry(chainConfig, genesisHash, headHeight)},
		},
	}
}

func handShake(
	ctx context.Context,
	status *proto_sentry.StatusData,
	peerID string,
	rw p2p.MsgReadWriter,
	version uint,
	minVersion uint,
) error {
	if status == nil {
		return fmt.Errorf("could not get status message from core for peer %s connection", peerID)
	}

	// Send out own handshake in a new thread
	errc := make(chan error, 2)

	// Convert proto status data into the one required by devp2p
	genesisHash := gointerfaces.ConvertH256ToHash(status.ForkData.Genesis)
	go func() {
		errc <- p2p.Send(rw, eth.StatusMsg, &eth.StatusPacket{
			ProtocolVersion: uint32(version),
			NetworkID:       status.NetworkId,
			TD:              gointerfaces.ConvertH256ToUint256Int(status.TotalDifficulty).ToBig(),
			Head:            gointerfaces.ConvertH256ToHash(status.BestHash),
			Genesis:         genesisHash,
			ForkID:          forkid.NewIDFromForks(status.ForkData.Forks, genesisHash, status.MaxBlock),
		})
	}()
	var readStatus = func() error {
		forkFilter := forkid.NewFilterFromForks(status.ForkData.Forks, genesisHash, status.MaxBlock)
		networkID := status.NetworkId
		// Read handshake message
		msg, err1 := rw.ReadMsg()
		if err1 != nil {
			return err1
		}

		if msg.Code != eth.StatusMsg {
			msg.Discard()
			return fmt.Errorf("first msg has code %x (!= %x)", msg.Code, eth.StatusMsg)
		}
		if msg.Size > eth.ProtocolMaxMsgSize {
			msg.Discard()
			return fmt.Errorf("message is too large %d, limit %d", msg.Size, eth.ProtocolMaxMsgSize)
		}
		// Decode the handshake and make sure everything matches
		var reply eth.StatusPacket
		if err1 = msg.Decode(&reply); err1 != nil {
			msg.Discard()
			return fmt.Errorf("decode message %v: %v", msg, err1)
		}
		msg.Discard()
		if reply.NetworkID != networkID {
			return fmt.Errorf("network id does not match: theirs %d, ours %d", reply.NetworkID, networkID)
		}
		if uint(reply.ProtocolVersion) < minVersion {
			return fmt.Errorf("version is less than allowed minimum: theirs %d, min %d", reply.ProtocolVersion, minVersion)
		}
		if reply.Genesis != genesisHash {
			return fmt.Errorf("genesis hash does not match: theirs %x, ours %x", reply.Genesis, genesisHash)
		}
		if err1 = forkFilter(reply.ForkID); err1 != nil {
			return fmt.Errorf("%v", err1)
		}
		return nil
	}
	go func() {
		errc <- readStatus()
	}()

	timeout := time.NewTimer(handshakeTimeout)
	defer timeout.Stop()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errc:
			if err != nil {
				return err
			}
		case <-timeout.C:
			return p2p.DiscReadTimeout
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func runPeer(
	ctx context.Context,
	peerHeightMap *sync.Map,
	peerPermitMap *sync.Map,
	peerRwMap *sync.Map,
	peer *p2p.Peer,
	version uint,
	minVersion uint,
	status *proto_sentry.StatusData,
	receiveCh chan<- StreamMsg,
	receiveUploadCh chan<- StreamMsg,
	receiveTxCh chan<- StreamMsg,
) error {
	peerID := peer.ID().String()
	rwRaw, ok := peerRwMap.Load(peerID)
	if !ok {
		return fmt.Errorf("peer has been penalized")
	}
	rw, _ := rwRaw.(p2p.MsgReadWriter)

	if err := handShake(ctx, status, peerID, rw, version, minVersion); err != nil {
		return fmt.Errorf("handshake to peer %s: %v", peerID, err)
	}
	log.Debug(fmt.Sprintf("[%s] Received status message OK", peerID), "name", peer.Name())

	for {
		var err error
		if err = common.Stopped(ctx.Done()); err != nil {
			return err
		}

		if _, ok := peerRwMap.Load(peerID); !ok {
			return fmt.Errorf("peer has been penalized")
		}
		msg, err := rw.ReadMsg()
		if err != nil {
			return fmt.Errorf("reading message: %v", err)
		}
		if msg.Size > eth.ProtocolMaxMsgSize {
			msg.Discard()
			return fmt.Errorf("message is too large %d, limit %d", msg.Size, eth.ProtocolMaxMsgSize)
		}
		switch msg.Code {
		case eth.StatusMsg:
			msg.Discard()
			// Status messages should never arrive after the handshake
			return fmt.Errorf("uncontrolled status message")
		case eth.GetBlockHeadersMsg:
			b := make([]byte, msg.Size)
			if _, err := io.ReadFull(msg.Payload, b); err != nil {
				log.Error(fmt.Sprintf("%s: reading msg into bytes: %v", peerID, err))
			}
			trySend(receiveUploadCh, &StreamMsg{b, peerID, "GetBlockHeadersMsg", proto_sentry.MessageId_GetBlockHeaders})
		case eth.BlockHeadersMsg:
			// Peer responded or sent message - reset the "back off" timer
			permitsRaw, _ := peerPermitMap.Load(peerID)
			permits, _ := permitsRaw.(int)
			if permits < maxPermitsPerPeer {
				permits++
				peerPermitMap.Store(peerID, permits)
			}
			b := make([]byte, msg.Size)
			if _, err := io.ReadFull(msg.Payload, b); err != nil {
				log.Error(fmt.Sprintf("%s: reading msg into bytes: %v", peerID, err))
			}
			trySend(receiveCh, &StreamMsg{b, peerID, "BlockHeadersMsg", proto_sentry.MessageId_BlockHeaders})
		case eth.GetBlockBodiesMsg:
			b := make([]byte, msg.Size)
			if _, err := io.ReadFull(msg.Payload, b); err != nil {
				log.Error(fmt.Sprintf("%s: reading msg into bytes: %v", peerID, err))
			}
			trySend(receiveUploadCh, &StreamMsg{b, peerID, "GetBlockBodiesMsg", proto_sentry.MessageId_GetBlockBodies})
		case eth.BlockBodiesMsg:
			// Peer responded or sent message - reset the "back off" timer
			permitsRaw, _ := peerPermitMap.Load(peerID)
			permits, _ := permitsRaw.(int)
			if permits < maxPermitsPerPeer {
				permits++
				peerPermitMap.Store(peerID, permits)
			}
			b := make([]byte, msg.Size)
			if _, err := io.ReadFull(msg.Payload, b); err != nil {
				log.Error(fmt.Sprintf("%s: reading msg into bytes: %v", peerID, err))
			}
			trySend(receiveCh, &StreamMsg{b, peerID, "BlockBodiesMsg", proto_sentry.MessageId_BlockBodies})
		case eth.GetNodeDataMsg:
			//log.Info(fmt.Sprintf("[%s] GetNodeData", peerID))
		case eth.GetReceiptsMsg:
			//log.Info(fmt.Sprintf("[%s] GetReceiptsMsg", peerID))
		case eth.ReceiptsMsg:
			//log.Info(fmt.Sprintf("[%s] ReceiptsMsg", peerID))
		case eth.NewBlockHashesMsg:
			b := make([]byte, msg.Size)
			if _, err := io.ReadFull(msg.Payload, b); err != nil {
				log.Error(fmt.Sprintf("%s: reading msg into bytes: %v", peerID, err))
			}
			trySend(receiveCh, &StreamMsg{b, peerID, "NewBlockHashesMsg", proto_sentry.MessageId_NewBlockHashes})
		case eth.NewBlockMsg:
			b := make([]byte, msg.Size)
			if _, err := io.ReadFull(msg.Payload, b); err != nil {
				log.Error(fmt.Sprintf("%s: reading msg into bytes: %v", peerID, err))
			}
			trySend(receiveCh, &StreamMsg{b, peerID, "NewBlockMsg", proto_sentry.MessageId_NewBlock})
		case eth.NewPooledTransactionHashesMsg:
			b := make([]byte, msg.Size)
			if _, err := io.ReadFull(msg.Payload, b); err != nil {
				log.Error(fmt.Sprintf("%s: reading msg into bytes: %v", peerID, err))
			}
			trySend(receiveTxCh, &StreamMsg{b, peerID, "NewPooledTransactionHashesMsg", proto_sentry.MessageId_NewPooledTransactionHashes})
			//var hashes []common.Hash
			//if err := msg.Decode(&hashes); err != nil {
			//	return fmt.Errorf("decode NewPooledTransactionHashesMsg %v: %v", msg, err)
			//}
			//var hashesStr strings.Builder
			//for _, hash := range hashes {
			//	if hashesStr.Len() > 0 {
			//		hashesStr.WriteString(",")
			//	}
			//	hashesStr.WriteString(fmt.Sprintf("%x-%x", hash[:4], hash[28:]))
			//}
			//log.Info(fmt.Sprintf("[%s] NewPooledTransactionHashesMsg {%s}", peerID, hashesStr.String()))
		case eth.GetPooledTransactionsMsg:
			b := make([]byte, msg.Size)
			if _, err := io.ReadFull(msg.Payload, b); err != nil {
				log.Error(fmt.Sprintf("%s: reading msg into bytes: %v", peerID, err))
			}
			trySend(receiveTxCh, &StreamMsg{b, peerID, "GetPooledTransactionsMsg", proto_sentry.MessageId_GetPooledTransactions})
			//log.Info(fmt.Sprintf("[%s] GetPooledTransactionsMsg", peerID)
		case eth.TransactionsMsg:
			b := make([]byte, msg.Size)
			if _, err := io.ReadFull(msg.Payload, b); err != nil {
				log.Error(fmt.Sprintf("%s: reading msg into bytes: %v", peerID, err))
			}
			trySend(receiveTxCh, &StreamMsg{b, peerID, "TransactionsMsg", proto_sentry.MessageId_Transactions})
			//var txs eth.TransactionsPacket
			//if err := msg.Decode(&txs); err != nil {
			//	return fmt.Errorf("decode TransactionMsg %v: %v", msg, err)
			//}
			//var hashesStr strings.Builder
			//for _, tx := range txs {
			//	if hashesStr.Len() > 0 {
			//		hashesStr.WriteString(",")
			//	}
			//	hash := tx.Hash()
			//	hashesStr.WriteString(fmt.Sprintf("%x-%x", hash[:4], hash[28:]))
			//}
			//log.Info(fmt.Sprintf("[%s] TransactionMsg {%s}", peerID, hashesStr.String()))
		case eth.PooledTransactionsMsg:
			b := make([]byte, msg.Size)
			if _, err := io.ReadFull(msg.Payload, b); err != nil {
				log.Error(fmt.Sprintf("%s: reading msg into bytes: %v", peerID, err))
			}
			trySend(receiveTxCh, &StreamMsg{b, peerID, "PooledTransactionsMsg", proto_sentry.MessageId_PooledTransactions})
			//log.Info(fmt.Sprintf("[%s] PooledTransactionsMsg", peerID)
		default:
			log.Error(fmt.Sprintf("[%s] Unknown message code: %d", peerID, msg.Code))
		}
		msg.Discard()
	}
}

func rootContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(ch)

		select {
		case <-ch:
			log.Info("Got interrupt, shutting down...")
		case <-ctx.Done():
		}

		cancel()
	}()
	return ctx
}

func grpcSentryServer(ctx context.Context, sentryAddr string) (*SentryServerImpl, error) {
	// STARTING GRPC SERVER
	log.Info("Starting Sentry P2P server", "on", sentryAddr)
	listenConfig := net.ListenConfig{
		Control: func(network, address string, _ syscall.RawConn) error {
			log.Info("Sentry P2P received connection", "via", network, "from", address)
			return nil
		},
	}
	lis, err := listenConfig.Listen(ctx, "tcp", sentryAddr)
	if err != nil {
		return nil, fmt.Errorf("could not create Sentry P2P listener: %w, addr=%s", err, sentryAddr)
	}
	var (
		streamInterceptors []grpc.StreamServerInterceptor
		unaryInterceptors  []grpc.UnaryServerInterceptor
	)
	streamInterceptors = append(streamInterceptors, grpc_recovery.StreamServerInterceptor())
	unaryInterceptors = append(unaryInterceptors, grpc_recovery.UnaryServerInterceptor())
	if metrics.Enabled {
		streamInterceptors = append(streamInterceptors, grpc_prometheus.StreamServerInterceptor)
		unaryInterceptors = append(unaryInterceptors, grpc_prometheus.UnaryServerInterceptor)
	}

	var grpcServer *grpc.Server
	//cpus := uint32(runtime.GOMAXPROCS(-1))
	opts := []grpc.ServerOption{
		//grpc.NumStreamWorkers(cpus), // reduce amount of goroutines
		grpc.WriteBufferSize(1024), // reduce buffers to save mem
		grpc.ReadBufferSize(1024),
		grpc.MaxConcurrentStreams(100), // to force clients reduce concurrency level
		// Don't drop the connection, settings accordign to this comment on GitHub
		// https://github.com/grpc/grpc-go/issues/3171#issuecomment-552796779
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(streamInterceptors...)),
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(unaryInterceptors...)),
	}
	grpcServer = grpc.NewServer(opts...)

	sentryServer := NewSentryServer(ctx)
	proto_sentry.RegisterSentryServer(grpcServer, sentryServer)
	if metrics.Enabled {
		grpc_prometheus.Register(grpcServer)
	}
	go func() {
		if err1 := grpcServer.Serve(lis); err1 != nil {
			log.Error("Sentry P2P server fail", "err", err1)
		}
	}()
	return sentryServer, nil
}

func NewSentryServer(ctx context.Context) *SentryServerImpl {
	return &SentryServerImpl{
		ctx:             ctx,
		ReceiveCh:       make(chan StreamMsg, 1024),
		ReceiveUploadCh: make(chan StreamMsg, 1024),
		ReceiveTxCh:     make(chan StreamMsg, 1024),
		stopCh:          make(chan struct{}),
		uploadStopCh:    make(chan struct{}),
		txStopCh:        make(chan struct{}),
	}
}

func p2pServer(ctx context.Context,
	nodeName string,
	readNodeInfo func() *eth.NodeInfo,
	sentryServer *SentryServerImpl,
	natSetting string, port int, staticPeers []string, discovery bool, netRestrict string,
	genesisHash common.Hash,
) (*p2p.Server, error) {
	server, err := makeP2PServer(
		ctx,
		nodeName,
		readNodeInfo,
		natSetting,
		port,
		&sentryServer.Peers,
		&sentryServer.PeerHeightMap,
		&sentryServer.PeerPermitMap,
		&sentryServer.PeerRwMap,
		genesisHash,
		sentryServer.GetStatus,
		sentryServer.ReceiveCh,
		sentryServer.ReceiveUploadCh,
		sentryServer.ReceiveTxCh,
	)
	if err != nil {
		return nil, err
	}

	enodes := make([]*enode.Node, len(staticPeers))
	for i, e := range staticPeers {
		enodes[i] = enode.MustParse(e)
	}

	server.StaticNodes = enodes
	server.NoDiscovery = discovery

	if netRestrict != "" {
		server.NetRestrict = new(netutil.Netlist)
		server.NetRestrict.Add(netRestrict)
	}
	return server, nil
}

// Sentry creates and runs standalone sentry
func Sentry(natSetting string, port int, sentryAddr string, staticPeers []string, discovery bool, netRestrict string) error {
	ctx := rootContext()

	sentryServer, err := grpcSentryServer(ctx, sentryAddr)
	if err != nil {
		return err
	}
	sentryServer.natSetting = natSetting
	sentryServer.port = port
	sentryServer.staticPeers = staticPeers
	sentryServer.discovery = discovery
	sentryServer.netRestrict = netRestrict

	<-ctx.Done()
	return nil
}

type StreamMsg struct {
	b       []byte
	peerID  string
	msgName string
	msgId   proto_sentry.MessageId
}

type SentryServerImpl struct {
	proto_sentry.UnimplementedSentryServer
	ctx             context.Context
	natSetting      string
	port            int
	staticPeers     []string
	discovery       bool
	netRestrict     string
	Peers           sync.Map
	PeerHeightMap   sync.Map
	PeerRwMap       sync.Map
	PeerPermitMap   sync.Map // Keeps track of request permits for each peer - for request load balancing
	statusData      *proto_sentry.StatusData
	P2pServer       *p2p.Server
	nodeName        string
	ReceiveCh       chan StreamMsg
	stopCh          chan struct{} // Channel used to signal (by closing) to the receiver on `receiveCh` to stop reading
	ReceiveUploadCh chan StreamMsg
	uploadStopCh    chan struct{} // Channel used to signal (by closing) to the receiver on `receiveUploadCh` to stop reading
	ReceiveTxCh     chan StreamMsg
	txStopCh        chan struct{} // Channel used to signal (by closing) to the receiver on `receiveTxCh` to stop reading
	lock            sync.RWMutex
}

func (ss *SentryServerImpl) PenalizePeer(_ context.Context, req *proto_sentry.PenalizePeerRequest) (*empty.Empty, error) {
	//log.Warn("Received penalty", "kind", req.GetPenalty().Descriptor().FullName, "from", fmt.Sprintf("%s", req.GetPeerId()))
	strId := string(gointerfaces.ConvertH512ToBytes(req.PeerId))
	ss.PeerRwMap.Delete(strId)
	ss.PeerPermitMap.Delete(strId)
	ss.PeerHeightMap.Delete(strId)
	return &empty.Empty{}, nil
}

func (ss *SentryServerImpl) PeerMinBlock(_ context.Context, req *proto_sentry.PeerMinBlockRequest) (*empty.Empty, error) {
	peerID := string(gointerfaces.ConvertH512ToBytes(req.PeerId))
	x, _ := ss.PeerHeightMap.Load(peerID)
	highestBlock, _ := x.(uint64)
	if req.MinBlock > highestBlock {
		ss.PeerHeightMap.Store(peerID, req.MinBlock)
	}
	return &empty.Empty{}, nil
}

func (ss *SentryServerImpl) findPeer(minBlock uint64) (string, bool) {
	// Choose a peer that we can send this request to, with maximum number of permits
	var foundPeerID string
	var maxPermits int
	ss.PeerHeightMap.Range(func(key, value interface{}) bool {
		valUint, _ := value.(uint64)
		if valUint >= minBlock {
			peerID := key.(string)
			permitsRaw, _ := ss.PeerPermitMap.Load(peerID)
			permits, _ := permitsRaw.(int)
			if permits > maxPermits {
				maxPermits = permits
				foundPeerID = peerID
			}
		}
		return true
	})
	return foundPeerID, maxPermits > 0
}

func (ss *SentryServerImpl) SendMessageByMinBlock(_ context.Context, inreq *proto_sentry.SendMessageByMinBlockRequest) (*proto_sentry.SentPeers, error) {
	peerID, found := ss.findPeer(inreq.MinBlock)
	if !found {
		return &proto_sentry.SentPeers{}, nil
	}
	rwRaw, _ := ss.PeerRwMap.Load(peerID)
	rw, _ := rwRaw.(p2p.MsgReadWriter)
	if rw == nil {
		ss.PeerHeightMap.Delete(peerID)
		ss.PeerPermitMap.Delete(peerID)
		ss.PeerRwMap.Delete(peerID)
		return &proto_sentry.SentPeers{}, fmt.Errorf("sendMessageByMinBlock find rw for peer %s", peerID)
	}
	var msgcode uint64
	switch inreq.Data.Id {
	case proto_sentry.MessageId_GetBlockHeaders:
		msgcode = eth.GetBlockHeadersMsg
	case proto_sentry.MessageId_GetBlockBodies:
		msgcode = eth.GetBlockBodiesMsg
	default:
		return &proto_sentry.SentPeers{}, fmt.Errorf("sendMessageByMinBlock not implemented for message Id: %s", inreq.Data.Id)
	}
	if err := rw.WriteMsg(p2p.Msg{Code: msgcode, Size: uint32(len(inreq.Data.Data)), Payload: bytes.NewReader(inreq.Data.Data)}); err != nil {
		ss.PeerHeightMap.Delete(peerID)
		ss.PeerPermitMap.Delete(peerID)
		ss.PeerRwMap.Delete(peerID)
		return &proto_sentry.SentPeers{}, fmt.Errorf("sendMessageByMinBlock to peer %s: %v", peerID, err)
	}
	permitsRaw, _ := ss.PeerPermitMap.Load(peerID)
	permits, _ := permitsRaw.(int)
	if permits > 0 {
		ss.PeerPermitMap.Store(peerID, permits-1)
	}
	return &proto_sentry.SentPeers{Peers: []*proto_types.H512{gointerfaces.ConvertBytesToH512([]byte(peerID))}}, nil
}

func (ss *SentryServerImpl) SendMessageById(_ context.Context, inreq *proto_sentry.SendMessageByIdRequest) (*proto_sentry.SentPeers, error) {
	peerID := string(gointerfaces.ConvertH512ToBytes(inreq.PeerId))
	rwRaw, ok := ss.PeerRwMap.Load(peerID)
	if !ok {
		return &proto_sentry.SentPeers{}, fmt.Errorf("peer not found: %s", peerID)
	}
	rw, _ := rwRaw.(p2p.MsgReadWriter)
	var msgcode uint64
	switch inreq.Data.Id {
	case proto_sentry.MessageId_GetBlockHeaders:
		msgcode = eth.GetBlockHeadersMsg
	case proto_sentry.MessageId_BlockHeaders:
		msgcode = eth.BlockHeadersMsg
	case proto_sentry.MessageId_BlockBodies:
		msgcode = eth.BlockBodiesMsg
	case proto_sentry.MessageId_GetReceipts:
		msgcode = eth.GetReceiptsMsg
	case proto_sentry.MessageId_Receipts:
		msgcode = eth.ReceiptsMsg
	case proto_sentry.MessageId_PooledTransactions:
		msgcode = eth.PooledTransactionsMsg
	case proto_sentry.MessageId_GetPooledTransactions:
		msgcode = eth.GetPooledTransactionsMsg
	default:
		return &proto_sentry.SentPeers{}, fmt.Errorf("sendMessageById not implemented for message Id: %s", inreq.Data.Id)
	}

	if err := rw.WriteMsg(p2p.Msg{Code: msgcode, Size: uint32(len(inreq.Data.Data)), Payload: bytes.NewReader(inreq.Data.Data)}); err != nil {
		ss.PeerHeightMap.Delete(peerID)
		ss.PeerPermitMap.Delete(peerID)
		ss.PeerRwMap.Delete(peerID)
		return &proto_sentry.SentPeers{}, fmt.Errorf("sendMessageById to peer %s: %v", peerID, err)
	}
	return &proto_sentry.SentPeers{Peers: []*proto_types.H512{inreq.PeerId}}, nil
}

func (ss *SentryServerImpl) SendMessageToRandomPeers(ctx context.Context, req *proto_sentry.SendMessageToRandomPeersRequest) (*proto_sentry.SentPeers, error) {
	var msgcode uint64
	switch req.Data.Id {
	case proto_sentry.MessageId_NewBlock:
		msgcode = eth.NewBlockMsg
	case proto_sentry.MessageId_NewBlockHashes:
		msgcode = eth.NewBlockHashesMsg
	default:
		return &proto_sentry.SentPeers{}, fmt.Errorf("sendMessageToRandomPeers not implemented for message Id: %s", req.Data.Id)
	}

	amount := uint64(0)
	ss.PeerRwMap.Range(func(key, value interface{}) bool {
		amount++
		return true
	})
	if req.MaxPeers > amount {
		amount = req.MaxPeers
	}

	// Send the block to a subset of our peers
	sendToAmount := int(math.Sqrt(float64(amount)))
	i := 0
	var innerErr error
	reply := &proto_sentry.SentPeers{Peers: []*proto_types.H512{}}
	ss.PeerRwMap.Range(func(key, value interface{}) bool {
		peerID := key.(string)
		rw, _ := value.(p2p.MsgReadWriter)
		if err := rw.WriteMsg(p2p.Msg{Code: msgcode, Size: uint32(len(req.Data.Data)), Payload: bytes.NewReader(req.Data.Data)}); err != nil {
			ss.PeerHeightMap.Delete(peerID)
			ss.PeerPermitMap.Delete(peerID)
			ss.PeerRwMap.Delete(peerID)
			innerErr = err
			return false
		}
		reply.Peers = append(reply.Peers, gointerfaces.ConvertBytesToH512([]byte(peerID)))
		i++
		return sendToAmount <= i
	})
	if innerErr != nil {
		return reply, fmt.Errorf("sendMessageToRandomPeers to peer %w", innerErr)
	}
	return reply, nil
}

func (ss *SentryServerImpl) SendMessageToAll(ctx context.Context, req *proto_sentry.OutboundMessageData) (*proto_sentry.SentPeers, error) {
	var msgcode uint64
	switch req.Id {
	case proto_sentry.MessageId_NewBlock:
		msgcode = eth.NewBlockMsg
	case proto_sentry.MessageId_NewBlockHashes:
		msgcode = eth.NewBlockHashesMsg
	default:
		return &proto_sentry.SentPeers{}, fmt.Errorf("sendMessageToRandomPeers not implemented for message Id: %s", req.Id)
	}

	var innerErr error
	reply := &proto_sentry.SentPeers{Peers: []*proto_types.H512{}}
	ss.PeerRwMap.Range(func(key, value interface{}) bool {
		peerID := key.(string)
		rw, _ := value.(p2p.MsgReadWriter)
		if err := rw.WriteMsg(p2p.Msg{Code: msgcode, Size: uint32(len(req.Data)), Payload: bytes.NewReader(req.Data)}); err != nil {
			ss.PeerHeightMap.Delete(peerID)
			ss.PeerPermitMap.Delete(peerID)
			ss.PeerRwMap.Delete(peerID)
			innerErr = err
			return false
		}
		reply.Peers = append(reply.Peers, gointerfaces.ConvertBytesToH512([]byte(peerID)))
		return true
	})
	if innerErr != nil {
		return reply, fmt.Errorf("sendMessageToRandomPeers to peer %w", innerErr)
	}
	return reply, nil
}

func (ss *SentryServerImpl) SetStatus(_ context.Context, statusData *proto_sentry.StatusData) (*emptypb.Empty, error) {
	genesisHash := gointerfaces.ConvertH256ToHash(statusData.ForkData.Genesis)

	ss.lock.Lock()
	defer ss.lock.Unlock()
	init := ss.statusData == nil
	if init {
		var err error
		ss.P2pServer, err = p2pServer(ss.ctx, ss.nodeName, func() *eth.NodeInfo { return nil }, ss, ss.natSetting, ss.port, ss.staticPeers, ss.discovery, ss.netRestrict, genesisHash)
		if err != nil {
			return &empty.Empty{}, err
		}
		// Add protocol
		if err := ss.P2pServer.Start(); err != nil {
			return &empty.Empty{}, fmt.Errorf("could not start server: %w", err)
		}
	}
	genesisHash = gointerfaces.ConvertH256ToHash(statusData.ForkData.Genesis)
	ss.P2pServer.LocalNode().Set(eth.CurrentENREntryFromForks(statusData.ForkData.Forks, genesisHash, statusData.MaxBlock))
	ss.statusData = statusData
	return &empty.Empty{}, nil
}

func (ss *SentryServerImpl) GetStatus() *proto_sentry.StatusData {
	ss.lock.RLock()
	defer ss.lock.RUnlock()
	return ss.statusData
}

func (ss *SentryServerImpl) restartReceive() {
	ss.lock.Lock()
	defer ss.lock.Unlock()
	// Close previous channel and recreate
	close(ss.stopCh)
	ss.stopCh = make(chan struct{})
}

func (ss *SentryServerImpl) ReceiveMessages(_ *emptypb.Empty, server proto_sentry.Sentry_ReceiveMessagesServer) error {
	ss.restartReceive()
	for {
		select {
		case <-ss.stopCh:
			log.Warn("Finished receive messages")
			return nil
		case streamMsg := <-ss.ReceiveCh:
			outreq := proto_sentry.InboundMessage{
				PeerId: gointerfaces.ConvertBytesToH512([]byte(streamMsg.peerID)),
				Id:     streamMsg.msgId,
				Data:   streamMsg.b,
			}
			if err := server.Send(&outreq); err != nil {
				log.Error("Sending msg to core P2P failed", "msg", streamMsg.msgName, "error", err)
				return err
			}
		}
	}
}

func trySend(ch chan<- StreamMsg, msg *StreamMsg) {
	select {
	case ch <- *msg:
	default:
		log.Warn("Dropped stream message", "type", msg.msgName)
	}
}

func (ss *SentryServerImpl) restartUpload() {
	ss.lock.Lock()
	defer ss.lock.Unlock()
	// Close previous channel and recreate
	close(ss.uploadStopCh)
	ss.uploadStopCh = make(chan struct{})
}

func (ss *SentryServerImpl) restartTxs() {
	ss.lock.Lock()
	defer ss.lock.Unlock()
	// Close previous channel and recreate
	close(ss.txStopCh)
	ss.txStopCh = make(chan struct{})
}

func (ss *SentryServerImpl) ReceiveUploadMessages(_ *emptypb.Empty, server proto_sentry.Sentry_ReceiveUploadMessagesServer) error {
	// Close previous channel and recreate
	ss.restartUpload()
	for {
		select {
		case <-ss.uploadStopCh:
			log.Warn("Finished receive upload messages")
			return nil
		case streamMsg := <-ss.ReceiveUploadCh:
			outreq := proto_sentry.InboundMessage{
				PeerId: gointerfaces.ConvertBytesToH512([]byte(streamMsg.peerID)),
				Id:     streamMsg.msgId,
				Data:   streamMsg.b,
			}
			if err := server.Send(&outreq); err != nil {
				log.Error("Sending msg to core P2P failed", "msg", streamMsg.msgName, "error", err)
				return err
			}
		}
	}
}

func (ss *SentryServerImpl) ReceiveTxMessages(_ *emptypb.Empty, server proto_sentry.Sentry_ReceiveTxMessagesServer) error {
	// Close previous channel and recreate
	ss.restartTxs()
	for {
		select {
		case <-ss.txStopCh:
			log.Warn("Finished receive txs messages")
			return nil
		case streamMsg := <-ss.ReceiveTxCh:
			outreq := proto_sentry.InboundMessage{
				PeerId: gointerfaces.ConvertBytesToH512([]byte(streamMsg.peerID)),
				Id:     streamMsg.msgId,
				Data:   streamMsg.b,
			}
			if err := server.Send(&outreq); err != nil {
				log.Error("Sending msg to core P2P failed", "msg", streamMsg.msgName, "error", err)
				return err
			}
		}
	}
}