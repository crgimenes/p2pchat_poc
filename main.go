package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	log "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	ma "github.com/multiformats/go-multiaddr"
)

// Estrutura para armazenar peers conhecidos com timestamp
type KnownPeer struct {
	Addr                  string    `json:"addr"`
	LastSeen              time.Time `json:"last_seen"`
	SuccessfulConnections int       `json:"successful_connections"`
}

type KnownPeers struct {
	Peers        map[string]KnownPeer `json:"peers"` // Chave é o peer ID
	LastModified time.Time            `json:"last_modified"`
}

const (
	KeyFile              = "node.key"
	PeersFile            = "known_peers.json"
	maxReconnectAttempts = 10
	initialBackoff       = 1 * time.Second
	maxBackoff           = 60 * time.Second
)

var (
	topicNameFlag   = flag.String("topicName", "applesauce_crg_y", "name of topic to join")
	connected       = make(map[peer.ID]bool)
	ignore          = make(map[peer.ID]bool)
	knownPeersMutex sync.RWMutex
	lastSavedPeers  KnownPeers
)

// Carregar peers conhecidos do disco
func loadKnownPeers() []peer.AddrInfo {
	knownPeersMutex.RLock()
	defer knownPeersMutex.RUnlock()

	var knownPeers KnownPeers

	data, err := os.ReadFile(PeersFile)
	if err != nil {
		// Inicializa uma estrutura vazia se o arquivo não existir
		knownPeers = KnownPeers{
			Peers:        make(map[string]KnownPeer),
			LastModified: time.Now(),
		}
		lastSavedPeers = knownPeers
		return nil
	}

	err = json.Unmarshal(data, &knownPeers)
	if err != nil {
		fmt.Printf("Erro ao desserializar peers conhecidos: %s\n", err)
		knownPeers = KnownPeers{
			Peers:        make(map[string]KnownPeer),
			LastModified: time.Now(),
		}
	}

	lastSavedPeers = knownPeers

	// Filtrar peers antigos (mais de 7 dias sem conexão)
	now := time.Now()
	for id, peer := range knownPeers.Peers {
		if now.Sub(peer.LastSeen) > 7*24*time.Hour && peer.SuccessfulConnections < 3 {
			delete(knownPeers.Peers, id)
		}
	}

	var peers []peer.AddrInfo
	for _, knownPeer := range knownPeers.Peers {
		addr, err := ma.NewMultiaddr(knownPeer.Addr)
		if err != nil {
			continue
		}

		peerInfo, err := peer.AddrInfoFromP2pAddr(addr)
		if err != nil {
			continue
		}

		peers = append(peers, *peerInfo)
	}

	return peers
}

// Salvar peers conhecidos no disco
func saveKnownPeers(h host.Host) {
	knownPeersMutex.Lock()
	defer knownPeersMutex.Unlock()

	// Carregue a estrutura atual se ainda não inicializada
	var knownPeers KnownPeers
	if lastSavedPeers.Peers == nil {
		knownPeers = KnownPeers{
			Peers:        make(map[string]KnownPeer),
			LastModified: time.Now(),
		}
	} else {
		knownPeers = lastSavedPeers
	}

	changed := false
	now := time.Now()

	// Atualiza peers conectados
	for peerID := range connected {
		peer := h.Peerstore().PeerInfo(peerID)

		// Escolhe o melhor endereço para este peer
		// Prefere endereços não-locais para maior estabilidade
		var bestAddr ma.Multiaddr
		for _, addr := range peer.Addrs {
			if bestAddr == nil {
				bestAddr = addr
				continue
			}

			// Prioriza endereços não locais
			if !isLocalAddress(bestAddr) && isLocalAddress(addr) {
				continue
			}

			// Você pode adicionar mais lógica de seleção aqui
			bestAddr = addr
		}

		if bestAddr != nil {
			fullAddr := bestAddr.String() + "/p2p/" + peer.ID.String()
			peerID := peer.ID.String()

			existingPeer, exists := knownPeers.Peers[peerID]
			if exists {
				existingPeer.LastSeen = now
				existingPeer.SuccessfulConnections++
				knownPeers.Peers[peerID] = existingPeer
				changed = true
			} else {
				knownPeers.Peers[peerID] = KnownPeer{
					Addr:                  fullAddr,
					LastSeen:              now,
					SuccessfulConnections: 1,
				}
				changed = true
			}
		}
	}

	// Salva apenas se houve mudanças
	if changed {
		knownPeers.LastModified = now
		data, err := json.Marshal(knownPeers)
		if err != nil {
			fmt.Printf("Erro ao serializar peers conhecidos: %s\n", err)
			return
		}

		err = os.WriteFile(PeersFile, data, 0644)
		if err != nil {
			fmt.Printf("Erro ao salvar peers conhecidos: %s\n", err)
		} else {
			lastSavedPeers = knownPeers
		}
	}
}

// Função auxiliar para determinar se um endereço é local
func isLocalAddress(addr ma.Multiaddr) bool {
	// Implementação simplificada - você pode expandir conforme necessário
	return strings.Contains(addr.String(), "/ip4/127.0.0.1/") ||
		strings.Contains(addr.String(), "/ip4/192.168.") ||
		strings.Contains(addr.String(), "/ip4/10.") ||
		strings.Contains(addr.String(), "/ip4/172.16.")
}

func loadOrCreateIdentity() crypto.PrivKey {
	if _, err := os.Stat(KeyFile); os.IsNotExist(err) {
		fmt.Println("Arquivo de chaves não encontrado. Gerando novas chaves...")
		priv, _, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		if err != nil {
			panic(err)
		}
		saveKeyToFile(priv)
		return priv
	}

	keyBytes, err := os.ReadFile(KeyFile)
	if err != nil {
		fmt.Printf("Erro ao ler chaves: %s. Gerando novas chaves...\n", err)
		priv, _, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		if err != nil {
			panic(err)
		}
		saveKeyToFile(priv)
		return priv
	}

	priv, err := crypto.UnmarshalPrivateKey(keyBytes)
	if err != nil {
		fmt.Printf("Erro ao carregar chaves: %s. Gerando novas chaves...\n", err)
		priv, _, err = crypto.GenerateKeyPair(crypto.RSA, 2048)
		if err != nil {
			panic(err)
		}
		saveKeyToFile(priv)
		return priv
	}

	fmt.Println("Chaves carregadas com sucesso do arquivo.")
	return priv
}

func saveKeyToFile(priv crypto.PrivKey) error {
	keyBytes, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return err
	}
	return os.WriteFile(KeyFile, keyBytes, 0600)
}

func initDHT(ctx context.Context, h host.Host) *dht.IpfsDHT {
	// Start a DHT, for use in peer discovery. We can't just make a new DHT
	// client because we want each peer to maintain its own local copy of the
	// DHT, so that the bootstrapping node of the DHT can go down without
	// inhibiting future peer discovery.
	kademliaDHT, err := dht.New(ctx, h, dht.MaxRecordAge(5*time.Second))
	if err != nil {
		panic(err)
	}
	err = kademliaDHT.Bootstrap(ctx)
	if err != nil {
		panic(err)
	}
	var wg sync.WaitGroup
	for _, peerAddr := range dht.DefaultBootstrapPeers {
		peerinfo, _ := peer.AddrInfoFromP2pAddr(peerAddr)
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := h.Connect(ctx, *peerinfo)
			if err != nil {
				fmt.Println("Bootstrap warning:", err)
			}
		}()
	}
	wg.Wait()

	return kademliaDHT
}

func discoverPeers(ctx context.Context, h host.Host) {
	// Armazena tentativas de reconexão por peer
	reconnectAttempts := make(map[peer.ID]int)
	lastAttempt := make(map[peer.ID]time.Time)

	// Salvamento periódico dos peers conhecidos
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				saveKnownPeers(h)
			case <-ctx.Done():
				return
			}
		}
	}()

	kademliaDHT := initDHT(ctx, h)
	routingDiscovery := drouting.NewRoutingDiscovery(kademliaDHT)

	for {
		dutil.Advertise(ctx, routingDiscovery, *topicNameFlag)

		// Tenta reconectar aos peers conhecidos
		knownPeers := loadKnownPeers()
		now := time.Now()

		// Reconexão com peers conhecidos
		for _, peerInfo := range knownPeers {
			if peerInfo.ID == h.ID() {
				continue
			}

			if _, ok := connected[peerInfo.ID]; ok {
				continue // Já conectado
			}

			// Verifica se atingiu o limite de tentativas
			if attempts, exists := reconnectAttempts[peerInfo.ID]; exists && attempts >= maxReconnectAttempts {
				fmt.Printf("Desistindo de reconexão com %s após %d tentativas\n", peerInfo.ID, attempts)
				ignore[peerInfo.ID] = true
				continue
			}

			// Aplica backoff exponencial
			if lastTime, exists := lastAttempt[peerInfo.ID]; exists {
				attempts := reconnectAttempts[peerInfo.ID]
				backoffDuration := initialBackoff * time.Duration(1<<uint(attempts))
				if backoffDuration > maxBackoff {
					backoffDuration = maxBackoff
				}

				if now.Sub(lastTime) < backoffDuration {
					continue // Ainda não é hora de tentar novamente
				}
			}

			// Tenta reconectar
			err := h.Connect(ctx, peerInfo)
			if err == nil {
				fmt.Printf("Reconectado com sucesso ao peer conhecido: %s\n", peerInfo.ID)
				connected[peerInfo.ID] = true
				delete(reconnectAttempts, peerInfo.ID)
				delete(ignore, peerInfo.ID)
				delete(lastAttempt, peerInfo.ID)
			} else {
				fmt.Printf("Falha na reconexão com %s: %s\n", peerInfo.ID, err)
				reconnectAttempts[peerInfo.ID]++
				lastAttempt[peerInfo.ID] = now
			}
		}

		// Tentar reconectar peers ignorados anteriormente
		for peerID := range ignore {
			if _, ok := connected[peerID]; ok {
				continue // Já está conectado
			}

			// Verifica se atingiu o limite de tentativas
			if attempts, exists := reconnectAttempts[peerID]; exists && attempts >= maxReconnectAttempts {
				continue // Desistir permanentemente após muitas tentativas
			}

			// Aplica backoff exponencial
			if lastTime, exists := lastAttempt[peerID]; exists {
				attempts := reconnectAttempts[peerID]
				backoffDuration := initialBackoff * time.Duration(1<<uint(attempts))
				if backoffDuration > maxBackoff {
					backoffDuration = maxBackoff
				}

				if now.Sub(lastTime) < backoffDuration {
					continue // Ainda não é hora de tentar novamente
				}
			}

			// Tenta encontrar o peer diretamente usando o DHT
			// Corrigindo problema: RoutingDiscovery não tem método FindPeer
			peerInfo, err := kademliaDHT.FindPeer(ctx, peerID)
			if err != nil {
				fmt.Printf("Não foi possível encontrar o peer %s: %s\n", peerID, err)
				reconnectAttempts[peerID]++
				lastAttempt[peerID] = now
				continue
			}

			err = h.Connect(ctx, peerInfo)
			if err == nil {
				fmt.Printf("Reconexão bem-sucedida com peer anteriormente ignorado: %s\n", peerID)
				connected[peerID] = true
				delete(reconnectAttempts, peerID)
				delete(ignore, peerID)
				delete(lastAttempt, peerID)
			} else {
				fmt.Printf("Falha na reconexão com peer ignorado %s: %s\n", peerID, err)
				reconnectAttempts[peerID]++
				lastAttempt[peerID] = now
			}
		}

		// Procura por novos peers
		peerChan, err := routingDiscovery.FindPeers(ctx, *topicNameFlag)
		if err != nil {
			fmt.Printf("Erro na descoberta de peers: %s. Tentando novamente...\n", err)
			time.Sleep(5 * time.Second)
			continue
		}

		// Processa novos peers encontrados
		for peerInfo := range peerChan {
			if peerInfo.ID == h.ID() {
				continue // Evitar auto-conexão
			}

			if _, ok := connected[peerInfo.ID]; ok {
				continue // Já conectado
			}

			if _, ok := ignore[peerInfo.ID]; ok {
				continue // Ignorado anteriormente
			}

			err := h.Connect(ctx, peerInfo)
			if err != nil {
				if err.Error() == "no addresses" {
					ignore[peerInfo.ID] = true
					continue
				}

				fmt.Printf("Falha ao conectar com %s: %s\n", peerInfo.ID, err)
				reconnectAttempts[peerInfo.ID] = 1
				lastAttempt[peerInfo.ID] = now
				continue
			}

			fmt.Printf("Conectado com sucesso a novo peer: %s\n", peerInfo.ID)
			connected[peerInfo.ID] = true
		}

		// Aguarda antes da próxima iteração
		time.Sleep(10 * time.Second)
	}
}

func streamConsoleTo(ctx context.Context, topic *pubsub.Topic) {
	reader := bufio.NewReader(os.Stdin)
	for {
		s, err := reader.ReadString('\n')
		if err != nil {
			panic(err)
		}
		err = topic.Publish(ctx, []byte(s))
		if err != nil {
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

	priv := loadOrCreateIdentity()
	h, err := libp2p.New(
		libp2p.ListenAddrStrings("/ip4/0.0.0.0/tcp/0"),
		libp2p.NATPortMap(),  // enable UPnP/PCP port mapping
		libp2p.EnableRelay(), // allow this node to be a relay hop
		//libp2p.EnableAutoRelay(),      // discover and use relays automatically  Deprecated: Use EnableAutoRelayWithStaticRelays or EnableAutoRelayWithPeerSource
		libp2p.Identity(priv),
	)
	if err != nil {
		panic(err)
	}

	// Exibe informação do host
	fmt.Printf("Peer ID: %s\n", h.ID())
	for _, a := range h.Addrs() {
		fmt.Printf("Address: %s/p2p/%s\n", a, h.ID())
	}

	h.Network().Notify(&network.NotifyBundle{
		DisconnectedF: func(n network.Network, c network.Conn) {
			pid := c.RemotePeer()
			// schedule reconnect
			go func() { h.Connect(ctx, peer.AddrInfo{ID: pid}) }()
		},
	})

	go discoverPeers(ctx, h)

	ps, err := pubsub.NewGossipSub(
		ctx,
		h,
		pubsub.WithFloodPublish(true), // dissemina rápido em redes pequenas
		pubsub.WithMessageSigning(true),
		pubsub.WithPeerExchange(true), // Habilita troca de peers
		pubsub.WithStrictSignatureVerification(true),
		pubsub.WithValidateQueueSize(128),
		pubsub.WithValidateThrottle(2048),
		pubsub.WithSubscriptionFilter(
			pubsub.WrapLimitSubscriptionFilter(
				pubsub.NewAllowlistSubscriptionFilter([]string{*topicNameFlag}...),
				100,
			),
		),
	)
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
