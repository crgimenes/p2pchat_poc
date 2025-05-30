package p2pnode

import (
	"context"
	"encoding/base64"
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
	"github.com/libp2p/go-libp2p/p2p/protocol/ping"
	ma "github.com/multiformats/go-multiaddr"
)

// knownPeer representa um peer conhecido pelo nó
type knownPeer struct {
	Addr                  string    `json:"addr"`
	LastSeen              time.Time `json:"last_seen"`
	SuccessfulConnections int       `json:"successful_connections"`
}

// knownPeers contém a lista de peers conhecidos pelo nó
type KnownPeers struct {
	Peers        map[string]knownPeer `json:"peers"`
	LastModified time.Time            `json:"last_modified"`
}

// mdnsNotifee é um manipulador para notificações mDNS
type mdnsNotifee struct {
	node *Node
}

// Constantes para a configuração do nó P2P
const (
	defaultKeyFile       = "node.key"
	defaultPeersFile     = "known_peers.json"
	maxReconnectAttempts = 10
	initialBackoff       = 1 * time.Second
	maxBackoff           = 60 * time.Second
	mdnsServiceTag       = "p2pchat-poc"
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
	topic           *pubsub.Topic
	topicName       string
	staticRelays    []string
	logFile         *os.File
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
		staticRelays: defaultStaticRelays(),
	}
}

// defaultStaticRelays retorna uma lista de relays estáticos padrão
func defaultStaticRelays() []string {
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

// isConnected verifica se o nó está conectado a um peer específico
func (node *Node) isConnected(peerID peer.ID) bool {
	node.connectedMutex.RLock()
	defer node.connectedMutex.RUnlock()
	return node.connected[peerID]
}

// setConnected define o estado de conexão com um peer
func (node *Node) setConnected(peerID peer.ID, connected bool) {
	node.connectedMutex.Lock()
	defer node.connectedMutex.Unlock()
	if connected {
		node.connected[peerID] = true
		return
	}
	delete(node.connected, peerID)
}

// isIgnored verifica se um peer está sendo ignorado
func (node *Node) isIgnored(peerID peer.ID) bool {
	node.ignoreMutex.RLock()
	ret := node.ignore[peerID]
	node.ignoreMutex.RUnlock()
	return ret
}

// setIgnored define se um peer deve ser ignorado
func (node *Node) setIgnored(peerID peer.ID, ignored bool) {
	node.ignoreMutex.Lock()
	defer node.ignoreMutex.Unlock()
	if ignored {
		node.ignore[peerID] = true
		return
	}
	delete(node.ignore, peerID)
}

// getConnectedPeers retorna a lista de peers conectados
func (node *Node) getConnectedPeers() []peer.ID {
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

// loadKnownPeers carrega a lista de peers conhecidos do arquivo
func (node *Node) loadKnownPeers() []peer.AddrInfo {
	node.knownPeersMutex.RLock()
	defer node.knownPeersMutex.RUnlock()

	var knownPeers KnownPeers

	data, err := os.ReadFile(node.peersFile)
	if err != nil {
		knownPeers = KnownPeers{
			Peers:        make(map[string]knownPeer),
			LastModified: time.Now(),
		}
		node.lastSavedPeers = knownPeers
		return nil
	}

	err = json.Unmarshal(data, &knownPeers)
	if err != nil {
		fmt.Printf("Erro ao desserializar peers conhecidos: %s\n", err)
		knownPeers = KnownPeers{
			Peers:        make(map[string]knownPeer),
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

// saveKnownPeers salva a lista de peers conhecidos no arquivo
func (node *Node) saveKnownPeers() {
	node.knownPeersMutex.Lock()
	defer node.knownPeersMutex.Unlock()

	var knownPeers KnownPeers
	if node.lastSavedPeers.Peers == nil {
		knownPeers = KnownPeers{
			Peers:        make(map[string]knownPeer),
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
				knownPeers.Peers[peerID] = knownPeer{
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
func selectBestPeers(peers map[string]knownPeer, limit int) map[string]knownPeer {
	type PeerRanking struct {
		ID    string
		Peer  knownPeer
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

	result := make(map[string]knownPeer)
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
	backoffDuration := initialBackoff * time.Duration(1<<uint(attempt))
	backoffDuration = min(backoffDuration, maxBackoff)
	return backoffDuration
}

// loadOrCreateIdentity carrega ou cria uma nova identidade para o nó
func (node *Node) loadOrCreateIdentity() crypto.PrivKey {
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

	// Verifica se é um formato armored
	keyStr := string(keyBytes)

	// Se conter os marcadores de início e fim do formato armored
	if strings.Contains(keyStr, "-----BEGIN") && strings.Contains(keyStr, "-----END") {
		// Remove cabeçalhos e rodapés e espaços em branco
		keyStr = strings.ReplaceAll(keyStr, "-----BEGIN ED25519 PRIVATE KEY-----", "")
		keyStr = strings.ReplaceAll(keyStr, "-----END ED25519 PRIVATE KEY-----", "")
		keyStr = strings.TrimSpace(keyStr)

		// Remove todas as quebras de linha
		keyStr = strings.ReplaceAll(keyStr, "\n", "")

		// Decodifica base64
		decoded, err := base64.StdEncoding.DecodeString(keyStr)
		if err != nil {
			fmt.Printf("Erro ao decodificar chave armored: %s. Gerando novas chaves...\n", err)
			priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
			if err != nil {
				panic(err)
			}
			node.saveKeyToFile(priv)
			return priv
		}

		// Usa os bytes decodificados
		keyBytes = decoded
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

// saveKeyToFile salva a chave privada em um arquivo em formato armored
func (node *Node) saveKeyToFile(priv crypto.PrivKey) error {
	keyBytes, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return err
	}

	// Codificar em base64 para criar formato armored
	encoded := base64.StdEncoding.EncodeToString(keyBytes)

	// Adicionar cabeçalho e rodapé ao estilo OpenSSH
	armoredKey := "-----BEGIN ED25519 PRIVATE KEY-----\n"

	// Dividir em linhas de 64 caracteres
	for i := 0; i < len(encoded); i += 64 {
		end := i + 64
		if end > len(encoded) {
			end = len(encoded)
		}
		armoredKey += encoded[i:end] + "\n"
	}

	armoredKey += "-----END ED25519 PRIVATE KEY-----\n"

	return os.WriteFile(node.keyFile, []byte(armoredKey), 0600)
}

// initDHT inicializa a tabela hash distribuída do nó
func (node *Node) initDHT() *dht.IpfsDHT {
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
func (n *mdnsNotifee) HandlePeerFound(pi peer.AddrInfo) {
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
	n.node.setConnected(pi.ID, true)
}

// setupMDNS configura o serviço de descoberta local mDNS
func (node *Node) setupMDNS() error {
	service := mdns.NewMdnsService(node.host, mdnsServiceTag, &mdnsNotifee{node: node})
	if service == nil {
		return fmt.Errorf("falha ao criar serviço mDNS")
	}

	fmt.Println("Serviço de descoberta local mDNS iniciado")
	return nil
}

// discoverPeers inicia o processo de descoberta de peers
func (node *Node) discoverPeers() {
	reconnectAttempts := make(map[peer.ID]int)
	lastAttempt := make(map[peer.ID]time.Time)

	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				node.saveKnownPeers()
			case <-node.ctx.Done():
				return
			}
		}
	}()

	if node.dht == nil {
		node.initDHT()
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

		knownPeers := node.loadKnownPeers()
		now := time.Now()

		for _, peerInfo := range knownPeers {
			if peerInfo.ID == node.host.ID() {
				continue
			}

			if node.isConnected(peerInfo.ID) {
				continue
			}

			if attempts, exists := reconnectAttempts[peerInfo.ID]; exists && attempts >= maxReconnectAttempts {
				node.LogMessage("Desistindo de reconexão com %s após %d tentativas", peerInfo.ID, attempts)
				node.setIgnored(peerInfo.ID, true)
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
				node.LogMessage("Reconectado com sucesso ao peer conhecido: %s", peerInfo.ID)
				node.setConnected(peerInfo.ID, true)
				delete(reconnectAttempts, peerInfo.ID)
				node.setIgnored(peerInfo.ID, false)
				delete(lastAttempt, peerInfo.ID)
			} else {
				node.LogMessage("Falha na reconexão com %s: %s", peerInfo.ID, err)
				reconnectAttempts[peerInfo.ID]++
				lastAttempt[peerInfo.ID] = now
			}
		}

		ignoredPeers := node.getIgnoredPeers()
		for _, peerID := range ignoredPeers {
			if node.isConnected(peerID) {
				continue
			}

			if attempts, exists := reconnectAttempts[peerID]; exists && attempts >= maxReconnectAttempts {
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
				node.LogMessage("Não foi possível encontrar o peer %s: %s", peerID, err)
				reconnectAttempts[peerID]++
				lastAttempt[peerID] = now
				continue
			}

			err = node.host.Connect(node.ctx, peerInfo)
			if err == nil {
				node.LogMessage("Reconexão bem-sucedida com peer anteriormente ignorado: %s", peerID)
				node.setConnected(peerID, true)
				delete(reconnectAttempts, peerID)
				node.setIgnored(peerID, false)
				delete(lastAttempt, peerID)
			} else {
				node.LogMessage("Falha na reconexão com peer ignorado %s: %s", peerID, err)
				reconnectAttempts[peerID]++
				lastAttempt[peerID] = now
			}
		}

		peerChan, err := routingDiscovery.FindPeers(node.ctx, node.topicName)
		if err != nil {
			node.LogMessage("Erro na descoberta de peers: %s. Tentando novamente...", err)
			time.Sleep(5 * time.Second)
			continue
		}

		for peerInfo := range peerChan {
			if peerInfo.ID == node.host.ID() {
				continue
			}

			if node.isConnected(peerInfo.ID) {
				continue
			}

			if node.isIgnored(peerInfo.ID) {
				continue
			}

			err := node.host.Connect(node.ctx, peerInfo)
			if err != nil {
				if err.Error() == "no addresses" {
					node.setIgnored(peerInfo.ID, true)
					continue
				}

				node.LogMessage("Falha ao conectar com %s: %s", peerInfo.ID, err)
				reconnectAttempts[peerInfo.ID] = 1
				lastAttempt[peerInfo.ID] = now
				continue
			}

			node.LogMessage("Conectado com sucesso a novo peer: %s", peerInfo.ID)
			node.setConnected(peerInfo.ID, true)
		}

		time.Sleep(10 * time.Second)
	}
}

// getIgnoredPeers retorna a lista de peers ignorados
func (node *Node) getIgnoredPeers() []peer.ID {
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
	priv := node.loadOrCreateIdentity()
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
		node.LogMessage("Aviso: falha ao iniciar serviço relay: %s", err)
	}

	ids, ok := host.Peerstore().(identify.IDService)
	if !ok {
		node.LogMessage("Aviso: host não fornece serviço de identificação compatível")
	} else {
		addrF := func() []ma.Multiaddr { return host.Addrs() }
		_, err = holepunch.NewService(host, ids, addrF)
		if err != nil {
			node.LogMessage("Aviso: falha ao iniciar serviço de hole punch: %s", err)
		} else {
			node.LogMessage("Serviço de hole punch iniciado com sucesso")
		}
	}

	err = node.setupMDNS()
	if err != nil {
		node.LogMessage("Aviso: falha ao iniciar serviço mDNS: %s", err)
	}

	node.LogMessage("Peer ID: %s", host.ID())
	for _, a := range host.Addrs() {
		node.LogMessage("Address: %s/p2p/%s", a, host.ID())
	}

	host.Network().Notify(&network.NotifyBundle{
		DisconnectedF: func(n network.Network, c network.Conn) {
			pid := c.RemotePeer()
			node.setConnected(pid, false)
			node.LogMessage("Peer desconectado: %s", pid)
			go func() {
				err := host.Connect(node.ctx, peer.AddrInfo{ID: pid})
				if err == nil {
					node.setConnected(pid, true)
					node.LogMessage("Reconectado com peer: %s", pid)
				}
			}()
		},
		ConnectedF: func(n network.Network, c network.Conn) {
			pid := c.RemotePeer()
			node.setConnected(pid, true)
			node.LogMessage("Novo peer conectado: %s", pid)
		},
	})

	go node.discoverPeers()

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
	node.LogMessage("Conectado ao tópico: %s", node.topicName)

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
	node.LogMessage("Iniciando encerramento do nó P2P...")

	if node.host != nil {
		node.LogMessage("Salvando lista de peers...")
		node.saveKnownPeers()
	}

	if node.cancel != nil {
		node.LogMessage("Cancelando contexto...")
		node.cancel()
	}

	if node.topic != nil {
		node.LogMessage("Fechando tópico...")
	}

	if node.pubsub != nil {
		node.LogMessage("Fechando sistema de publicação/assinatura...")
	}

	if node.dht != nil {
		node.LogMessage("Fechando DHT...")
		err := node.dht.Close()
		if err != nil {
			node.LogMessage("Erro ao fechar DHT: %s", err)
		}
	}

	if node.host != nil {
		node.LogMessage("Fechando host libp2p...")
		err := node.host.Close()
		if err != nil {
			node.LogMessage("Erro ao fechar host: %s", err)
		}
	}

	node.LogMessage("Nó P2P encerrado com sucesso")
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

// OnlinePeers devolve só quem está inscrito, conectado e responde ao ping.
func (n *Node) OnlinePeers(ctx context.Context) ([]peer.ID, error) {
	// 1. inscritos no mesmo tópico
	ids := n.pubsub.ListPeers(n.topicName)

	// inicia serviço ping (uma vez na inicialização do nó)
	ps := ping.NewPingService(n.host)

	var ready []peer.ID
	for _, pid := range ids {
		if !n.isConnected(pid) { // 2. conexão ativa
			continue
		}
		// 3. ping rápido de 3 s para confirmar liveness
		ctxPing, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		select {
		case res := <-ps.Ping(ctxPing, pid):
			if res.Error == nil { // peer respondeu
				ready = append(ready, pid)
			}
		case <-ctxPing.Done():
			// timeout ‑ descarta
		}
	}
	return ready, nil
}

// InitLogFile inicializa o arquivo de log
func (node *Node) InitLogFile(logPath string) error {
	// Se já existe um arquivo de log aberto, fecha primeiro
	if node.logFile != nil {
		node.logFile.Close()
	}

	// Cria ou trunca o arquivo de log
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("falha ao criar arquivo de log: %w", err)
	}

	node.logFile = file
	// Escreve cabeçalho do log
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	_, err = fmt.Fprintf(node.logFile, "[%s] Sessão de log iniciada\n", timestamp)
	return err
}

// LogMessage registra uma mensagem no arquivo de log
func (node *Node) LogMessage(format string, args ...interface{}) {
	if node.logFile == nil {
		return
	}

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(node.logFile, "[%s] %s\n", timestamp, msg)
}

// CloseLogFile fecha o arquivo de log
func (node *Node) CloseLogFile() {
	if node.logFile != nil {
		node.LogMessage("Sessão de log encerrada")
		node.logFile.Close()
		node.logFile = nil
	}
}

// ProcessCommand processa comandos especiais iniciados com "!"
func (node *Node) ProcessCommand(command string) (string, error) {
	// Verifica se é realmente um comando
	if !strings.HasPrefix(command, "!") {
		return "", fmt.Errorf("não é um comando")
	}

	// Remove o prefixo "!"
	cmdText := strings.TrimPrefix(command, "!")
	// Divide pelo primeiro espaço para separar o comando dos argumentos
	parts := strings.SplitN(cmdText, " ", 2)
	cmd := parts[0]

	// Argumento opcional
	var arg string
	if len(parts) > 1 {
		arg = parts[1]
	}

	// Log do comando recebido
	node.LogMessage("Comando recebido: %s (arg: %s)", cmd, arg)

	// Processa cada comando específico
	switch strings.ToLower(cmd) {
	case "op":
		// Comando para listar peers online
		return node.handleOnlinePeersCommand()
	default:
		return fmt.Sprintf("Comando desconhecido: %s", cmd), nil
	}
}

// handleOnlinePeersCommand implementa o comando !op para listar peers online
func (node *Node) handleOnlinePeersCommand() (string, error) {
	ctx, cancel := context.WithTimeout(node.ctx, 5*time.Second)
	defer cancel()

	peers, err := node.OnlinePeers(ctx)
	if err != nil {
		return "", fmt.Errorf("erro ao obter peers online: %w", err)
	}

	if len(peers) == 0 {
		return "Nenhum peer online encontrado.", nil
	}

	result := fmt.Sprintf("Peers online (%d):\n", len(peers))
	for i, p := range peers {
		result += fmt.Sprintf("%d. %s\n", i+1, p.String())
	}

	node.LogMessage("Comando !op executado: %d peers encontrados", len(peers))
	return result, nil
}
