package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	libHost "github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/multiformats/go-multiaddr"
)

type FileMetadata struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

var (
	host       libHost.Host
	peerFiles  = make(map[string][]FileMetadata) // Map of peer ID to their files
	localFiles []FileMetadata                    // Files on this node
	filesMutex sync.Mutex                        // Mutex to protect file lists
	filesDir   = "./shared"                      // Directory to store uploaded files
)

func handleStream(s network.Stream) {
	log.Printf("Established stream with peer: %s\n", s.Conn().RemotePeer().String())

	rw := bufio.NewReadWriter(bufio.NewReader(s), bufio.NewWriter(s))

	go func() {
		// Continuously read data from the stream
		for {
			str, err := rw.ReadString('\n')
			if err != nil {
				log.Printf("Error reading from stream: %v", err)
				return
			}
			if str != "" {
				var peerFilesReceived []FileMetadata
				err := json.Unmarshal([]byte(str), &peerFilesReceived)
				if err != nil {
					log.Printf("Error unmarshaling peer files: %v", err)
					continue
				}

				// Update the peerFiles map with received data
				peerID := s.Conn().RemotePeer().String()
				filesMutex.Lock()
				peerFiles[peerID] = peerFilesReceived
				filesMutex.Unlock()

				log.Printf("Updated files from peer %s: %v", peerID, peerFilesReceived)

				// Broadcast the received metadata to all other connected peers
				broadcastToPeers(peerID, peerFilesReceived)
			}
		}
	}()
}

func broadcastToPeers(senderID string, files []FileMetadata) {
	filesJSON, _ := json.Marshal(files)

	for _, conn := range host.Network().Conns() {
		peerID := conn.RemotePeer().String()
		if peerID == senderID {
			continue // Skip broadcasting back to the sender
		}

		stream, err := host.NewStream(context.Background(), conn.RemotePeer(), "/fileshare/1.0.0")
		if err != nil {
			log.Printf("Failed to open stream to peer %s: %v", peerID, err)
			continue
		}

		rw := bufio.NewReadWriter(bufio.NewReader(stream), bufio.NewWriter(stream))
		_, err = rw.WriteString(fmt.Sprintf("%s\n", filesJSON))
		if err != nil {
			log.Printf("Failed to send file metadata to peer %s: %v", peerID, err)
		}
		_ = rw.Flush()
	}
}

func broadcastFileMetadata() {
	filesMutex.Lock()
	localFilesJSON, _ := json.Marshal(localFiles)
	filesMutex.Unlock()

	for _, conn := range host.Network().Conns() {
		stream, err := host.NewStream(context.Background(), conn.RemotePeer(), "/fileshare/1.0.0")
		if err != nil {
			log.Printf("Failed to open stream to peer %s: %v", conn.RemotePeer().String(), err)
			continue
		}

		rw := bufio.NewReadWriter(bufio.NewReader(stream), bufio.NewWriter(stream))
		_, err = rw.WriteString(fmt.Sprintf("%s\n", localFilesJSON))
		if err != nil {
			log.Printf("Failed to send file metadata to peer %s: %v", conn.RemotePeer().String(), err)
		}
		_ = rw.Flush()
	}
}

func connectToPeer(peerAddr string) error {
	maddr, err := multiaddr.NewMultiaddr(peerAddr)
	if err != nil {
		return fmt.Errorf("invalid multiaddress: %v", err)
	}

	info, err := peer.AddrInfoFromP2pAddr(maddr)
	if err != nil {
		return fmt.Errorf("failed to parse peer address: %v", err)
	}

	host.Peerstore().AddAddrs(info.ID, info.Addrs, peerstore.PermanentAddrTTL)

	// Open a stream to the peer
	s, err := host.NewStream(context.Background(), info.ID, "/fileshare/1.0.0")
	if err != nil {
		return fmt.Errorf("failed to create stream: %v", err)
	}

	// Handle the stream
	handleStream(s)
	return nil
}

func readData(rw *bufio.ReadWriter) {
	for {
		str, _ := rw.ReadString('\n')
		if str == "" {
			return
		}
		if str != "\n" {
			fmt.Printf("\x1b[32m%s\x1b[0m> ", str)
		}
	}
}

func writeData(rw *bufio.ReadWriter) {
	stdReader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		sendData, err := stdReader.ReadString('\n')
		if err != nil {
			panic(err)
		}
		rw.WriteString(fmt.Sprintf("%s\n", sendData))
		rw.Flush()
	}
}

func uploadFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST is supported", http.StatusMethodNotAllowed)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Failed to read file", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	// Save file locally
	filePath := filepath.Join(filesDir, header.Filename)
	os.MkdirAll(filesDir, os.ModePerm)
	out, err := os.Create(filePath)
	if err != nil {
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}
	defer out.Close()

	size, err := io.Copy(out, file)
	if err != nil {
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}

	// Add to local files
	filesMutex.Lock()
	localFiles = append(localFiles, FileMetadata{Name: header.Filename, Size: size})
	filesMutex.Unlock()

	// Broadcast the updated metadata to all peers
	broadcastFileMetadata()

	fmt.Fprintf(w, "File uploaded successfully: %s", header.Filename)
}

func getFiles(w http.ResponseWriter, r *http.Request) {
	filesMutex.Lock()
	defer filesMutex.Unlock()
	json.NewEncoder(w).Encode(localFiles)
}

func getPeersAndFiles(w http.ResponseWriter, r *http.Request) {
	peerData := map[string][]FileMetadata{}
	filesMutex.Lock()
	for peerID, files := range peerFiles {
		peerData[peerID] = files
	}
	filesMutex.Unlock()

	json.NewEncoder(w).Encode(peerData)
}

func startWebInterface(port int) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "static/index.html")
	})
	http.HandleFunc("/upload", uploadFile)
	http.HandleFunc("/files", getFiles)
	http.HandleFunc("/peers", getPeersAndFiles)

	addr := fmt.Sprintf(":%d", port)
	log.Printf("Starting web interface at http://localhost%s\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func main() {
	sourcePort := flag.Int("sp", 0, "Source port number")
	dest := flag.String("d", "", "Destination multiaddr string")
	port := flag.Int("port", 8080, "Port for the web interface")
	flag.Parse()

	var r io.Reader
	r = rand.Reader

	prvKey, _, err := crypto.GenerateKeyPairWithReader(crypto.RSA, 2048, r)
	if err != nil {
		panic(err)
	}

	sourceMultiAddr, _ := multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", *sourcePort))
	host, err = libp2p.New(
		libp2p.ListenAddrs(sourceMultiAddr),
		libp2p.Identity(prvKey),
	)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Node started with ID: %s\n", host.ID())
	fmt.Println("Listening on addresses:")
	for _, addr := range host.Addrs() {
		fmt.Printf(" - %s/p2p/%s\n", addr.String(), host.ID())
	}

	go startWebInterface(*port)

	// Set stream handler for the file-sharing protocol
	host.SetStreamHandler("/fileshare/1.0.0", handleStream)

	// Connect to destination peer if provided
	if *dest != "" {
		err := connectToPeer(*dest)
		if err != nil {
			log.Fatalf("Failed to connect to peer: %v", err)
		}
	}

	select {}
}
