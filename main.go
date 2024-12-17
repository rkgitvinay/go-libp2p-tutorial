package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
)

var (
	streams        sync.Map
	bootstrapPeers = []string{"/ip4/127.0.0.1/tcp/4001/p2p/QmBootstrapPeerID"} // Replace with your bootstrap node details
)

// DHT and Peer Discovery
func setupDHT(ctx context.Context, h host.Host) *dht.IpfsDHT {
	kadDHT, err := dht.New(ctx, h, dht.Mode(dht.ModeAuto))
	if err != nil {
		log.Fatalf("Failed to create DHT: %v", err)
	}

	if err := kadDHT.Bootstrap(ctx); err != nil {
		log.Fatalf("Failed to bootstrap DHT: %v", err)
	}

	for _, addr := range bootstrapPeers {
		peerInfo, err := peer.AddrInfoFromString(addr)
		if err != nil {
			continue
		}
		h.Peerstore().AddAddrs(peerInfo.ID, peerInfo.Addrs, time.Hour)
		if err := h.Connect(ctx, *peerInfo); err == nil {
			fmt.Printf("Connected to bootstrap node: %s\n", addr)
		}
	}

	return kadDHT
}

// MDNS for Local Network Discovery
func startMDNS(h host.Host, serviceTag string) {
	notifee := &mdnsNotifee{h: h}
	service := mdns.NewMdnsService(h, serviceTag, notifee)
	if err := service.Start(); err != nil {
		log.Fatalf("Failed to start MDNS: %v", err)
	}
	fmt.Println("mDNS peer discovery started...")
}

type mdnsNotifee struct {
	h host.Host
}

func (n *mdnsNotifee) HandlePeerFound(pi peer.AddrInfo) {
	fmt.Printf("Discovered new peer: %s\n", pi.ID.String())
	n.h.Connect(context.Background(), pi)
}

// Stream Handlers
func handleStream(s network.Stream) {
	peerID := s.Conn().RemotePeer()
	log.Printf("New stream from: %s\n", peerID)
	rw := bufio.NewReadWriter(bufio.NewReader(s), bufio.NewWriter(s))
	streams.Store(peerID, rw)
	go readData(rw, peerID)
}

func readData(rw *bufio.ReadWriter, peerID peer.ID) {
	for {
		str, err := rw.ReadString('\n')
		if err != nil {
			log.Printf("Peer %s disconnected.\n", peerID)
			streams.Delete(peerID)
			return
		}
		fmt.Printf("[PEER %s] %s", peerID.String(), str)
	}
}

// File Broadcasting
func broadcastFile(filename string) {
	file, err := os.Open(filename)
	if err != nil {
		fmt.Printf("Failed to open file: %v\n", err)
		return
	}
	defer file.Close()

	info, _ := file.Stat()
	filesize := info.Size()

	streams.Range(func(_, value interface{}) bool {
		rw := value.(*bufio.ReadWriter)
		rw.WriteString(fmt.Sprintf("FILE:%s %d\n", filepath.Base(filename), filesize))
		rw.Flush()

		buf := make([]byte, 4096)
		for {
			n, err := file.Read(buf)
			if err != nil && err != io.EOF {
				return false
			}
			if n == 0 {
				break
			}
			rw.Write(buf[:n])
			rw.Flush()
		}
		return true
	})

	fmt.Println("File broadcast completed.")
}

func writeData() {
	stdReader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		sendData, _ := stdReader.ReadString('\n')

		if len(sendData) > 5 && sendData[:5] == "send:" {
			broadcastFile(sendData[5 : len(sendData)-1])
		} else {
			streams.Range(func(_, value interface{}) bool {
				rw := value.(*bufio.ReadWriter)
				rw.WriteString(sendData)
				rw.Flush()
				return true
			})
		}
	}
}

func createHost(ctx context.Context, port int) host.Host {
	listenAddr := fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port)
	h, err := libp2p.New(
		libp2p.ListenAddrStrings(listenAddr),
		libp2p.EnableAutoRelay(),
		libp2p.EnableRelay(),
		libp2p.NATPortMap(),
	)
	if err != nil {
		log.Fatalf("Failed to create host: %v", err)
	}
	return h
}

func main() {
	sourcePort := flag.Int("sp", 4001, "Source port")
	flag.Parse()

	ctx := context.Background()
	host := createHost(ctx, *sourcePort)

	// Start Peer Discovery
	setupDHT(ctx, host)
	startMDNS(host, "libp2p-app")

	// Set Stream Handler
	host.SetStreamHandler("/chat/1.0.0", handleStream)
	fmt.Printf("Listening on port %d. Host ID: %s\n", *sourcePort, host.ID().String())

	// Interactive Prompt
	go writeData()
	select {}
}
