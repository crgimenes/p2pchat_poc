package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	log "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
)

var (
	topicNameFlag = flag.String("topicName", "applesauce_crg_x", "name of topic to join")
	//connected     = make(map[peer.ID]bool)
	connected sync.Map = sync.Map{}
	ignore             = make(map[peer.ID]bool)
)

func initDHT(ctx context.Context, h host.Host) *dht.IpfsDHT {
	// Start a DHT, for use in peer discovery. We can't just make a new DHT
	// client because we want each peer to maintain its own local copy of the
	// DHT, so that the bootstrapping node of the DHT can go down without
	// inhibiting future peer discovery.
	kademliaDHT, err := dht.New(ctx, h, dht.MaxRecordAge(5*time.Second))
	if err != nil {
		panic(err)
	}
	if err = kademliaDHT.Bootstrap(ctx); err != nil {
		panic(err)
	}
	var wg sync.WaitGroup
	for _, peerAddr := range dht.DefaultBootstrapPeers {
		peerinfo, _ := peer.AddrInfoFromP2pAddr(peerAddr)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.Connect(ctx, *peerinfo); err != nil {
				fmt.Println("Bootstrap warning:", err)
			}
		}()
	}
	wg.Wait()

	return kademliaDHT
}

func discoverPeers(ctx context.Context, h host.Host) {
	b := backoff.NewExponentialBackOff()
	kademliaDHT := initDHT(ctx, h)
	routingDiscovery := drouting.NewRoutingDiscovery(kademliaDHT)
	for {
		dutil.Advertise(ctx, routingDiscovery, *topicNameFlag)

		peerChan, err := routingDiscovery.FindPeers(ctx, *topicNameFlag)
		if err != nil {
			panic(err)
		}
		for peer := range peerChan {
			if peer.ID == h.ID() {
				continue // No self connection
			}

			if _, ok := ignore[peer.ID]; ok {
				continue // Ignore this peer
			}

			//if _, ok := connected[peer.ID]; ok {
			if _, ok := connected.Load(peer.ID); ok {
				continue // Already connected
			}

			err := h.Connect(ctx, peer)
			if err != nil {
				if err.Error() == "no addresses" { // TODO: Handle this by compare ErrNoAddresses
					ignore[peer.ID] = true
					continue
				}

				fmt.Println("Failed connecting to", peer.ID, ", error:", err)

				// ignore peers that we can't connect to
				ignore[peer.ID] = true
				continue
			}
			fmt.Println("Connected to:", peer.ID)

			// add peer to connected list
			//connected[peer.ID] = true
			connected.Store(peer.ID, struct{}{})
		}
		time.Sleep(b.NextBackOff())
	}
}

func streamConsoleTo(ctx context.Context, topic *pubsub.Topic) {
	reader := bufio.NewReader(os.Stdin)
	for {
		s, err := reader.ReadString('\n')
		if err != nil {
			panic(err)
		}
		if err := topic.Publish(ctx, []byte(s)); err != nil {
			fmt.Println("### Publish error:", err)
		}
	}
}

func printMessagesFrom(ctx context.Context, sub *pubsub.Subscription) {
	for {
		m, err := sub.Next(ctx)
		if err != nil {
			panic(err)
		}
		fmt.Println(m.ReceivedFrom, ": ", string(m.Message.Data))
	}
}

func main() {
	//log.SetAllLoggers(log.LevelWarn)
	log.SetAllLoggers(log.LevelError)
	flag.Parse()

	ctx := context.Background()

	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/0.0.0.0/tcp/0"),
		libp2p.NATPortMap(),  // enable UPnP/PCP port mapping
		libp2p.EnableRelay(), // allow this node to be a relay hop
		//libp2p.EnableAutoRelay(),      // discover and use relays automatically  Deprecated: Use EnableAutoRelayWithStaticRelays or EnableAutoRelayWithPeerSource
	)
	if err != nil {
		panic(err)
	}
	go discoverPeers(ctx, h)

	ps, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		panic(err)
	}
	topic, err := ps.Join(*topicNameFlag)
	if err != nil {
		panic(err)
	}
	go streamConsoleTo(ctx, topic)

	sub, err := topic.Subscribe()
	if err != nil {
		panic(err)
	}
	printMessagesFrom(ctx, sub)
}
