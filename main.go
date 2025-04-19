package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
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
)

func main() {
	// Configure o nível de log para minimizar mensagens desnecessárias
	log.SetAllLoggers(log.LevelError)

	// Crie e inicie o nó P2P
	node := p2pnode.NewNode(TopicName, KeyFile, PeersFile)
	err := node.Start()
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
				err = node.PublishMessage([]byte(s))
				if err != nil {
					fmt.Printf("Erro ao publicar mensagem: %s\n", err)
				}
			}
		}
	}()

	// Inscreva-se no tópico para receber mensagens
	sub, err := node.Subscribe()
	if err != nil {
		fmt.Printf("Falha ao inscrever-se no tópico: %s\n", err)
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
					continue
				}
				fmt.Printf("%s: %s\n", m.ReceivedFrom, string(m.Data))
			}
		}
	}()

	// Aguarde todas as goroutines terminarem
	wg.Wait()
	fmt.Println("Programa encerrado com sucesso")
}
