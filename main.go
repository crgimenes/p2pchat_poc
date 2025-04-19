package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"sort"

	"os/signal"
	"syscall"

	log "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns" // Adicionar importação para mDNS
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/libp2p/go-libp2p/p2p/protocol/holepunch"
	"github.com/libp2p/go-libp2p/p2p/protocol/identify"
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
	mdnsServiceTag       = "p2pchat-poc" // Tag para o serviço mDNS
)

// P2PNode encapsula toda a funcionalidade de um nó P2P
type P2PNode struct {
	// Configuração
	topicName string
	keyFile   string
	peersFile string

	// Componentes libp2p
	host   host.Host
	dht    *dht.IpfsDHT
	pubsub *pubsub.PubSub
	topic  *pubsub.Topic

	// Estado interno com proteção contra concorrência
	connectedMutex sync.RWMutex
	connected      map[peer.ID]bool

	ignoreMutex sync.RWMutex
	ignore      map[peer.ID]bool

	knownPeersMutex sync.RWMutex
	lastSavedPeers  KnownPeers

	// Controle de contexto
	ctx    context.Context
	cancel context.CancelFunc

	// Lista de relays estáticos
	staticRelays []string
}

// NewP2PNode cria uma nova instância de P2PNode
func NewP2PNode(topicName, keyFile, peersFile string) *P2PNode {
	ctx, cancel := context.WithCancel(context.Background())

	return &P2PNode{
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

// DefaultStaticRelays retorna a lista padrão de relays estáticos
func DefaultStaticRelays() []string {
	return []string{
		"/ip4/147.75.80.110/tcp/4001/p2p/QmbFgm5rao4mLdUAaCPgRGRoqyMKfK2gCgQaT77PmPjsEY",
		"/ip4/147.75.195.153/tcp/4001/p2p/QmW9m57aiBDHAkKj9nmFSEn7ZqrcF1fZS4bipsTCHburei",
		"/dnsaddr/bootstrap.libp2p.io/p2p/QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN",
		"/dnsaddr/bootstrap.libp2p.io/p2p/QmbLHAnMoJPWSCR5Zhtx6BHJX9KiKNN6tpvbUcqanj75Nb",
	}
}

// SetStaticRelays permite substituir a lista de relays estáticos
func (node *P2PNode) SetStaticRelays(relays []string) {
	node.staticRelays = relays
}

// IsConnected verifica se um peer está conectado
func (node *P2PNode) IsConnected(peerID peer.ID) bool {
	node.connectedMutex.RLock()
	defer node.connectedMutex.RUnlock()
	return node.connected[peerID]
}

// SetConnected marca um peer como conectado
func (node *P2PNode) SetConnected(peerID peer.ID, connected bool) {
	node.connectedMutex.Lock()
	defer node.connectedMutex.Unlock()
	if connected {
		node.connected[peerID] = true
	} else {
		delete(node.connected, peerID)
	}
}

// IsIgnored verifica se um peer deve ser ignorado
func (node *P2PNode) IsIgnored(peerID peer.ID) bool {
	node.ignoreMutex.RLock()
	defer node.ignoreMutex.RUnlock()
	return node.ignore[peerID]
}

// SetIgnored marca um peer para ser ignorado ou não
func (node *P2PNode) SetIgnored(peerID peer.ID, ignored bool) {
	node.ignoreMutex.Lock()
	defer node.ignoreMutex.Unlock()
	if ignored {
		node.ignore[peerID] = true
	} else {
		delete(node.ignore, peerID)
	}
}

// GetConnectedPeers retorna uma lista de todos os peers conectados
func (node *P2PNode) GetConnectedPeers() []peer.ID {
	node.connectedMutex.RLock()
	defer node.connectedMutex.RUnlock()

	peers := make([]peer.ID, 0, len(node.connected))
	for peerID := range node.connected {
		peers = append(peers, peerID)
	}
	return peers
}

// Converte strings de endereços para peer.AddrInfo
func (node *P2PNode) convertToAddrInfo(addresses []string) []peer.AddrInfo {
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

// Carregar peers conhecidos do disco
func (node *P2PNode) loadKnownPeers() []peer.AddrInfo {
	node.knownPeersMutex.RLock()
	defer node.knownPeersMutex.RUnlock()

	var knownPeers KnownPeers

	data, err := os.ReadFile(node.peersFile)
	if err != nil {
		// Inicializa uma estrutura vazia se o arquivo não existir
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

	node.lastSavedPeers = knownPeers

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
func (node *P2PNode) saveKnownPeers() {
	node.knownPeersMutex.Lock()
	defer node.knownPeersMutex.Unlock()

	// Carregue a estrutura atual se ainda não inicializada
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

	// Atualiza peers conectados
	for peerID := range node.connected {
		peer := node.host.Peerstore().PeerInfo(peerID)

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

	// Limita o número de peers armazenados para 30, mantendo os melhores
	const maxPeers = 30
	if len(knownPeers.Peers) > maxPeers {
		knownPeers.Peers = selectBestPeers(knownPeers.Peers, maxPeers)
		changed = true
	}

	// Salva apenas se houve mudanças
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

// Seleciona os melhores peers com base em critérios de qualidade
func selectBestPeers(peers map[string]KnownPeer, limit int) map[string]KnownPeer {
	// Criar uma estrutura para classificação
	type PeerRanking struct {
		ID    string
		Peer  KnownPeer
		Score float64
	}

	// Calcular pontuação para cada peer
	var rankings []PeerRanking
	now := time.Now()

	for id, peer := range peers {
		// Pontuação baseada em:
		// 1. Número de conexões bem-sucedidas (maior é melhor)
		// 2. Quão recentemente o peer foi visto (mais recente é melhor)

		recencyScore := 1.0 / (now.Sub(peer.LastSeen).Hours() + 1.0) // Evita divisão por zero
		connectionsScore := float64(peer.SuccessfulConnections)

		// Combinação ponderada dos critérios (podemos ajustar os pesos)
		score := (recencyScore * 10.0) + connectionsScore

		rankings = append(rankings, PeerRanking{
			ID:    id,
			Peer:  peer,
			Score: score,
		})
	}

	// Ordena os peers por pontuação (maior para menor)
	sort.Slice(rankings, func(i, j int) bool {
		return rankings[i].Score > rankings[j].Score
	})

	// Seleciona apenas os top 'limit' peers
	result := make(map[string]KnownPeer)
	for i := 0; i < limit && i < len(rankings); i++ {
		result[rankings[i].ID] = rankings[i].Peer
	}

	return result
}

// Função auxiliar para determinar se um endereço é local
func isLocalAddress(addr ma.Multiaddr) bool {
	// Implementação simplificada - você pode expandir conforme necessário
	return strings.Contains(addr.String(), "/ip4/127.0.0.1/") ||
		strings.Contains(addr.String(), "/ip4/192.168.") ||
		strings.Contains(addr.String(), "/ip4/10.") ||
		strings.Contains(addr.String(), "/ip4/172.16.")
}

// Calcula o próximo tempo de espera para backoff exponencial
func nextBackoff(attempt int) time.Duration {
	backoffDuration := initialBackoff * time.Duration(1<<uint(attempt))
	backoffDuration = max(backoffDuration, maxBackoff)
	return backoffDuration
}

// LoadOrCreateIdentity carrega ou cria identidade do nó
func (node *P2PNode) LoadOrCreateIdentity() crypto.PrivKey {
	if _, err := os.Stat(node.keyFile); os.IsNotExist(err) {
		fmt.Println("Arquivo de chaves não encontrado. Gerando novas chaves...")
		priv, _, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		if err != nil {
			panic(err)
		}
		node.saveKeyToFile(priv)
		return priv
	}

	keyBytes, err := os.ReadFile(node.keyFile)
	if err != nil {
		fmt.Printf("Erro ao ler chaves: %s. Gerando novas chaves...\n", err)
		priv, _, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		if err != nil {
			panic(err)
		}
		node.saveKeyToFile(priv)
		return priv
	}

	priv, err := crypto.UnmarshalPrivateKey(keyBytes)
	if err != nil {
		fmt.Printf("Erro ao carregar chaves: %s. Gerando novas chaves...\n", err)
		priv, _, err = crypto.GenerateKeyPair(crypto.RSA, 2048)
		if err != nil {
			panic(err)
		}
		node.saveKeyToFile(priv)
		return priv
	}

	fmt.Println("Chaves carregadas com sucesso do arquivo.")
	return priv
}

func (node *P2PNode) saveKeyToFile(priv crypto.PrivKey) error {
	keyBytes, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return err
	}
	return os.WriteFile(node.keyFile, keyBytes, 0600)
}

// InitDHT inicializa o DHT para descoberta de peers
func (node *P2PNode) InitDHT() *dht.IpfsDHT {
	// Start a DHT, for use in peer discovery. We can't just make a new DHT
	// client because we want each peer to maintain its own local copy of the
	// DHT, so that the bootstrapping node of the DHT can go down without
	// inhibiting future peer discovery.
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

// MDNSNotifee implementa interface de notificação de descoberta para mDNS
type MDNSNotifee struct {
	node *P2PNode
}

// HandlePeerFound é chamada quando mDNS descobre um novo peer
func (n *MDNSNotifee) HandlePeerFound(pi peer.AddrInfo) {
	if pi.ID == n.node.host.ID() {
		return // Ignora auto-descoberta
	}

	fmt.Printf("Peer encontrado via mDNS: %s\n", pi.ID)

	// Tenta se conectar ao peer descoberto
	err := n.node.host.Connect(context.Background(), pi)
	if err != nil {
		fmt.Printf("Falha ao conectar via mDNS com %s: %s\n", pi.ID, err)
		return
	}

	fmt.Printf("Conectado via mDNS com %s\n", pi.ID)
	n.node.SetConnected(pi.ID, true)
}

// SetupMDNS inicia serviço de descoberta mDNS
func (node *P2PNode) SetupMDNS() error {
	// Configurar serviço mDNS para descoberta local
	service := mdns.NewMdnsService(node.host, mdnsServiceTag, &MDNSNotifee{node: node})
	if service == nil {
		return fmt.Errorf("falha ao criar serviço mDNS")
	}

	fmt.Println("Serviço de descoberta local mDNS iniciado")
	return nil
}

// DiscoverPeers implementa a descoberta contínua de peers
func (node *P2PNode) DiscoverPeers() {
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
				node.saveKnownPeers()
			case <-node.ctx.Done():
				return
			}
		}
	}()

	// Inicializa DHT se ainda não estiver inicializado
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

		// Tenta reconectar aos peers conhecidos
		knownPeers := node.loadKnownPeers()
		now := time.Now()

		// Reconexão com peers conhecidos
		for _, peerInfo := range knownPeers {
			if peerInfo.ID == node.host.ID() {
				continue
			}

			if node.IsConnected(peerInfo.ID) {
				continue // Já conectado
			}

			// Verifica se atingiu o limite de tentativas
			if attempts, exists := reconnectAttempts[peerInfo.ID]; exists && attempts >= maxReconnectAttempts {
				fmt.Printf("Desistindo de reconexão com %s após %d tentativas\n", peerInfo.ID, attempts)
				node.SetIgnored(peerInfo.ID, true)
				continue
			}

			// Aplica backoff exponencial
			if lastTime, exists := lastAttempt[peerInfo.ID]; exists {
				attempts := reconnectAttempts[peerInfo.ID]
				backoffDuration := nextBackoff(attempts)

				if now.Sub(lastTime) < backoffDuration {
					continue // Ainda não é hora de tentar novamente
				}
			}

			// Tenta reconectar
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

		// Tentar reconectar peers ignorados anteriormente
		ignoredPeers := node.GetIgnoredPeers()
		for _, peerID := range ignoredPeers {
			if node.IsConnected(peerID) {
				continue // Já está conectado
			}

			// Verifica se atingiu o limite de tentativas
			if attempts, exists := reconnectAttempts[peerID]; exists && attempts >= maxReconnectAttempts {
				continue // Desistir permanentemente após muitas tentativas
			}

			// Aplica backoff exponencial
			if lastTime, exists := lastAttempt[peerID]; exists {
				attempts := reconnectAttempts[peerID]
				backoffDuration := nextBackoff(attempts)

				if now.Sub(lastTime) < backoffDuration {
					continue // Ainda não é hora de tentar novamente
				}
			}

			// Tenta encontrar o peer diretamente usando o DHT
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

		// Procura por novos peers
		peerChan, err := routingDiscovery.FindPeers(node.ctx, node.topicName)
		if err != nil {
			fmt.Printf("Erro na descoberta de peers: %s. Tentando novamente...\n", err)
			time.Sleep(5 * time.Second)
			continue
		}

		// Processa novos peers encontrados
		for peerInfo := range peerChan {
			if peerInfo.ID == node.host.ID() {
				continue // Evitar auto-conexão
			}

			if node.IsConnected(peerInfo.ID) {
				continue // Já conectado
			}

			if node.IsIgnored(peerInfo.ID) {
				continue // Ignorado anteriormente
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

		// Aguarda antes da próxima iteração
		time.Sleep(10 * time.Second)
	}
}

// GetIgnoredPeers retorna uma lista de todos os peers ignorados
func (node *P2PNode) GetIgnoredPeers() []peer.ID {
	node.ignoreMutex.RLock()
	defer node.ignoreMutex.RUnlock()

	peers := make([]peer.ID, 0, len(node.ignore))
	for peerID := range node.ignore {
		peers = append(peers, peerID)
	}
	return peers
}

// Start inicia o nó P2P com todas as funcionalidades
func (node *P2PNode) Start() error {
	// Carrega ou cria identidade
	priv := node.LoadOrCreateIdentity()

	// Configura e inicializa o host libp2p
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

	// Configura serviços adicionais
	_, err = relay.New(host)
	if err != nil {
		fmt.Printf("Aviso: falha ao iniciar serviço relay: %s\n", err)
	}

	// Configura hole punching
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

	// Configura mDNS para descoberta local
	err = node.SetupMDNS()
	if err != nil {
		fmt.Printf("Aviso: falha ao iniciar serviço mDNS: %s\n", err)
	}

	// Exibe informação do host
	fmt.Printf("Peer ID: %s\n", host.ID())
	for _, a := range host.Addrs() {
		fmt.Printf("Address: %s/p2p/%s\n", a, host.ID())
	}

	// Configura notificações de rede
	host.Network().Notify(&network.NotifyBundle{
		DisconnectedF: func(n network.Network, c network.Conn) {
			pid := c.RemotePeer()
			node.SetConnected(pid, false)
			// Tenta reconectar
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
			// Opcional: logar conexões novas
			// fmt.Printf("Nova conexão: %s via %s\n", pid, c.RemoteMultiaddr())
		},
	})

	// Inicia descoberta de peers em goroutine separada
	go node.DiscoverPeers()

	// Inicializa pubsub
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

	// Participa do tópico
	topic, err := ps.Join(node.topicName)
	if err != nil {
		return fmt.Errorf("falha ao participar do tópico: %w", err)
	}
	node.topic = topic

	return nil
}

// GetTopic retorna o tópico atual
func (node *P2PNode) GetTopic() *pubsub.Topic {
	return node.topic
}

// Subscribe inscreve-se no tópico atual
func (node *P2PNode) Subscribe() (*pubsub.Subscription, error) {
	if node.topic == nil {
		return nil, fmt.Errorf("nenhum tópico disponível para inscrição")
	}
	return node.topic.Subscribe()
}

// PublishMessage publica uma mensagem no tópico
func (node *P2PNode) PublishMessage(data []byte) error {
	if node.topic == nil {
		return fmt.Errorf("nenhum tópico disponível para publicação")
	}
	return node.topic.Publish(node.ctx, data)
}

// Stop para todas as operações do nó de forma ordenada
func (node *P2PNode) Stop() {
	fmt.Println("Iniciando encerramento do nó P2P...")

	// Primeiro salvamos os peers conhecidos
	if node.host != nil {
		fmt.Println("Salvando lista de peers...")
		node.saveKnownPeers()
	}

	// Cancelamos o contexto para sinalizar para todas as goroutines pararem
	if node.cancel != nil {
		fmt.Println("Cancelando contexto...")
		node.cancel()
	}

	// Fechamos componentes na ordem inversa de criação
	if node.topic != nil {
		fmt.Println("Fechando tópico...")
		// Nota: topic.Close() não existe na API atual, mas seria ideal chamá-lo se existisse
	}

	if node.pubsub != nil {
		fmt.Println("Fechando sistema de publicação/assinatura...")
		// Mesma observação sobre pubsub.Close()
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

func main() {
	log.SetAllLoggers(log.LevelError)
	flag.Parse()

	topicNameFlag := flag.String("topic", "p2pchat", "Nome do tópico para comunicação P2P")

	// Cria nova instância do componente P2P
	p2pNode := NewP2PNode(*topicNameFlag, KeyFile, PeersFile)

	// Inicia o nó
	err := p2pNode.Start()
	if err != nil {
		panic(fmt.Sprintf("Falha ao iniciar nó P2P: %s", err))
	}

	// Configura captura de sinais para encerramento limpo
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	// WaitGroup para aguardar o término de todas as goroutines
	var wg sync.WaitGroup

	// Inicia rotina para processar sinais
	wg.Add(1)
	go func() {
		defer wg.Done()
		sig := <-signalChan
		fmt.Printf("\nSinal recebido: %v, iniciando encerramento controlado...\n", sig)

		// Desregistra o handler para evitar múltiplos sinais
		signal.Stop(signalChan)

		// Inicia processo de encerramento do nó
		p2pNode.Stop()
	}()

	// Inicia rotina de leitura do console com controle de encerramento
	wg.Add(1)
	go func() {
		defer wg.Done()
		reader := bufio.NewReader(os.Stdin)
		for {
			select {
			case <-p2pNode.ctx.Done():
				fmt.Println("Encerrando rotina de leitura do console...")
				return
			default:
				s, err := reader.ReadString('\n')
				if err != nil {
					if err == io.EOF || strings.Contains(err.Error(), "closed") {
						return
					}
					fmt.Printf("Erro na leitura do console: %s\n", err)
					continue
				}

				// Publica a mensagem
				err = p2pNode.PublishMessage([]byte(s))
				if err != nil {
					fmt.Printf("Erro ao publicar mensagem: %s\n", err)
				}
			}
		}
	}()

	// Inscreve-se no tópico para receber mensagens
	sub, err := p2pNode.Subscribe()
	if err != nil {
		fmt.Printf("Falha ao inscrever-se no tópico: %s\n", err)
		p2pNode.Stop()
		os.Exit(1)
	}

	// Processa mensagens recebidas até cancelamento
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-p2pNode.ctx.Done():
				fmt.Println("Encerrando processamento de mensagens...")
				return
			default:
				m, err := sub.Next(p2pNode.ctx)
				if err != nil {
					if err == context.Canceled {
						return
					}
					fmt.Printf("Erro ao receber mensagem: %s\n", err)
					continue
				}
				fmt.Printf("%s: %s", m.ReceivedFrom, string(m.Message.Data))
			}
		}
	}()

	// Aguarda todas as goroutines terminarem
	wg.Wait()
	fmt.Println("Programa encerrado com sucesso")
}
