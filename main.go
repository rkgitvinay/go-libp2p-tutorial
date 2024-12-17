package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"os"
	"path/filepath"
	"sync"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"

	"github.com/multiformats/go-multiaddr"
)

var (
	streams    sync.Map                     // Existing map for peer streams
	knownPeers = make(map[peer.ID]struct{}) // Tracks known peer IDs
	peersMutex sync.Mutex
)

func handleStream(s network.Stream) {
	peerID := s.Conn().RemotePeer()
	log.Printf("New stream from: %s\n", peerID)

	// Add peer to known peers
	peersMutex.Lock()
	knownPeers[peerID] = struct{}{}
	peersMutex.Unlock()

	// Create a buffered read-writer for the stream
	rw := bufio.NewReadWriter(bufio.NewReader(s), bufio.NewWriter(s))

	// Store the stream for bidirectional communication
	streams.Store(peerID, rw)

	// Handle reading and writing in separate goroutines
	go readData(rw, peerID)
	go writeData()

	// Keep the stream open
}

func readData(rw *bufio.ReadWriter, peerID peer.ID) {
	for {
		str, _ := rw.ReadString('\n')

		if str == "" {
			log.Printf("Connection with peer %s closed.\n", peerID)
			streams.Delete(peerID)
			peersMutex.Lock()
			delete(knownPeers, peerID) // Remove peer from knownPeers
			peersMutex.Unlock()
			return
		}

		if str != "\n" {
			switch {
			case str == "LIST\n":
				// Respond with all known peer IDs
				peersMutex.Lock()
				for p := range knownPeers {
					rw.WriteString(fmt.Sprintf("PEER:%s\n", p))
				}
				rw.Flush()
				peersMutex.Unlock()
			case len(str) > 5 && str[:5] == "PEER:":
				// Register received peer ID
				newPeer := str[5 : len(str)-1]
				peersMutex.Lock()
				knownPeers[peer.ID(newPeer)] = struct{}{}
				peersMutex.Unlock()
				fmt.Printf("Discovered Peer: %s\n", newPeer)
			case len(str) > 5 && str[:5] == "FILE:":
				handleFileReceive(str[5:], rw)
			default:
				// Display chat message
				fmt.Printf("[PEER %s] %s> ", peerID, str)

				// Forward the message to all other connected peers
				streams.Range(func(key, value interface{}) bool {
					if key != peerID { // Exclude the original sender
						peerRW := value.(*bufio.ReadWriter)
						peerRW.WriteString(fmt.Sprintf("[FORWARDED from %s]: %s", peerID, str))
						peerRW.Flush()
					}
					return true
				})
			}
		}
	}
}

func handleFileReceive(metadata string, rw *bufio.ReadWriter) {
	// Extract filename and size from metadata
	var filename string
	var filesize int64
	fmt.Sscanf(metadata, "%s %d", &filename, &filesize)

	newFileName := "./received/" + filepath.Base(filename)
	fmt.Printf("Receiving file: %s (%d bytes)\n", filename, filesize)

	file, err := os.Create(newFileName)
	if err != nil {
		fmt.Printf("Failed to create file: %v\n", err)
		return
	}
	defer file.Close()

	received := int64(0)
	buf := make([]byte, 4096)

	for received < filesize {
		n, err := rw.Read(buf)
		if err != nil {
			fmt.Printf("Error reading file: %v\n", err)
			return
		}

		_, err = file.Write(buf[:n])
		if err != nil {
			fmt.Printf("Error writing file: %v\n", err)
			return
		}

		received += int64(n)
	}

	fmt.Printf("File %s received and saved successfully.\n", newFileName)

	// Forward the file to all other connected peers
	forwardFile(newFileName, filesize, rw)
}

func forwardFile(filename string, filesize int64, senderRW *bufio.ReadWriter) {
	fmt.Printf("Forwarding file: %s (%d bytes)\n", filename, filesize)

	file, err := os.Open(filename)
	if err != nil {
		fmt.Printf("Failed to open file for forwarding: %v\n", err)
		return
	}
	defer file.Close()

	// Send file metadata and data to all connected peers except the sender
	streams.Range(func(key, value interface{}) bool {
		peerRW := value.(*bufio.ReadWriter)
		if peerRW != senderRW { // Avoid sending back to the sender
			// Send metadata
			peerRW.WriteString(fmt.Sprintf("FILE:%s %d\n", filepath.Base(filename), filesize))
			peerRW.Flush()

			// Send file data
			buf := make([]byte, 4096)
			for {
				n, err := file.Read(buf)
				if err != nil {
					if err == io.EOF {
						break
					}
					fmt.Printf("Error reading file: %v\n", err)
					return false
				}
				peerRW.Write(buf[:n])
				peerRW.Flush()
			}
		}
		return true
	})

	fmt.Printf("File %s forwarded successfully.\n", filename)
}

func writeData() {
	stdReader := bufio.NewReader(os.Stdin)

	for {
		fmt.Print("> ")
		sendData, err := stdReader.ReadString('\n')
		if err != nil {
			panic(err)
		}

		sendData = sendData[:len(sendData)-1] // Trim newline character

		switch {
		case sendData == "list":
			// Send LIST command to all peers
			fmt.Println("Requesting list of connected peers...")
			streams.Range(func(key, value interface{}) bool {
				peerRW := value.(*bufio.ReadWriter)
				peerRW.WriteString("LIST\n")
				peerRW.Flush()
				return true
			})
		case len(sendData) > 5 && sendData[:5] == "send:":
			// Broadcast file to all connected peers
			filename := sendData[5:]
			broadcastFile(filename)
		default:
			// Broadcast chat message to all peers
			streams.Range(func(key, value interface{}) bool {
				peerRW := value.(*bufio.ReadWriter)
				peerRW.WriteString(fmt.Sprintf("%s\n", sendData))
				peerRW.Flush()
				return true
			})
		}
	}
}

func broadcastFile(filename string) {
	file, err := os.Open(filename)
	if err != nil {
		fmt.Printf("Failed to open file: %v\n", err)
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		fmt.Printf("Failed to get file info: %v\n", err)
		return
	}
	filesize := info.Size()

	// Send file metadata and data to all connected peers
	streams.Range(func(key, value interface{}) bool {
		rw := value.(*bufio.ReadWriter)

		// Send metadata
		rw.WriteString(fmt.Sprintf("FILE:%s %d\n", filename, filesize))
		rw.Flush()

		// Send file data
		buf := make([]byte, 4096)
		for {
			n, err := file.Read(buf)
			if err != nil {
				if err == io.EOF {
					break
				}
				fmt.Printf("Error reading file: %v\n", err)
				return false
			}
			rw.Write(buf[:n])
			rw.Flush()
		}
		return true
	})

	fmt.Printf("File %s broadcasted successfully.\n", filename)
}

func main() {
	sourcePort := flag.Int("sp", 0, "Source port number")
	dest := flag.String("d", "", "Destination multiaddr string")
	help := flag.Bool("help", false, "Display help")
	debug := flag.Bool("debug", false, "Debug generates the same node ID on every execution")

	flag.Parse()

	if *help {
		fmt.Printf("This program demonstrates a multi-node p2p chat and file-sharing application using libp2p\n\n")
		fmt.Println("Usage:")
		fmt.Println("  Listener: ./chat -sp <SOURCE_PORT>")
		fmt.Println("  Dialer: ./chat -d <MULTIADDR>")
		os.Exit(0)
	}

	var r io.Reader
	if *debug {
		r = mrand.New(mrand.NewSource(int64(*sourcePort)))
	} else {
		r = rand.Reader
	}

	prvKey, _, err := crypto.GenerateKeyPairWithReader(crypto.RSA, 2048, r)
	if err != nil {
		panic(err)
	}

	sourceMultiAddr, _ := multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", *sourcePort))

	host, err := libp2p.New(
		// context.Background(),
		libp2p.ListenAddrs(sourceMultiAddr),
		libp2p.Identity(prvKey),
	)

	if err != nil {
		panic(err)
	}

	host.SetStreamHandler("/chat/1.0.0", handleStream)

	if *dest == "" {
		// Listener mode
		var port string
		for _, la := range host.Network().ListenAddresses() {
			if p, err := la.ValueForProtocol(multiaddr.P_TCP); err == nil {
				port = p
				break
			}
		}

		if port == "" {
			panic("Was not able to find actual local port")
		}

		fmt.Printf("Run './chat -d /ip4/127.0.0.1/tcp/%v/p2p/%s' on another console.\n", port, host.ID())
		fmt.Printf("OR\n")
		fmt.Printf("Run 'go run main.go -d /ip4/127.0.0.1/tcp/%v/p2p/%s' on another console.\n", port, host.ID())
		fmt.Println("Waiting for incoming connections...")

		select {}
	} else {
		// Dialer mode
		maddr, err := multiaddr.NewMultiaddr(*dest)
		if err != nil {
			log.Fatalln(err)
		}

		info, err := peer.AddrInfoFromP2pAddr(maddr)
		if err != nil {
			log.Fatalln(err)
		}

		host.Peerstore().AddAddrs(info.ID, info.Addrs, peerstore.PermanentAddrTTL)

		s, err := host.NewStream(context.Background(), info.ID, "/chat/1.0.0")
		if err != nil {
			panic(err)
		}

		rw := bufio.NewReadWriter(bufio.NewReader(s), bufio.NewWriter(s))
		streams.Store(info.ID, rw)

		go writeData()
		go readData(rw, info.ID)

		select {}
	}
}
