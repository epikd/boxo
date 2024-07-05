package network

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	bsmsg "github.com/ipfs/boxo/bitswap/message"
	bitswap_message_pb "github.com/ipfs/boxo/bitswap/message/pb"
	"github.com/ipfs/boxo/bitswap/network/internal"
	blocks "github.com/ipfs/go-block-format"

	cid "github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p/core/connmgr"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	peerstore "github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/core/routing"
	"github.com/libp2p/go-libp2p/p2p/protocol/ping"
	msgio "github.com/libp2p/go-msgio"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multistream"

	pool "github.com/libp2p/go-buffer-pool"

	"github.com/katzenpost/hpqc/nike"
	"github.com/katzenpost/hpqc/nike/x25519"
	kpsphinx "github.com/katzenpost/katzenpost/core/sphinx"
	"github.com/katzenpost/katzenpost/core/sphinx/commands"
	"github.com/katzenpost/katzenpost/core/sphinx/geo"
)

var log = logging.Logger("bitswap_network")

var connectTimeout = time.Second * 5

var (
	maxSendTimeout = 2 * time.Minute
	minSendTimeout = 10 * time.Second
	sendLatency    = 2 * time.Second
	minSendRate    = (100 * 1000) / 8 // 100kbit/s

)

// NewFromIpfsHost returns a BitSwapNetwork supported by underlying IPFS host.
func NewFromIpfsHost(host host.Host, r routing.ContentRouting, opts ...NetOpt) BitSwapNetwork {
	s := processSettings(opts...)
	nrHops := 2
	scheme := x25519.Scheme(rand.Reader)
	geom := geo.GeometryFromUserForwardPayloadLength(scheme, 512, true, nrHops)
	sphinx := kpsphinx.NewNIKESphinx(scheme, geom)
	surbmap := make(map[[16]byte][]byte)
	surbdest := make(map[[16]byte]peer.ID)
	serversurb := make(map[peer.ID][]byte)

	pub, priv, err := scheme.GenerateKeyPair()
	if err != nil {
		log.Infof("Key generation error.")
	}

	bitswapNetwork := impl{
		host:    host,
		routing: r,

		protocolBitswapNoVers:  s.ProtocolPrefix + ProtocolBitswapNoVers,
		protocolBitswapOneZero: s.ProtocolPrefix + ProtocolBitswapOneZero,
		protocolBitswapOneOne:  s.ProtocolPrefix + ProtocolBitswapOneOne,
		protocolBitswap:        s.ProtocolPrefix + ProtocolBitswap,
		protocolSphinx:         s.ProtocolPrefix + ProtocolSphinx,
		recsphinx:              sphinx,
		surbmap:                surbmap,
		surbdest:               surbdest,
		serversurb:             serversurb,
		scheme:                 scheme,
		pubk:                   pub,
		privk:                  priv,
		nrHops:                 nrHops,

		supportedProtocols: s.SupportedProtocols,
	}

	return &bitswapNetwork
}

func processSettings(opts ...NetOpt) Settings {
	s := Settings{SupportedProtocols: append([]protocol.ID(nil), internal.DefaultProtocols...)}
	for _, opt := range opts {
		opt(&s)
	}
	for i, proto := range s.SupportedProtocols {
		s.SupportedProtocols[i] = s.ProtocolPrefix + proto
	}
	return s
}

// impl transforms the ipfs network interface, which sends and receives
// NetMessage objects, into the bitswap network interface.
type impl struct {
	// NOTE: Stats must be at the top of the heap allocation to ensure 64bit
	// alignment.
	stats Stats

	host          host.Host
	routing       routing.ContentRouting
	connectEvtMgr *connectEventManager

	protocolBitswapNoVers  protocol.ID
	protocolBitswapOneZero protocol.ID
	protocolBitswapOneOne  protocol.ID
	protocolBitswap        protocol.ID
	protocolSphinx         protocol.ID

	supportedProtocols []protocol.ID

	// inbound messages from the network are forwarded to the receiver
	receivers []Receiver

	scheme    nike.Scheme
	privk     nike.PrivateKey
	pubk      nike.PublicKey
	recsphinx *kpsphinx.Sphinx
	nrHops    int

	clientlk sync.RWMutex
	serverlk sync.RWMutex

	// NodeID - PID
	nidpid map[[32]byte]peer.ID
	keys   map[peer.ID]nike.PublicKey

	// ID - decryptionkey
	surbmap map[[16]byte][]byte
	// ID - destination
	surbdest map[[16]byte]peer.ID
	// surbid - temp PID
	serversurb map[peer.ID][]byte
}

type streamMessageSender struct {
	to        peer.ID
	stream    network.Stream
	connected bool
	bsnet     *impl
	opts      *MessageSenderOpts
	dst       peer.ID
}

// Open a stream to the remote peer
func (s *streamMessageSender) Connect(ctx context.Context) (network.Stream, error) {
	if s.connected {
		return s.stream, nil
	}

	tctx, cancel := context.WithTimeout(ctx, s.opts.SendTimeout)
	defer cancel()

	if err := s.bsnet.ConnectTo(tctx, s.to); err != nil {
		return nil, err
	}

	stream, err := s.bsnet.newStreamToPeer(tctx, s.to)
	if err != nil {
		return nil, err
	}

	s.stream = stream
	s.connected = true
	return s.stream, nil
}

// Reset the stream
func (s *streamMessageSender) Reset() error {
	if s.stream != nil {
		err := s.stream.Reset()
		s.connected = false
		return err
	}
	return nil
}

// Close the stream
func (s *streamMessageSender) Close() error {
	return s.stream.Close()
}

// Indicates whether the peer supports HAVE / DONT_HAVE messages
func (s *streamMessageSender) SupportsHave() bool {
	return s.bsnet.SupportsHave(s.stream.Protocol())
}

// Send a message to the peer, attempting multiple times
func (s *streamMessageSender) SendMsg(ctx context.Context, msg bsmsg.BitSwapMessage) error {
	return s.multiAttempt(ctx, func() error {
		return s.send(ctx, msg)
	})
}

// Perform a function with multiple attempts, and a timeout
func (s *streamMessageSender) multiAttempt(ctx context.Context, fn func() error) error {
	// Try to call the function repeatedly
	var err error
	for i := 0; i < s.opts.MaxRetries; i++ {
		if err = fn(); err == nil {
			// Attempt was successful
			return nil
		}

		// Attempt failed

		// If the sender has been closed or the context cancelled, just bail out
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Protocol is not supported, so no need to try multiple times
		if errors.Is(err, multistream.ErrNotSupported[protocol.ID]{}) {
			s.bsnet.connectEvtMgr.MarkUnresponsive(s.to)
			return err
		}

		// Failed to send so reset stream and try again
		_ = s.Reset()

		// Failed too many times so mark the peer as unresponsive and return an error
		if i == s.opts.MaxRetries-1 {
			s.bsnet.connectEvtMgr.MarkUnresponsive(s.to)
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.opts.SendErrorBackoff):
			// wait a short time in case disconnect notifications are still propagating
			log.Infof("send message to %s failed but context was not Done: %s", s.to, err)
		}
	}
	return err
}

// Send a message to the peer
func (s *streamMessageSender) send(ctx context.Context, msg bsmsg.BitSwapMessage) error {
	start := time.Now()
	stream, err := s.Connect(ctx)
	if err != nil {
		log.Infof("failed to open stream to %s: %s", s.to, err)
		return err
	}

	// The send timeout includes the time required to connect
	// (although usually we will already have connected - we only need to
	// connect after a failed attempt to send)
	timeout := s.opts.SendTimeout - time.Since(start)
	if err = s.bsnet.msgToStream(ctx, stream, msg, timeout, s.dst); err != nil {
		log.Infof("failed to send message to %s: %s", s.to, err)
		return err
	}

	return nil
}

func (bsnet *impl) Self() peer.ID {
	return bsnet.host.ID()
}

func (bsnet *impl) Ping(ctx context.Context, p peer.ID) ping.Result {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	res := <-ping.Ping(ctx, bsnet.host, p)
	return res
}

func (bsnet *impl) Latency(p peer.ID) time.Duration {
	return bsnet.host.Peerstore().LatencyEWMA(p)
}

// Indicates whether the given protocol supports HAVE / DONT_HAVE messages
func (bsnet *impl) SupportsHave(proto protocol.ID) bool {
	switch proto {
	case bsnet.protocolBitswapOneOne, bsnet.protocolBitswapOneZero, bsnet.protocolBitswapNoVers:
		return false
	}
	return true
}

func (bsnet *impl) msgToStream(ctx context.Context, s network.Stream, msg bsmsg.BitSwapMessage, timeout time.Duration, p peer.ID) error {
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}

	if err := s.SetWriteDeadline(deadline); err != nil {
		log.Warnf("error setting deadline: %s", err)
	}
	log.Infof("Send Message: %v", msg.Wantlist())
	// Older Bitswap versions use a slightly different wire format so we need
	// to convert the message to the appropriate format depending on the remote
	// peer's Bitswap version.
	switch s.Protocol() {
	case bsnet.protocolSphinx:
		log.Infof("Sphinx send Message - to %v", s.Conn().RemotePeer())
		data, err := msg.ToProtoV1().Marshal()
		if err != nil {
			log.Infof("error: %s", err)
			return err
		}
		surbid := [16]byte{}
		_, err = io.ReadFull(rand.Reader, surbid[:])
		path, surbpath, err := bsnet.createPath(s.Conn().RemotePeer(), p, surbid)
		if err != nil {
			return err
		}

		geom := geo.GeometryFromUserForwardPayloadLength(bsnet.scheme, len(data), true, bsnet.nrHops)
		sphinx := kpsphinx.NewNIKESphinx(bsnet.scheme, geom)

		surb, key, err := sphinx.NewSURB(rand.Reader, surbpath)
		if err != nil {
			return err
		}

		bsnet.clientlk.Lock()
		bsnet.surbmap[surbid] = key
		bsnet.surbdest[surbid] = p
		bsnet.clientlk.Unlock()

		payload := make([]byte, 2, 2+geom.SURBLength+len(data))
		payload[0] = 1
		payload = append(payload, surb...)
		payload = append(payload, data...)

		pkt, err := sphinx.NewPacket(rand.Reader, path, []byte(payload))
		if err != nil {
			return err
		}

		size := len(pkt)

		buf := pool.Get(size + binary.MaxVarintLen64)
		defer pool.Put(buf)

		n := binary.PutUvarint(buf, uint64(size))
		copy(buf[n:], pkt[:])

		n += len(pkt)

		_, err = s.Write(buf[:n])
		if err != nil {
			log.Infof("Write error: %v", err)
			return err
		}
		//log.Infof("Written: %v", written)

	case bsnet.protocolBitswapOneOne, bsnet.protocolBitswap:
		if err := msg.ToNetV1(s); err != nil {
			log.Debugf("error: %s", err)
			return err
		}
	case bsnet.protocolBitswapOneZero, bsnet.protocolBitswapNoVers:
		log.Infof("Msg send.")
		if err := msg.ToNetV0(s); err != nil {
			log.Debugf("error: %s", err)
			return err
		}
	default:
		return fmt.Errorf("unrecognized protocol on remote: %s", s.Protocol())
	}

	atomic.AddUint64(&bsnet.stats.MessagesSent, 1)

	if err := s.SetWriteDeadline(time.Time{}); err != nil {
		log.Warnf("error resetting deadline: %s", err)
	}
	return nil
}

func (bsnet *impl) NewMessageSender(ctx context.Context, p peer.ID, opts *MessageSenderOpts) (MessageSender, error) {
	opts = setDefaultOpts(opts)

	var first peer.ID
	if bsnet.nrHops == 1 {
		first = p
	} else {
		for k := range bsnet.keys {
			if k != bsnet.Self() {
				if k == p {
					continue
				}
				first = k
				break
			}
		}
	}
	err := first.Validate()
	if err != nil {
		log.Infof("Could not determine first hop.")
	}

	sender := &streamMessageSender{
		to:    first,
		bsnet: bsnet,
		opts:  opts,
		dst:   p,
	}

	err = sender.multiAttempt(ctx, func() error {
		_, err := sender.Connect(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}

	return sender, nil
}

func setDefaultOpts(opts *MessageSenderOpts) *MessageSenderOpts {
	copy := *opts
	if opts.MaxRetries == 0 {
		copy.MaxRetries = 3
	}
	if opts.SendTimeout == 0 {
		copy.SendTimeout = maxSendTimeout
	}
	if opts.SendErrorBackoff == 0 {
		copy.SendErrorBackoff = 100 * time.Millisecond
	}
	return &copy
}

func sendTimeout(size int) time.Duration {
	timeout := sendLatency
	timeout += time.Duration((uint64(time.Second) * uint64(size)) / uint64(minSendRate))
	if timeout > maxSendTimeout {
		timeout = maxSendTimeout
	} else if timeout < minSendTimeout {
		timeout = minSendTimeout
	}
	return timeout
}

func (bsnet *impl) SendMessage(
	ctx context.Context,
	p peer.ID,
	outgoing bsmsg.BitSwapMessage,
) error {

	bsnet.serverlk.RLock()
	_, ok := bsnet.serversurb[p]
	bsnet.serverlk.RUnlock()
	if ok {
		log.Infof("Sphinx Reply")
		err := bsnet.reply(ctx, outgoing, p)
		if err != nil {
			log.Infof("Sphinx Reply Error: " + err.Error())
			return err
		}

	} else {
		tctx, cancel := context.WithTimeout(ctx, connectTimeout)
		defer cancel()

		s, err := bsnet.newStreamToPeer(tctx, p)
		if err != nil {
			return err
		}

		timeout := sendTimeout(outgoing.Size())
		if err = bsnet.msgToStream(ctx, s, outgoing, timeout, s.Conn().RemotePeer()); err != nil {
			_ = s.Reset()
			return err
		}
		return s.Close()
	}
	return nil
}

func (bsnet *impl) newStreamToPeer(ctx context.Context, p peer.ID) (network.Stream, error) {
	return bsnet.host.NewStream(ctx, p, bsnet.supportedProtocols...)
}

func (bsnet *impl) Start(r ...Receiver) {
	bsnet.receivers = r
	{
		connectionListeners := make([]ConnectionListener, len(r))
		for i, v := range r {
			connectionListeners[i] = v
		}
		bsnet.connectEvtMgr = newConnectEventManager(connectionListeners...)
	}
	for _, proto := range bsnet.supportedProtocols {
		bsnet.host.SetStreamHandler(proto, bsnet.handleNewStream)
	}
	bsnet.host.Network().Notify((*netNotifiee)(bsnet))
	bsnet.connectEvtMgr.Start()
}

func (bsnet *impl) Stop() {
	bsnet.connectEvtMgr.Stop()
	bsnet.host.Network().StopNotify((*netNotifiee)(bsnet))
}

func (bsnet *impl) ConnectTo(ctx context.Context, p peer.ID) error {
	return bsnet.host.Connect(ctx, peer.AddrInfo{ID: p})
}

func (bsnet *impl) DisconnectFrom(ctx context.Context, p peer.ID) error {
	return bsnet.host.Network().ClosePeer(p)
}

// FindProvidersAsync returns a channel of providers for the given key.
func (bsnet *impl) FindProvidersAsync(ctx context.Context, k cid.Cid, max int) <-chan peer.ID {
	out := make(chan peer.ID, max)
	go func() {
		defer close(out)
		providers := bsnet.routing.FindProvidersAsync(ctx, k, max)
		for info := range providers {
			if info.ID == bsnet.host.ID() {
				continue // ignore self as provider
			}
			bsnet.host.Peerstore().AddAddrs(info.ID, info.Addrs, peerstore.TempAddrTTL)
			select {
			case <-ctx.Done():
				return
			case out <- info.ID:
			}
		}
	}()
	return out
}

// Provide provides the key to the network
func (bsnet *impl) Provide(ctx context.Context, k cid.Cid) error {
	return bsnet.routing.Provide(ctx, k, true)
}

// handleNewStream receives a new stream from the network.
func (bsnet *impl) handleNewStream(s network.Stream) {
	defer s.Close()

	if len(bsnet.receivers) == 0 {
		_ = s.Reset()
		return
	}

	reader := msgio.NewVarintReaderSize(s, network.MessageSizeMax)
	for {
		if s.Protocol() == ProtocolSphinx {
			data, err := reader.ReadMsg()
			if err != nil {
				if err != io.EOF {
					_ = s.Reset()
					for _, v := range bsnet.receivers {
						v.ReceiveError(err)
					}
					log.Infof("bitswap net handleNewStream from %s error: %s", s.Conn().RemotePeer(), err)
				}
				return
			}
			log.Infof("Sphinx Protocol - received.")
			recv := make([]byte, len(data))
			copy(recv[:], data[:])

			pay, _, cmds, err := bsnet.recsphinx.Unwrap(bsnet.privk, data)

			if err != nil {
				log.Infof("Unwrap error: %v", err)
			}
			if len(cmds) > 0 && data[0] != 0 {
				log.Infof("Packet false: %v", data)
			}
			reader.ReleaseMsg(recv)
			log.Infof("%v, %v", s.Conn().RemotePeer().String(), len(cmds))
			if len(cmds) == 0 {
				log.Infof("No routingcommands: %v, payload: %v", len(cmds), pay)
			}
			if len(cmds) > 0 {
				switch rcmd := cmds[0].(type) {
				case *commands.NextNodeHop:
					log.Infof("Forward - from: %v to %v", s.Conn().RemotePeer().String(), bsnet.nidpid[rcmd.ID].String())
					err := bsnet.forward(context.Background(), data, rcmd)
					if err != nil {
						log.Infof("Forward error: %s", err.Error())
						return
					}
					bsnet.connectEvtMgr.OnMessage(s.Conn().RemotePeer())
					atomic.AddUint64(&bsnet.stats.MessagesRecvd, 1)

				case *commands.Recipient:
					log.Infof("Receipient Command")
					if pay[0] == 1 {
						msg, err := help(pay[(2 + bsnet.recsphinx.Geometry().SURBLength):])
						if err != nil {
							log.Infof("Error: pbmsg - bsmsg ")
							return
						}
						length := bsnet.recsphinx.Geometry().SURBLength
						//log.Infof("SURB Length: %v", length)
						log.Infof("Msg: %v, %v, %v", msg.Wantlist(), msg.BlockPresences(), len(msg.Blocks()))
						surb := pay[2 : length+2]
						sum := [32]byte{}
						_, err = io.ReadFull(rand.Reader, sum[:])
						if err != nil {
							log.Infof("Temp PID Error: %v", err.Error())
						}
						p := peer.ID(sum[:])
						bsnet.serverlk.Lock()
						bsnet.serversurb[p] = surb
						bsnet.serverlk.Unlock()

						ctx := context.Background()
						bsnet.connectEvtMgr.OnMessage(s.Conn().RemotePeer())
						atomic.AddUint64(&bsnet.stats.MessagesRecvd, 1)
						for _, v := range bsnet.receivers {
							v.ReceiveMessage(ctx, p, msg)
						}
					}
				case *commands.SURBReply:
					log.Infof("SURB command")
					bsnet.clientlk.RLock()
					deck, ok := bsnet.surbmap[rcmd.ID]
					bsnet.clientlk.RUnlock()
					if !ok {
						return
					}

					pay, err := bsnet.recsphinx.DecryptSURBPayload(pay, deck)
					if err != nil {
						log.Infof("Surb decrypt error: " + err.Error())
						return
					}
					msg, err := help(pay)
					if err != nil {
						log.Infof("Error: pbmsg - bsmsg ")
						return
					}
					bsnet.clientlk.Lock()
					p := bsnet.surbdest[rcmd.ID]
					delete(bsnet.surbdest, rcmd.ID)
					delete(bsnet.surbmap, rcmd.ID)
					bsnet.clientlk.Unlock()
					log.Infof("%v, %v, %v", msg.BlockPresences(), msg.Wantlist(), msg.Blocks())
					ctx := context.Background()
					for _, v := range bsnet.receivers {
						v.ReceiveMessage(ctx, p, msg)
					}

				default:
					fmt.Println("unknown")
				}
			}

		} else {
			log.Infof("Msg received")
			received, err := bsmsg.FromMsgReader(reader)
			if err != nil {
				if err != io.EOF {
					_ = s.Reset()
					for _, v := range bsnet.receivers {
						v.ReceiveError(err)
					}
					log.Debugf("bitswap net handleNewStream from %s error: %s", s.Conn().RemotePeer(), err)
				}
				return
			}

			p := s.Conn().RemotePeer()
			ctx := context.Background()
			log.Debugf("bitswap net handleNewStream from %s", s.Conn().RemotePeer())
			bsnet.connectEvtMgr.OnMessage(s.Conn().RemotePeer())
			atomic.AddUint64(&bsnet.stats.MessagesRecvd, 1)
			for _, v := range bsnet.receivers {
				v.ReceiveMessage(ctx, p, received)
			}
		}
	}
}

func (bsnet *impl) ConnectionManager() connmgr.ConnManager {
	return bsnet.host.ConnManager()
}

func (bsnet *impl) Stats() Stats {
	return Stats{
		MessagesRecvd: atomic.LoadUint64(&bsnet.stats.MessagesRecvd),
		MessagesSent:  atomic.LoadUint64(&bsnet.stats.MessagesSent),
	}
}

func (bsnet *impl) createPath(first peer.ID, dst peer.ID, surbid [16]byte) ([]*kpsphinx.PathHop, []*kpsphinx.PathHop, error) {
	if len(bsnet.keys) < bsnet.nrHops {
		return nil, nil, fmt.Errorf("Not enough keys")
	}
	var path []*kpsphinx.PathHop
	var retpath []*kpsphinx.PathHop

	if bsnet.nrHops > 1 {
		fh := &kpsphinx.PathHop{}
		farr, _ := first.MarshalBinary()
		copy(fh.ID[:], farr[:])
		fh.NIKEPublicKey = bsnet.keys[first]
		path = append(path, fh)

		for k, e := range bsnet.keys {
			if k == dst || k == bsnet.host.ID() || k == first {
				continue
			}
			if len(path)+1 >= bsnet.nrHops {
				break
			}
			hop := &kpsphinx.PathHop{}
			barr, _ := k.MarshalBinary()
			copy(hop.ID[:], barr[:])
			hop.NIKEPublicKey = e

			path = append(path, hop)
		}
		for k, e := range bsnet.keys {
			if k == dst || k == bsnet.host.ID() {
				continue
			}
			if len(retpath)+1 >= bsnet.nrHops {
				break
			}
			hop := &kpsphinx.PathHop{}
			barr, _ := k.MarshalBinary()
			copy(hop.ID[:], barr[:])
			hop.NIKEPublicKey = e

			retpath = append(retpath, hop)
		}
	}

	hop := &kpsphinx.PathHop{}
	dstarr, _ := dst.MarshalBinary()
	copy(hop.ID[:], dstarr[:])
	hop.NIKEPublicKey = bsnet.keys[dst]
	recipCmd := &commands.Recipient{}
	copy(recipCmd.ID[:], dstarr[:])
	hop.Commands = append(hop.Commands, recipCmd)
	path = append(path, hop)

	selfid := bsnet.Self()
	self := &kpsphinx.PathHop{}
	selfarr, _ := selfid.MarshalBinary()
	copy(self.ID[:], selfarr[:])
	self.NIKEPublicKey = bsnet.pubk
	surbCmd := &commands.SURBReply{}
	surbCmd.ID = surbid
	self.Commands = append(self.Commands, surbCmd)
	retpath = append(retpath, self)

	return path, retpath, nil
}

type netNotifiee impl

func (nn *netNotifiee) impl() *impl {
	return (*impl)(nn)
}

func (nn *netNotifiee) Connected(n network.Network, v network.Conn) {
	// ignore transient connections
	if v.Stat().Transient {
		return
	}

	nn.impl().connectEvtMgr.Connected(v.RemotePeer())
}

func (nn *netNotifiee) Disconnected(n network.Network, v network.Conn) {
	// Only record a "disconnect" when we actually disconnect.
	if n.Connectedness(v.RemotePeer()) == network.Connected {
		return
	}

	nn.impl().connectEvtMgr.Disconnected(v.RemotePeer())
}
func (nn *netNotifiee) OpenedStream(n network.Network, s network.Stream) {}
func (nn *netNotifiee) ClosedStream(n network.Network, v network.Stream) {}
func (nn *netNotifiee) Listen(n network.Network, a ma.Multiaddr)         {}
func (nn *netNotifiee) ListenClose(n network.Network, a ma.Multiaddr)    {}

func (bsnet *impl) reply(ctx context.Context, msg bsmsg.BitSwapMessage, p peer.ID) error {
	bsnet.serverlk.RLock()
	surb := bsnet.serversurb[p]
	bsnet.serverlk.RUnlock()
	payload, err := msg.ToProtoV1().Marshal()
	if err != nil {
		return err
	}
	pkt, nid, err := bsnet.recsphinx.NewPacketFromSURB(surb, payload)
	if err != nil {
		return err
	}
	pid := bsnet.nidpid[*nid]

	bsnet.serverlk.Lock()
	delete(bsnet.serversurb, p)
	bsnet.serverlk.Unlock()

	tctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	s, err := bsnet.newStreamToPeer(tctx, pid)
	if err != nil {
		return err
	}

	err = bsnet.pktwrite(tctx, pkt, s)
	if err != nil {
		log.Infof("Reply Error: %s", err)
		return err
	}

	log.Infof("Reply success.")

	return nil
}

func (bsnet *impl) forward(ctx context.Context, pkt []byte, cmd *commands.NextNodeHop) error {

	pid := bsnet.nidpid[cmd.ID]

	tctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	s, err := bsnet.newStreamToPeer(tctx, pid)
	if err != nil {
		log.Infof("Stream error:%s", err)
		return err
	}

	err = bsnet.pktwrite(tctx, pkt, s)
	if err != nil {
		log.Infof("Forward Error: %s", err)
		return err
	}
	log.Infof("Forward success.")

	return nil
}

func (bsnet *impl) pktwrite(ctx context.Context, pkt []byte, s network.Stream) error {

	timeout := sendTimeout(len(pkt))

	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}

	if err := s.SetWriteDeadline(deadline); err != nil {
		log.Warnf("error setting deadline: %s", err)
	}

	size := len(pkt)

	buf := pool.Get(size + binary.MaxVarintLen64)
	defer pool.Put(buf)

	n := binary.PutUvarint(buf, uint64(size))
	copy(buf[n:], pkt[:])
	n += size

	_, err := s.Write(buf[:n])
	if err != nil {
		log.Infof("Write error")
		return err
	}

	atomic.AddUint64(&bsnet.stats.MessagesSent, 1)

	if err := s.SetWriteDeadline(time.Time{}); err != nil {
		log.Warnf("error resetting deadline: %s", err)
	}

	return s.Close()

}

func help(data []byte) (bsmsg.BitSwapMessage, error) {

	pbmsg := bitswap_message_pb.Message{}
	pbmsg.Unmarshal(data)
	msg := bsmsg.New(pbmsg.Wantlist.Full)
	for _, e := range pbmsg.Wantlist.Entries {
		msg.AddEntry(e.Block.Cid, e.Priority, e.WantType, e.SendDontHave)
	}
	for _, e := range pbmsg.Blocks {
		b := blocks.NewBlock(e)
		msg.AddBlock(b)
	}
	for _, e := range pbmsg.GetPayload() {
		pref, err := cid.PrefixFromBytes(e.GetPrefix())
		if err != nil {
			return nil, err
		}

		c, err := pref.Sum(e.GetData())
		if err != nil {
			return nil, err
		}

		blk, err := blocks.NewBlockWithCid(e.GetData(), c)
		if err != nil {
			return nil, err
		}

		msg.AddBlock(blk)
	}
	for _, bi := range pbmsg.GetBlockPresences() {
		if !bi.Cid.Cid.Defined() {
			return nil, errors.New("missing cid")
		}
		msg.AddBlockPresence(bi.Cid.Cid, bi.Type)
	}

	return msg, nil
}

func (bsnet *impl) UpdatePubKeys(keys map[peer.ID]nike.PublicKey) {
	bsnet.nidpid = make(map[[32]byte]peer.ID)
	for k := range keys {
		karr, err := k.MarshalBinary()
		if err != nil {
			log.Infof("UpdatePubKeys: " + err.Error())
		}
		var nid [32]byte
		copy(nid[:], karr[:])
		bsnet.nidpid[nid] = k
	}
	bsnet.keys = keys
	log.Infof("%v new Pubkeys", len(keys))
}

func (bsnet *impl) GetNikeKey() nike.PublicKey {
	return bsnet.pubk
}

func (bsnet *impl) SetNikeKey(priv nike.PrivateKey, pub nike.PublicKey) {
	bsnet.privk = priv
	bsnet.pubk = pub
	return
}

func (bsnet *impl) Scheme() nike.Scheme {
	return bsnet.scheme
}

func (bsnet *impl) SetHops(hops int) {
	bsnet.nrHops = hops
	geom := geo.GeometryFromUserForwardPayloadLength(bsnet.scheme, 512, true, hops)
	sphinx := kpsphinx.NewNIKESphinx(bsnet.scheme, geom)
	bsnet.recsphinx = sphinx
}
