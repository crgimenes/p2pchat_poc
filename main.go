package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/ipfs/go-log/v2"

	"e1/p2pnode"
)

const (
	TopicName = "p2pchat"
	KeyFile   = "node.key"
	PeersFile = "known_peers.json"
	LogFile   = "p2pchat.log"
)

func main() {
	// Configure o nível de log para minimizar mensagens desnecessárias
	log.SetAllLoggers(log.LevelError)

	// Crie e inicie o nó P2P
	node := p2pnode.NewNode(TopicName, KeyFile, PeersFile)

	// Inicializa o arquivo de log
	err := node.InitLogFile(LogFile)
	if err != nil {
		fmt.Printf("Aviso: não foi possível inicializar o arquivo de log: %s\n", err)
	}

	err = node.Start()
	if err != nil {
		panic(fmt.Sprintf("Falha ao iniciar nó P2P: %s", err))
	}

	// Configure manipulador de sinais para encerramento controlado
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	var wg sync.WaitGroup

	// Goroutine para tratamento de sinais de interrupção
	wg.Add(1)
	go func() {
		defer wg.Done()
		sig := <-signalChan
		fmt.Printf("\nSinal recebido: %v, iniciando encerramento controlado...\n", sig)

		signal.Stop(signalChan)
		node.CloseLogFile() // Fecha o arquivo de log
		node.Stop()
	}()

	// Goroutine para leitura de mensagens do console
	wg.Add(1)
	go func() {
		defer wg.Done()
		reader := bufio.NewReader(os.Stdin)
		ctx := node.Context()
		for {
			select {
			case <-ctx.Done():
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

				s = strings.TrimSpace(s)

				// Verifica se é um comando
				if strings.HasPrefix(s, "!") {
					// Processar como comando
					result, err := node.ProcessCommand(s)
					if err != nil {
						fmt.Printf("Erro ao processar comando: %s\n", err)
					} else if result != "" {
						fmt.Println(result)
					}
				} else {
					// Processar como mensagem normal
					err = node.PublishMessage([]byte(s))
					if err != nil {
						fmt.Printf("Erro ao publicar mensagem: %s\n", err)
						node.LogMessage("Erro ao publicar mensagem: %s", err)
					} else {
						node.LogMessage("Mensagem enviada: %s", s)
					}
				}
			}
		}
	}()

	// Inscreva-se no tópico para receber mensagens
	sub, err := node.Subscribe()
	if err != nil {
		fmt.Printf("Falha ao inscrever-se no tópico: %s\n", err)
		node.CloseLogFile()
		node.Stop()
		os.Exit(1)
	}

	// Goroutine para processamento de mensagens recebidas
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx := node.Context()
		for {
			select {
			case <-ctx.Done():
				fmt.Println("Encerrando processamento de mensagens...")
				return
			default:
				m, err := sub.Next(ctx)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					fmt.Printf("Erro ao receber mensagem: %s\n", err)
					node.LogMessage("Erro ao receber mensagem: %s", err)
					continue
				}

				// Registra a mensagem recebida no log
				message := string(m.Data)
				node.LogMessage("Mensagem recebida de %s: %s", m.ReceivedFrom, message)

				// Exibe a mensagem no console
				fmt.Printf("%s: %s\n", m.ReceivedFrom, message)
			}
		}
	}()

	// Mensagem inicial após inicialização bem-sucedida
	fmt.Println("P2P Chat iniciado. Para listar peers online, digite !op")
	fmt.Println("Os logs do sistema estão sendo gravados em:", filepath.Join(filepath.Dir(LogFile), LogFile))

	// Aguarde todas as goroutines terminarem
	wg.Wait()
	fmt.Println("Programa encerrado com sucesso")
}
