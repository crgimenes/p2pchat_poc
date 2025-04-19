package p2pnode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
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
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/libp2p/go-libp2p/p2p/protocol/holepunch"
	"github.com/libp2p/go-libp2p/p2p/protocol/identify"
	ma "github.com/multiformats/go-multiaddr"
)

// KnownPeer representa um peer conhecido pelo nó
type KnownPeer struct {
	Addr                  string    `json:"addr"`
	LastSeen              time.Time `json:"last_seen"`
	SuccessfulConnections int       `json:"successful_connections"`
}

// KnownPeers contém a lista de peers conhecidos pelo nó
type KnownPeers struct {
	Peers        map[string]KnownPeer `json:"peers"`
	LastModified time.Time            `json:"last_modified"`
}

// MDNSNotifee é um manipulador para notificações mDNS
type MDNSNotifee struct {
	node *Node
}

// Constantes para a configuração do nó P2P
const (
	DefaultKeyFile       = "node.key"
	DefaultPeersFile     = "known_peers.json"
	MaxReconnectAttempts = 10
	InitialBackoff       = 1 * time.Second
	MaxBackoff           = 60 * time.Second
	MDNSServiceTag       = "p2pchat-poc"
)

// Node representa um nó P2P completo
type Node struct {
	cancel          context.CancelFunc
	connected       map[peer.ID]bool
	connectedMutex  sync.RWMutex
	ctx             context.Context
	dht             *dht.IpfsDHT
	host            host.Host
	ignore          map[peer.ID]bool
	ignoreMutex     sync.RWMutex
	keyFile         string
	knownPeersMutex sync.RWMutex
	lastSavedPeers  KnownPeers
	peersFile       string
	pubsub          *pubsub.PubSub
	staticRelays    []string
	topic           *pubsub.Topic
	topicName       string
}

// NewNode cria uma nova instância de um nó P2P
func NewNode(topicName, keyFile, peersFile string) *Node {
	ctx, cancel := context.WithCancel(context.Background())

	return &Node{
		topicName:    topicName,
		keyFile:      keyFile,
		peersFile:    peersFile,
		connected:    make(map[peer.ID]bool),
		ignore:       make(map[peer.ID]bool),
		ctx:          ctx,
		cancel:       cancel,
		staticRelays: DefaultStaticRelays(),
	}
}

// DefaultStaticRelays retorna uma lista de relays estáticos padrão
func DefaultStaticRelays() []string {
	return []string{
		"/ip4/147.75.80.110/tcp/4001/p2p/QmbFgm5rao4mLdUAaCPgRGRoqyMKfK2gCgQaT77PmPjsEY",
		"/ip4/147.75.195.153/tcp/4001/p2p/QmW9m57aiBDHAkKj9nmFSEn7ZqrcF1fZS4bipsTCHburei",
		"/dnsaddr/bootstrap.libp2p.io/p2p/QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN",
		"/dnsaddr/bootstrap.libp2p.io/p2p/QmbLHAnMoJPWSCR5Zhtx6BHJX9KiKNN6tpvbUcqanj75Nb",
	}
}

// SetStaticRelays define os relays estáticos para o nó
func (node *Node) SetStaticRelays(relays []string) {
	node.staticRelays = relays
}

// IsConnected verifica se o nó está conectado a um peer específico
func (node *Node) IsConnected(peerID peer.ID) bool {
	node.connectedMutex.RLock()
	defer node.connectedMutex.RUnlock()
	return node.connected[peerID]
}

// SetConnected define o estado de conexão com um peer
func (node *Node) SetConnected(peerID peer.ID, connected bool) {
	node.connectedMutex.Lock()
	defer node.connectedMutex.Unlock()
	if connected {
		node.connected[peerID] = true
		return
	}
	delete(node.connected, peerID)
}

// IsIgnored verifica se um peer está sendo ignorado
func (node *Node) IsIgnored(peerID peer.ID) bool {
	node.ignoreMutex.RLock()
	ret := node.ignore[peerID]
	node.ignoreMutex.RUnlock()
	return ret
}

// SetIgnored define se um peer deve ser ignorado
func (node *Node) SetIgnored(peerID peer.ID, ignored bool) {
	node.ignoreMutex.Lock()
	defer node.ignoreMutex.Unlock()
	if ignored {
		node.ignore[peerID] = true
		return
	}
	delete(node.ignore, peerID)
}

// GetConnectedPeers retorna a lista de peers conectados
func (node *Node) GetConnectedPeers() []peer.ID {
	node.connectedMutex.RLock()
	defer node.connectedMutex.RUnlock()

	peers := make([]peer.ID, 0, len(node.connected))
	for peerID := range node.connected {
		peers = append(peers, peerID)
	}
	return peers
}

// convertToAddrInfo converte strings de endereço para AddrInfo
func (node *Node) convertToAddrInfo(addresses []string) []peer.AddrInfo {
	var addrInfos []peer.AddrInfo
	for _, addrStr := range addresses {
		addr, err := ma.NewMultiaddr(addrStr)
		if err != nil {
			fmt.Printf("Endereço inválido %s: %s\n", addrStr, err)
			continue
		}

		addrInfo, err := peer.AddrInfoFromP2pAddr(addr)
		if err != nil {
			fmt.Printf("Falha ao converter %s para AddrInfo: %s\n", addrStr, err)
			continue
		}
		addrInfos = append(addrInfos, *addrInfo)
	}
	return addrInfos
}

// LoadKnownPeers carrega a lista de peers conhecidos do arquivo
func (node *Node) LoadKnownPeers() []peer.AddrInfo {
	node.knownPeersMutex.RLock()
	defer node.knownPeersMutex.RUnlock()

	var knownPeers KnownPeers

	data, err := os.ReadFile(node.peersFile)
	if err != nil {
		knownPeers = KnownPeers{
			Peers:        make(map[string]KnownPeer),
			LastModified: time.Now(),
		}
		node.lastSavedPeers = knownPeers
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

	// Atualiza os endereços QUIC antigos para o novo formato
	for id, peer := range knownPeers.Peers {
		if strings.Contains(peer.Addr, "/udp/") && strings.Contains(peer.Addr, "/quic") && !strings.Contains(peer.Addr, "/quic-v1") {
			updatedAddr := strings.Replace(peer.Addr, "/quic", "/quic-v1", 1)
			fmt.Printf("Atualizando endereço do peer %s de %s para %s\n", id, peer.Addr, updatedAddr)
			peer.Addr = updatedAddr
			knownPeers.Peers[id] = peer
		}
	}

	node.lastSavedPeers = knownPeers

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
			fmt.Printf("Endereço inválido ignorado: %s, erro: %s\n", knownPeer.Addr, err)
			continue
		}

		peerInfo, err := peer.AddrInfoFromP2pAddr(addr)
		if err != nil {
			fmt.Printf("Falha ao converter para AddrInfo: %s, erro: %s\n", knownPeer.Addr, err)
			continue
		}

		peers = append(peers, *peerInfo)
	}

	return peers
}

// SaveKnownPeers salva a lista de peers conhecidos no arquivo
func (node *Node) SaveKnownPeers() {
	node.knownPeersMutex.Lock()
	defer node.knownPeersMutex.Unlock()

	var knownPeers KnownPeers
	if node.lastSavedPeers.Peers == nil {
		knownPeers = KnownPeers{
			Peers:        make(map[string]KnownPeer),
			LastModified: time.Now(),
		}
	} else {
		knownPeers = node.lastSavedPeers
	}

	changed := false
	now := time.Now()

	for peerID := range node.connected {
		peer := node.host.Peerstore().PeerInfo(peerID)

		var bestAddr ma.Multiaddr
		for _, addr := range peer.Addrs {
			if bestAddr == nil {
				bestAddr = addr
				continue
			}

			if !isLocalAddress(bestAddr) && isLocalAddress(addr) {
				continue
			}

			bestAddr = addr
		}

		if bestAddr != nil {
			fullAddr := bestAddr.String() + "/p2p/" + peer.ID.String()

			// Atualiza endereços QUIC antigos para o formato novo
			if strings.Contains(fullAddr, "/udp/") && strings.Contains(fullAddr, "/quic") && !strings.Contains(fullAddr, "/quic-v1") {
				fullAddr = strings.Replace(fullAddr, "/quic", "/quic-v1", 1)
			}

			peerID := peer.ID.String()

			existingPeer, exists := knownPeers.Peers[peerID]
			if exists {
				existingPeer.LastSeen = now
				existingPeer.SuccessfulConnections++
				existingPeer.Addr = fullAddr // Atualiza o endereço para garantir que esteja usando o formato mais recente
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

	const maxPeers = 30
	if len(knownPeers.Peers) > maxPeers {
		knownPeers.Peers = selectBestPeers(knownPeers.Peers, maxPeers)
		changed = true
	}

	if changed {
		knownPeers.LastModified = now
		data, err := json.Marshal(knownPeers)
		if err != nil {
			fmt.Printf("Erro ao serializar peers conhecidos: %s\n", err)
			return
		}

		err = os.WriteFile(node.peersFile, data, 0644)
		if err != nil {
			fmt.Printf("Erro ao salvar peers conhecidos: %s\n", err)
		} else {
			node.lastSavedPeers = knownPeers
		}
	}
}

// selectBestPeers seleciona os melhores peers com base em uma pontuação
func selectBestPeers(peers map[string]KnownPeer, limit int) map[string]KnownPeer {
	type PeerRanking struct {
		ID    string
		Peer  KnownPeer
		Score float64
	}

	var rankings []PeerRanking
	now := time.Now()

	for id, peer := range peers {
		recencyScore := 1.0 / (now.Sub(peer.LastSeen).Hours() + 1.0) // Evita divisão por zero
		connectionsScore := float64(peer.SuccessfulConnections)
		score := (recencyScore * 10.0) + connectionsScore

		rankings = append(rankings, PeerRanking{
			ID:    id,
			Peer:  peer,
			Score: score,
		})
	}

	sort.Slice(rankings, func(i, j int) bool {
		return rankings[i].Score > rankings[j].Score
	})

	result := make(map[string]KnownPeer)
	for i := 0; i < limit && i < len(rankings); i++ {
		result[rankings[i].ID] = rankings[i].Peer
	}

	return result
}

// isLocalAddress verifica se um endereço é local
func isLocalAddress(addr ma.Multiaddr) bool {
	return strings.Contains(addr.String(), "/ip4/127.0.0.1/") ||
		strings.Contains(addr.String(), "/ip4/192.168.") ||
		strings.Contains(addr.String(), "/ip4/10.") ||
		strings.Contains(addr.String(), "/ip4/172.16.")
}

// nextBackoff calcula o próximo tempo de espera para nova tentativa
func nextBackoff(attempt int) time.Duration {
	attempt = min(attempt, 30)
	backoffDuration := InitialBackoff * time.Duration(1<<uint(attempt))
	backoffDuration = min(backoffDuration, MaxBackoff)
	return backoffDuration
}

// LoadOrCreateIdentity carrega ou cria uma nova identidade para o nó
func (node *Node) LoadOrCreateIdentity() crypto.PrivKey {
	if _, err := os.Stat(node.keyFile); os.IsNotExist(err) {
		fmt.Println("Arquivo de chaves não encontrado. Gerando novas chaves...")
		priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
		if err != nil {
			panic(err)
		}
		node.saveKeyToFile(priv)
		return priv
	}

	keyBytes, err := os.ReadFile(node.keyFile)
	if err != nil {
		fmt.Printf("Erro ao ler chaves: %s. Gerando novas chaves...\n", err)
		priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
		if err != nil {
			panic(err)
		}
		node.saveKeyToFile(priv)
		return priv
	}

	priv, err := crypto.UnmarshalPrivateKey(keyBytes)
	if err != nil {
		fmt.Printf("Erro ao carregar chaves: %s. Gerando novas chaves...\n", err)
		priv, _, err = crypto.GenerateKeyPair(crypto.Ed25519, -1)
		if err != nil {
			panic(err)
		}
		node.saveKeyToFile(priv)
		return priv
	}

	fmt.Println("Chaves carregadas com sucesso do arquivo.")
	return priv
}

// saveKeyToFile salva a chave privada em um arquivo
func (node *Node) saveKeyToFile(priv crypto.PrivKey) error {
	keyBytes, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return err
	}
	return os.WriteFile(node.keyFile, keyBytes, 0600)
}

// InitDHT inicializa a tabela hash distribuída do nó
func (node *Node) InitDHT() *dht.IpfsDHT {
	kademliaDHT, err := dht.New(node.ctx, node.host, dht.MaxRecordAge(5*time.Second))
	if err != nil {
		panic(err)
	}
	err = kademliaDHT.Bootstrap(node.ctx)
	if err != nil {
		panic(err)
	}
	var wg sync.WaitGroup
	for _, peerAddr := range dht.DefaultBootstrapPeers {
		peerinfo, _ := peer.AddrInfoFromP2pAddr(peerAddr)
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := node.host.Connect(node.ctx, *peerinfo)
			if err != nil {
				fmt.Println("Bootstrap warning:", err)
			}
		}()
	}
	wg.Wait()

	node.dht = kademliaDHT
	return kademliaDHT
}

// HandlePeerFound é chamado quando um peer é descoberto via mDNS
func (n *MDNSNotifee) HandlePeerFound(pi peer.AddrInfo) {
	if pi.ID == n.node.host.ID() {
		return
	}

	fmt.Printf("Peer encontrado via mDNS: %s\n", pi.ID)

	err := n.node.host.Connect(context.Background(), pi)
	if err != nil {
		fmt.Printf("Falha ao conectar via mDNS com %s: %s\n", pi.ID, err)
		return
	}

	fmt.Printf("Conectado via mDNS com %s\n", pi.ID)
	n.node.SetConnected(pi.ID, true)
}

// SetupMDNS configura o serviço de descoberta local mDNS
func (node *Node) SetupMDNS() error {
	service := mdns.NewMdnsService(node.host, MDNSServiceTag, &MDNSNotifee{node: node})
	if service == nil {
		return fmt.Errorf("falha ao criar serviço mDNS")
	}

	fmt.Println("Serviço de descoberta local mDNS iniciado")
	return nil
}

// DiscoverPeers inicia o processo de descoberta de peers
func (node *Node) DiscoverPeers() {
	reconnectAttempts := make(map[peer.ID]int)
	lastAttempt := make(map[peer.ID]time.Time)

	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				node.SaveKnownPeers()
			case <-node.ctx.Done():
				return
			}
		}
	}()

	if node.dht == nil {
		node.InitDHT()
	}
	routingDiscovery := drouting.NewRoutingDiscovery(node.dht)

	for {
		select {
		case <-node.ctx.Done():
			return
		default:
			// Continua execução normal
		}

		dutil.Advertise(node.ctx, routingDiscovery, node.topicName)

		knownPeers := node.LoadKnownPeers()
		now := time.Now()

		for _, peerInfo := range knownPeers {
			if peerInfo.ID == node.host.ID() {
				continue
			}

			if node.IsConnected(peerInfo.ID) {
				continue
			}

			if attempts, exists := reconnectAttempts[peerInfo.ID]; exists && attempts >= MaxReconnectAttempts {
				fmt.Printf("Desistindo de reconexão com %s após %d tentativas\n", peerInfo.ID, attempts)
				node.SetIgnored(peerInfo.ID, true)
				continue
			}

			if lastTime, exists := lastAttempt[peerInfo.ID]; exists {
				attempts := reconnectAttempts[peerInfo.ID]
				backoffDuration := nextBackoff(attempts)

				if now.Sub(lastTime) < backoffDuration {
					continue
				}
			}

			err := node.host.Connect(node.ctx, peerInfo)
			if err == nil {
				fmt.Printf("Reconectado com sucesso ao peer conhecido: %s\n", peerInfo.ID)
				node.SetConnected(peerInfo.ID, true)
				delete(reconnectAttempts, peerInfo.ID)
				node.SetIgnored(peerInfo.ID, false)
				delete(lastAttempt, peerInfo.ID)
			} else {
				fmt.Printf("Falha na reconexão com %s: %s\n", peerInfo.ID, err)
				reconnectAttempts[peerInfo.ID]++
				lastAttempt[peerInfo.ID] = now
			}
		}

		ignoredPeers := node.GetIgnoredPeers()
		for _, peerID := range ignoredPeers {
			if node.IsConnected(peerID) {
				continue
			}

			if attempts, exists := reconnectAttempts[peerID]; exists && attempts >= MaxReconnectAttempts {
				continue
			}

			if lastTime, exists := lastAttempt[peerID]; exists {
				attempts := reconnectAttempts[peerID]
				backoffDuration := nextBackoff(attempts)

				if now.Sub(lastTime) < backoffDuration {
					continue
				}
			}

			peerInfo, err := node.dht.FindPeer(node.ctx, peerID)
			if err != nil {
				fmt.Printf("Não foi possível encontrar o peer %s: %s\n", peerID, err)
				reconnectAttempts[peerID]++
				lastAttempt[peerID] = now
				continue
			}

			err = node.host.Connect(node.ctx, peerInfo)
			if err == nil {
				fmt.Printf("Reconexão bem-sucedida com peer anteriormente ignorado: %s\n", peerID)
				node.SetConnected(peerID, true)
				delete(reconnectAttempts, peerID)
				node.SetIgnored(peerID, false)
				delete(lastAttempt, peerID)
			} else {
				fmt.Printf("Falha na reconexão com peer ignorado %s: %s\n", peerID, err)
				reconnectAttempts[peerID]++
				lastAttempt[peerID] = now
			}
		}

		peerChan, err := routingDiscovery.FindPeers(node.ctx, node.topicName)
		if err != nil {
			fmt.Printf("Erro na descoberta de peers: %s. Tentando novamente...\n", err)
			time.Sleep(5 * time.Second)
			continue
		}

		for peerInfo := range peerChan {
			if peerInfo.ID == node.host.ID() {
				continue
			}

			if node.IsConnected(peerInfo.ID) {
				continue
			}

			if node.IsIgnored(peerInfo.ID) {
				continue
			}

			err := node.host.Connect(node.ctx, peerInfo)
			if err != nil {
				if err.Error() == "no addresses" {
					node.SetIgnored(peerInfo.ID, true)
					continue
				}

				fmt.Printf("Falha ao conectar com %s: %s\n", peerInfo.ID, err)
				reconnectAttempts[peerInfo.ID] = 1
				lastAttempt[peerInfo.ID] = now
				continue
			}

			fmt.Printf("Conectado com sucesso a novo peer: %s\n", peerInfo.ID)
			node.SetConnected(peerInfo.ID, true)
		}

		time.Sleep(10 * time.Second)
	}
}

// GetIgnoredPeers retorna a lista de peers ignorados
func (node *Node) GetIgnoredPeers() []peer.ID {
	node.ignoreMutex.RLock()
	defer node.ignoreMutex.RUnlock()

	peers := make([]peer.ID, 0, len(node.ignore))
	for peerID := range node.ignore {
		peers = append(peers, peerID)
	}
	return peers
}

// Start inicia o nó P2P
func (node *Node) Start() error {
	priv := node.LoadOrCreateIdentity()
	staticRelaysInfo := node.convertToAddrInfo(node.staticRelays)

	opts := []libp2p.Option{
		libp2p.ListenAddrStrings(
			"/ip4/0.0.0.0/tcp/0",
			"/ip4/0.0.0.0/udp/0/quic-v1",
			"/ip4/0.0.0.0/tcp/0/ws",
		),
		libp2p.NATPortMap(),
		libp2p.EnableHolePunching(),
		libp2p.EnableRelayService(),
		libp2p.EnableAutoRelayWithStaticRelays(staticRelaysInfo),
		libp2p.ForceReachabilityPublic(),
		libp2p.Identity(priv),
	}

	host, err := libp2p.New(opts...)
	if err != nil {
		return fmt.Errorf("falha ao criar host libp2p: %w", err)
	}
	node.host = host

	_, err = relay.New(host)
	if err != nil {
		fmt.Printf("Aviso: falha ao iniciar serviço relay: %s\n", err)
	}

	ids, ok := host.Peerstore().(identify.IDService)
	if !ok {
		fmt.Println("Aviso: host não fornece serviço de identificação compatível")
	} else {
		addrF := func() []ma.Multiaddr { return host.Addrs() }
		_, err = holepunch.NewService(host, ids, addrF)
		if err != nil {
			fmt.Printf("Aviso: falha ao iniciar serviço de hole punch: %s\n", err)
		} else {
			fmt.Println("Serviço de hole punch iniciado com sucesso")
		}
	}

	err = node.SetupMDNS()
	if err != nil {
		fmt.Printf("Aviso: falha ao iniciar serviço mDNS: %s\n", err)
	}

	fmt.Printf("Peer ID: %s\n", host.ID())
	for _, a := range host.Addrs() {
		fmt.Printf("Address: %s/p2p/%s\n", a, host.ID())
	}

	host.Network().Notify(&network.NotifyBundle{
		DisconnectedF: func(n network.Network, c network.Conn) {
			pid := c.RemotePeer()
			node.SetConnected(pid, false)
			go func() {
				err := host.Connect(node.ctx, peer.AddrInfo{ID: pid})
				if err == nil {
					node.SetConnected(pid, true)
				}
			}()
		},
		ConnectedF: func(n network.Network, c network.Conn) {
			pid := c.RemotePeer()
			node.SetConnected(pid, true)
		},
	})

	go node.DiscoverPeers()

	ps, err := pubsub.NewGossipSub(
		node.ctx,
		host,
		pubsub.WithFloodPublish(true),
		pubsub.WithMessageSigning(true),
		pubsub.WithPeerExchange(true),
		pubsub.WithStrictSignatureVerification(true),
		pubsub.WithValidateQueueSize(128),
		pubsub.WithValidateThrottle(2048),
		pubsub.WithSubscriptionFilter(
			pubsub.WrapLimitSubscriptionFilter(
				pubsub.NewAllowlistSubscriptionFilter([]string{node.topicName}...),
				100,
			),
		),
	)
	if err != nil {
		return fmt.Errorf("falha ao criar pubsub: %w", err)
	}
	node.pubsub = ps

	topic, err := ps.Join(node.topicName)
	if err != nil {
		return fmt.Errorf("falha ao participar do tópico: %w", err)
	}
	node.topic = topic

	return nil
}

// GetTopic retorna o tópico atual do nó
func (node *Node) GetTopic() *pubsub.Topic {
	return node.topic
}

// Subscribe inscreve o nó no tópico atual
func (node *Node) Subscribe() (*pubsub.Subscription, error) {
	if node.topic == nil {
		return nil, fmt.Errorf("nenhum tópico disponível para inscrição")
	}
	return node.topic.Subscribe()
}

// PublishMessage publica uma mensagem no tópico atual
func (node *Node) PublishMessage(data []byte) error {
	if node.topic == nil {
		return fmt.Errorf("nenhum tópico disponível para publicação")
	}
	return node.topic.Publish(node.ctx, data)
}

// Stop para o nó P2P
func (node *Node) Stop() {
	fmt.Println("Iniciando encerramento do nó P2P...")

	if node.host != nil {
		fmt.Println("Salvando lista de peers...")
		node.SaveKnownPeers()
	}

	if node.cancel != nil {
		fmt.Println("Cancelando contexto...")
		node.cancel()
	}

	if node.topic != nil {
		fmt.Println("Fechando tópico...")
	}

	if node.pubsub != nil {
		fmt.Println("Fechando sistema de publicação/assinatura...")
	}

	if node.dht != nil {
		fmt.Println("Fechando DHT...")
		err := node.dht.Close()
		if err != nil {
			fmt.Printf("Erro ao fechar DHT: %s\n", err)
		}
	}

	if node.host != nil {
		fmt.Println("Fechando host libp2p...")
		err := node.host.Close()
		if err != nil {
			fmt.Printf("Erro ao fechar host: %s\n", err)
		}
	}

	fmt.Println("Nó P2P encerrado com sucesso")
}

// GetID retorna o ID do peer do nó
func (node *Node) GetID() peer.ID {
	if node.host == nil {
		return ""
	}
	return node.host.ID()
}

// GetAddrs retorna os endereços do nó
func (node *Node) GetAddrs() []ma.Multiaddr {
	if node.host == nil {
		return nil
	}
	return node.host.Addrs()
}

// Context retorna o contexto associado ao nó
func (node *Node) Context() context.Context {
	return node.ctx
}

// SetLogLevel define o nível de log para o pacote
func SetLogLevel(level string) {
	log.SetAllLoggers(log.LevelError)
	if level != "" {
		log.SetLogLevel("p2p", level)
	}
}
