package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	logging "github.com/ipfs/go-log/v2"
	"github.com/katzenpost/hpqc/nike"

	bitswap "github.com/ipfs/boxo/bitswap"
	bsnet "github.com/ipfs/boxo/bitswap/network"
	blockstore "github.com/ipfs/boxo/blockstore"
	block "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	libp2p "github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multihash"
	"github.com/testground/sdk-go/network"
	"github.com/testground/sdk-go/run"
	"github.com/testground/sdk-go/runtime"
	"github.com/testground/sdk-go/sync"
)

var (
	testcases = map[string]interface{}{
		"speed-test": run.InitializedTestCaseFn(runSpeedTest),
	}
	networkState  = sync.State("network-configured")
	readyState    = sync.State("ready-to-publish")
	readyDLState  = sync.State("ready-to-download")
	doneState     = sync.State("done")
	providerTopic = sync.NewTopic("provider", &peer.AddrInfo{})
	blockTopic    = sync.NewTopic("blocks", &multihash.Multihash{})
)

type Info struct {
	ID peer.ID
	//Addr  []multiaddr.Multiaddr
	Pubk []byte
	Seq  int64
}

func main() {
	run.InvokeMap(testcases)
}

func runSpeedTest(runenv *runtime.RunEnv, initCtx *run.InitContext) error {

	runenv.RecordMessage("running speed-test")
	ctx := context.Background()
	count := runenv.IntParam("count")
	hops := runenv.IntParam("hops") + 1
	if hops < 1 {
		hops = 1
	}
	//linkShape := network.LinkShape{}
	linkShape := network.LinkShape{
		Latency: 100 * time.Millisecond,
		// Jitter:        5 * time.Millisecond,
		Bandwidth: 3e6,
		// Loss:          0.02,
		// Corrupt:       0.01,
		// CorruptCorr:   0.1,
		// Reorder:       0.01,
		// ReorderCorr:   0.1,
		// Duplicate:     0.02,
		// DuplicateCorr: 0.1,
	}
	initCtx.NetClient.MustConfigureNetwork(ctx, &network.Config{
		Network:        "default",
		Enable:         true,
		Default:        linkShape,
		CallbackState:  networkState,
		CallbackTarget: runenv.TestGroupInstanceCount,
		RoutingPolicy:  network.AllowAll,
	})
	listen, err := multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/%s/tcp/%d", initCtx.NetClient.MustGetDataNetworkIP().String(), 3333+initCtx.GlobalSeq))
	//listen, err := multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/%s/udp/%d/quic", initCtx.NetClient.MustGetDataNetworkIP().String(), 3333+initCtx.GlobalSeq))
	if err != nil {
		return err
	}
	h, err := libp2p.New(libp2p.ListenAddrs(listen))
	if err != nil {
		return err
	}
	kad, err := dht.New(ctx, h)
	if err != nil {
		return err
	}
	for _, a := range h.Addrs() {
		runenv.RecordMessage("listening on addr: %s", a.String())
	}

	bstore := blockstore.NewBlockstore(datastore.NewMapDatastore())
	ex := bitswap.New(ctx, bsnet.NewFromIpfsHost(h, kad), bstore)

	fp := filepath.Join(runenv.TestOutputsPath, "log")
	logf, err := os.Create(fp)
	if err != nil {
		runenv.RecordFailure(err)
	}
	defer logf.Close()
	logcfg := logging.GetConfig()
	logcfg.File = filepath.Clean(fp)
	logcfg.Format = logging.JSONOutput
	logcfg.Level = logging.LevelInfo
	logcfg.Stderr = false
	logcfg.Stdout = true
	logging.SetupLogging(logcfg)

	//scheme := x25519.Scheme(rand.Reader)
	scheme := ex.Scheme()
	pubk, privk, err := scheme.GenerateKeyPair()
	if err != nil {
		runenv.RecordMessage("Key generation error")
	}
	ex.SetNikeKey(privk, pubk)
	ex.SetHops(hops)

	bpubk, _ := pubk.MarshalBinary()

	tgc := sync.MustBoundClient(ctx, runenv)
	ai := peer.AddrInfo{
		ID:    h.ID(),
		Addrs: h.Addrs(),
	}
	runenv.RecordMessage("%v, %v", privk.Public().Bytes(), ai)
	announce := Info{
		ID:   ai.ID,
		Pubk: bpubk,
		Seq:  initCtx.GlobalSeq,
	}

	_, err = tgc.Publish(ctx, providerTopic, &ai)
	if err != nil {
		return fmt.Errorf("error during addrs publish: %s", err)
	}

	runenv.RecordMessage("Publish AddressInfos.")
	aiCh := make(chan *peer.AddrInfo)
	sctx, cancelSub := context.WithCancel(ctx)
	if _, err := tgc.Subscribe(sctx, providerTopic, aiCh); err != nil {
		cancelSub()
		return fmt.Errorf("error during waiting for others addrs (sub): %s", err)
	}
	var ais []peer.AddrInfo
	for i := 1; i <= runenv.TestGroupInstanceCount; i++ {
		ai, ok := <-aiCh
		if !ok {
			cancelSub()
			return fmt.Errorf("subscription closed")
		}
		ais = append(ais, *ai)
	}
	cancelSub()

	peersTopic := sync.NewTopic("peers", &Info{})
	_, err = tgc.Publish(ctx, peersTopic, &announce)
	if err != nil {
		return fmt.Errorf("error during addrs publish: %s", err)
	}

	// Get addresses of all peers
	peerCh := make(chan *Info)
	sctx2, cancelSub2 := context.WithCancel(ctx)
	if _, err := tgc.Subscribe(sctx2, peersTopic, peerCh); err != nil {
		cancelSub2()
		return fmt.Errorf("error during waiting for others addrs (sub): %s", err)
	}
	var infos []Info
	for i := 0; i < runenv.TestGroupInstanceCount; i++ {
		ai, ok := <-peerCh
		if !ok {
			cancelSub2()
			return fmt.Errorf("subscription closed")
		}
		infos = append(infos, *ai)
	}
	cancelSub2()

	connect := make(map[int64]peer.AddrInfo)
	keys := make(map[peer.ID]nike.PublicKey)
	for _, e := range infos {
		pk, err := scheme.UnmarshalBinaryPublicKey(e.Pubk)
		if err != nil {
			runenv.RecordFailure(err)
		}
		keys[e.ID] = pk
		sai := peer.AddrInfo{}
		for _, ai := range ais {
			if ai.ID == e.ID {
				sai = ai
				break
			}
		}
		connect[e.Seq] = sai
	}

	runenv.RecordMessage("Received %v AddrInfos", len(ais))
	runenv.RecordMessage("Received %v infos", len(infos))

	ex.UpdatePubKeys(keys)

	for k, e := range connect {
		if e.ID == h.ID() {
			continue
		}
		if k > initCtx.GlobalSeq {
			runenv.RecordMessage("connecting to: %s", fmt.Sprint(e.ID))
			err := h.Connect(ctx, e)
			if err != nil {
				return fmt.Errorf("could not connect: %w", err)
			}
		}
	}

	switch c := initCtx.GlobalSeq; {
	case c == 2:
		runenv.RecordMessage("running provider")
		err = runProvide(ctx, runenv, h, bstore, ex, count)
	case c == 1:
		runenv.RecordMessage("running requestor")
		err = runRequest(ctx, runenv, h, bstore, ex, count)
	case c > 2:
		runenv.RecordMessage("running passive")
		err = runProvide(ctx, runenv, h, bstore, ex, 0)
	default:
		runenv.RecordMessage("not part of a group")
		err = errors.New("unknown test group id")
	}
	return err
}

func runProvide(ctx context.Context, runenv *runtime.RunEnv, h host.Host, bstore blockstore.Blockstore, ex *bitswap.Bitswap, count int) error {

	runenv.RecordMessage("Provider: %v", h.ID())
	tgc := sync.MustBoundClient(ctx, runenv)
	tgc.MustSignalAndWait(ctx, readyState, runenv.TestInstanceCount)

	size := runenv.SizeParam("size")
	for i := 0; i < count; i++ {
		runenv.RecordMessage("generating %d-sized random block", size)
		buf := make([]byte, size)
		_, _ = rand.Read(buf)
		blk := block.NewBlock(buf)
		err := bstore.Put(ctx, blk)
		if err != nil {
			return err
		}
		err = ex.NotifyNewBlocks(ctx, blk)
		if err != nil {
			return err
		}
		mh := blk.Multihash()
		runenv.RecordMessage("publishing block %s", mh.String())
		tgc.MustPublish(ctx, blockTopic, &mh)
	}
	tgc.MustSignalAndWait(ctx, readyDLState, runenv.TestInstanceCount)
	runenv.RecordMessage("ReadyDL")
	tgc.MustSignalAndWait(ctx, doneState, runenv.TestInstanceCount)

	runenv.RecordMessage("Done")
	return nil
}

func runRequest(ctx context.Context, runenv *runtime.RunEnv, h host.Host, bstore blockstore.Blockstore, ex *bitswap.Bitswap, count int) error {

	runenv.RecordMessage("Requestor: %v", h.ID())
	tgc := sync.MustBoundClient(ctx, runenv)
	blkmhs := make(chan *multihash.Multihash)

	blockmhSub, err := tgc.Subscribe(ctx, blockTopic, blkmhs)
	if err != nil {
		return fmt.Errorf("could not subscribe to block sub: %w", err)
	}
	defer blockmhSub.Done()

	// tell the provider that we're ready for it to publish blocks
	tgc.MustSignalAndWait(ctx, readyState, runenv.TestInstanceCount)
	runenv.RecordMessage("Ready")

	cids := cid.NewSet()
	for i := 0; i < count; i++ {
		mh := <-blkmhs
		cids.Add(cid.NewCidV0(*mh))
	}
	runenv.RecordMessage("Received: CIDs -- %v \n", cids.Keys())

	// wait until the provider is ready for us to start downloading
	tgc.MustSignalAndWait(ctx, readyDLState, runenv.TestInstanceCount)
	runenv.RecordMessage("ReadyDL")

	runenv.RecordMessage("Connections: %v", len(h.Network().Conns()))

	begin := time.Now()
	for _, key := range cids.Keys() {
		runenv.RecordMessage("downloading block %s", key.String())
		dlBegin := time.Now()
		blk, err := ex.GetBlock(ctx, key)
		if err != nil {
			return fmt.Errorf("could not download block %s: %w", key.String(), err)
		}
		err = bstore.Put(ctx, blk) // store block
		if err != nil {
			return fmt.Errorf("could not store block %s: %w", key.String(), err)
		}
		dlDuration := time.Since(dlBegin)
		s := &BitswapStat{
			SingleDownloadSpeed: &SingleDownloadSpeed{
				Cid:              blk.Cid().String(),
				DownloadDuration: dlDuration,
			},
		}
		runenv.RecordMessage(Marshal(s))
		runenv.R().RecordPoint("dur-block-ms", float64(s.DownloadDuration.Milliseconds()))

		stored, err := bstore.Has(ctx, blk.Cid())
		if err != nil {
			return fmt.Errorf("error checking if blck was stored %s: %w", key.String(), err)
		}
		if !stored {
			return fmt.Errorf("block was not stored %s: %w", key.String(), err)
		}
	}
	duration := time.Since(begin)
	s := &BitswapStat{
		MultipleDownloadSpeed: &MultipleDownloadSpeed{
			BlockCount:    count,
			TotalDuration: duration,
		},
	}
	runenv.RecordMessage(Marshal(s))
	runenv.R().RecordPoint("total-dur-ms", float64(s.TotalDuration.Milliseconds()))
	// Request first block again
	// if cids.Len() > 0 {
	// 	cid := cids.Keys()[0]
	// 	err = bstore.DeleteBlock(ctx, cid)
	// 	if err != nil {
	// 		runenv.RecordFailure(err)
	// 	}
	// 	runenv.RecordMessage("Redownloading block %s", cid.String())
	// 	dlBegin := time.Now()
	// 	blk, err := ex.GetBlock(ctx, cid)
	// 	if err != nil {
	// 		return fmt.Errorf("could not download block %s: %w", cid.String(), err)
	// 	}
	// 	err = bstore.Put(ctx, blk) // store block
	// 	if err != nil {
	// 		runenv.RecordFailure(err)
	// 	}
	// 	dlDuration := time.Since(dlBegin)
	// 	s := &BitswapStat{
	// 		SingleDownloadSpeed: &SingleDownloadSpeed{
	// 			Cid:              blk.Cid().String(),
	// 			DownloadDuration: dlDuration,
	// 		},
	// 	}
	// 	runenv.RecordMessage(Marshal(s))
	// }

	tgc.MustSignalEntry(ctx, doneState)
	return nil
}
