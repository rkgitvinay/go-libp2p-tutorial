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
	"strings"
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
	filesDir   = "./shared"                      // Directory to store uploaded files
)

var (
	globalFileMap = make(map[string][]FileMetadata) // Global map of peer ID to their files
	filesMutex    sync.Mutex                        // Mutex to protect global file map
)

// Add these new types to handle file transfer
type FileRequest struct {
	FileName string `json:"filename"`
}

type FileResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Size    int64  `json:"size,omitempty"`
}

func handleStream(s network.Stream) {
	log.Printf("Established stream with peer: %s\n", s.Conn().RemotePeer().String())

	rw := bufio.NewReadWriter(bufio.NewReader(s), bufio.NewWriter(s))

	go func() {
		for {
			str, err := rw.ReadString('\n')
			if err != nil {
				log.Printf("Error reading from stream: %v", err)
				return
			}
			if str != "" {
				var receivedGlobalMap map[string][]FileMetadata
				err := json.Unmarshal([]byte(str), &receivedGlobalMap)
				if err != nil {
					log.Printf("Error unmarshaling global map: %v", err)
					continue
				}

				// Update the global file map
				filesMutex.Lock()
				for peerID, files := range receivedGlobalMap {
					globalFileMap[peerID] = files
				}
				filesMutex.Unlock()

				log.Printf("Updated global file map: %v", globalFileMap)

				// Broadcast the updated global map to all other peers
				propagateGlobalMap(s.Conn().RemotePeer().String(), receivedGlobalMap)
			}
		}
	}()
}

// Handle actual file transfer
func handleFileTransfer(s network.Stream) {
	rw := bufio.NewReadWriter(bufio.NewReader(s), bufio.NewWriter(s))

	// Read the file request
	reqData, err := rw.ReadString('\n')
	if err != nil {
		log.Printf("Error reading file request: %v", err)
		s.Close()
		return
	}

	var fileReq FileRequest
	if err := json.Unmarshal([]byte(reqData), &fileReq); err != nil {
		log.Printf("Error unmarshaling file request: %v", err)
		s.Close()
		return
	}

	// Open the requested file
	filePath := filepath.Join(filesDir, fileReq.FileName)
	file, err := os.Open(filePath)
	if err != nil {
		log.Printf("Error opening file: %v", err)
		s.Close()
		return
	}
	defer file.Close()

	// Stream the file
	_, err = io.Copy(rw, file)
	if err != nil {
		log.Printf("Error streaming file: %v", err)
		return
	}

	// Ensure all data is written
	err = rw.Flush()
	if err != nil {
		log.Printf("Error flushing data: %v", err)
	}

	s.Close()
}

// Add new HTTP handler for initiating downloads
func handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Only GET is supported", http.StatusMethodNotAllowed)
		return
	}

	// Get parameters from URL
	peerID := r.URL.Query().Get("peer_id")
	fileName := r.URL.Query().Get("file_name")

	if peerID == "" || fileName == "" {
		http.Error(w, "Missing peer_id or filename parameter", http.StatusBadRequest)
		return
	}

	// If downloading from self, serve the local file
	if peerID == host.ID().String() {
		filePath := filepath.Join(filesDir, fileName)
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}

		// Set headers for file download
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", fileName))
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, filePath)
		return
	}

	// Check if the file exists in the local `received` directory
	receivedDir := "./received"
	localFilePath := filepath.Join(receivedDir, fileName)

	if _, err := os.Stat(localFilePath); !os.IsNotExist(err) {
		// Serve the file from the `received` directory
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", fileName))
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, localFilePath)
		return
	}

	err := streamFileToClient(w, peerID, fileName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// New function to stream file directly to HTTP client
func streamFileToClient(w http.ResponseWriter, peerID string, fileName string) error {
	// First ensure we're connected to the peer
	err := ensureConnectedToPeer(peerID)
	if err != nil {
		return fmt.Errorf("failed to connect to peer: %v", err)
	}

	// Find peer info
	peerIDObj, err := peer.Decode(peerID)
	if err != nil {
		return fmt.Errorf("invalid peer ID: %v", err)
	}

	// Open stream for file transfer
	ctx := context.Background()
	transferStream, err := host.NewStream(ctx, peerIDObj, "/filetransfer/1.0.0")
	if err != nil {
		return fmt.Errorf("failed to open transfer stream: %v", err)
	}
	defer transferStream.Close()

	// Send file request
	request := FileRequest{FileName: fileName}
	reqData, _ := json.Marshal(request)

	rw := bufio.NewReadWriter(bufio.NewReader(transferStream), bufio.NewWriter(transferStream))
	rw.WriteString(fmt.Sprintf("%s\n", reqData))
	rw.Flush()

	// Set headers for file download
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", fileName))
	w.Header().Set("Content-Type", "application/octet-stream")

	// Create a buffered writer for better performance
	bufWriter := bufio.NewWriter(w)
	defer bufWriter.Flush()

	// Stream the file directly to the client
	_, err = io.Copy(bufWriter, rw.Reader)
	if err != nil {
		return fmt.Errorf("failed to stream file: %v", err)
	}

	return nil
}

func propagateGlobalMap(senderID string, updatedGlobalMap map[string][]FileMetadata) {
	globalMapJSON, _ := json.Marshal(updatedGlobalMap)

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
		_, err = rw.WriteString(fmt.Sprintf("%s\n", globalMapJSON))
		if err != nil {
			log.Printf("Failed to send global map to peer %s: %v", peerID, err)
		}
		_ = rw.Flush()
	}
}

func broadcastGlobalMap() {
	filesMutex.Lock()
	globalMapJSON, _ := json.Marshal(globalFileMap)
	filesMutex.Unlock()

	for _, conn := range host.Network().Conns() {
		stream, err := host.NewStream(context.Background(), conn.RemotePeer(), "/fileshare/1.0.0")
		if err != nil {
			log.Printf("Failed to open stream to peer %s: %v", conn.RemotePeer().String(), err)
			continue
		}

		rw := bufio.NewReadWriter(bufio.NewReader(stream), bufio.NewWriter(stream))
		_, err = rw.WriteString(fmt.Sprintf("%s\n", globalMapJSON))
		if err != nil {
			log.Printf("Failed to send global map to peer %s: %v", conn.RemotePeer().String(), err)
		}
		_ = rw.Flush()
	}
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

	// Add addresses to peerstore with a longer TTL
	host.Peerstore().AddAddrs(info.ID, info.Addrs, peerstore.PermanentAddrTTL)

	log.Printf("Added peer %s to peerstore with addresses: %v", info.ID, info.Addrs)

	// Try to connect
	ctx := context.Background()
	if err := host.Connect(ctx, *info); err != nil {
		return fmt.Errorf("failed to connect: %v", err)
	}

	log.Printf("Successfully connected to peer %s", info.ID)

	// After connecting, exchange peer information
	exchangePeerInfo(info.ID)

	return nil
}

// Add this new function to exchange peer information
func exchangePeerInfo(peerID peer.ID) {
	stream, err := host.NewStream(context.Background(), peerID, "/peer-discovery/1.0.0")
	if err != nil {
		log.Printf("Failed to open peer info exchange stream: %v", err)
		return
	}
	defer stream.Close()

	// Send our known peers
	rw := bufio.NewReadWriter(bufio.NewReader(stream), bufio.NewWriter(stream))
	peerList := make(map[string][]string)

	for _, p := range host.Peerstore().Peers() {
		addrs := host.Peerstore().Addrs(p)
		addrStrings := make([]string, len(addrs))
		for i, addr := range addrs {
			addrStrings[i] = addr.String()
		}
		peerList[p.String()] = addrStrings
	}

	peerListBytes, _ := json.Marshal(peerList)
	rw.WriteString(fmt.Sprintf("%s\n", peerListBytes))
	rw.Flush()

	log.Printf("Sent peer information to %s", peerID)
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
	globalFileMap[host.ID().String()] = localFiles
	filesMutex.Unlock()

	// Broadcast the updated global map
	broadcastGlobalMap()

	fmt.Fprintf(w, "File uploaded successfully: %s", header.Filename)
}

func getGlobalMap(w http.ResponseWriter, r *http.Request) {
	filesMutex.Lock()
	defer filesMutex.Unlock()
	json.NewEncoder(w).Encode(globalFileMap)
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
	http.HandleFunc("/peers", getGlobalMap)
	http.HandleFunc("/global", getGlobalMap) // New endpoint

	http.HandleFunc("/download", handleDownload)

	addr := fmt.Sprintf(":%d", port)
	log.Printf("Starting web interface at http://localhost%s\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// Add this new function to handle peer discovery and connection
func ensureConnectedToPeer(peerID string) error {
	// First check if we're already connected
	peerIDObj, err := peer.Decode(peerID)
	if err != nil {
		return fmt.Errorf("invalid peer ID: %v", err)
	}

	// If we're already connected, return immediately
	if len(host.Network().ConnsToPeer(peerIDObj)) > 0 {
		fmt.Printf("Already connected to peer %s\n", peerID)
		return nil
	}

	// If not connected, try to find the peer's addresses from our peerstore
	peerInfo := host.Peerstore().PeerInfo(peerIDObj)
	if len(peerInfo.Addrs) > 0 {
		ctx := context.Background()
		if err := host.Connect(ctx, peerInfo); err != nil {
			return fmt.Errorf("failed to connect to peer: %v", err)
		}
		return nil
	}

	// If we don't have the peer's addresses, ask our connected peers
	for _, conn := range host.Network().Conns() {
		stream, err := host.NewStream(context.Background(), conn.RemotePeer(), "/peer-discovery/1.0.0")
		if err != nil {
			continue
		}
		defer stream.Close()

		// Ask for peer addresses
		rw := bufio.NewReadWriter(bufio.NewReader(stream), bufio.NewWriter(stream))
		_, err = rw.WriteString(fmt.Sprintf("%s\n", peerID))
		if err != nil {
			continue
		}
		rw.Flush()

		// Read response
		response, err := rw.ReadString('\n')
		if err != nil {
			continue
		}

		// Parse multiaddr and connect
		maddr, err := multiaddr.NewMultiaddr(strings.TrimSpace(response))
		if err != nil {
			continue
		}

		info, err := peer.AddrInfoFromP2pAddr(maddr)
		if err != nil {
			continue
		}

		ctx := context.Background()
		if err := host.Connect(ctx, *info); err != nil {
			continue
		}

		return nil
	}

	return fmt.Errorf("could not find or connect to peer")
}

// Add this function to help with debugging
func logPeerConnections() {
	log.Printf("=== Current Peer Connections ===")
	for _, conn := range host.Network().Conns() {
		log.Printf("Connected to: %s", conn.RemotePeer().String())
		log.Printf("Connection addresses: %v", conn.RemoteMultiaddr())
	}
	log.Printf("==============================")
}

// Add this new protocol handler in main()
func handlePeerDiscovery(s network.Stream) {
	rw := bufio.NewReadWriter(bufio.NewReader(s), bufio.NewWriter(s))

	log.Printf("All peers in peerstore:")
	for _, p := range host.Peerstore().Peers() {
		addrs := host.Peerstore().Addrs(p)
		log.Printf("Peer %s has addresses: %v", p.String(), addrs)
	}

	// Read requested peer ID
	requestedPeer, err := rw.ReadString('\n')
	if err != nil {
		s.Close()
		return
	}
	requestedPeer = strings.TrimSpace(requestedPeer)

	// Try to find peer in our peerstore
	peerID, err := peer.Decode(requestedPeer)
	if err != nil {
		s.Close()
		return
	}

	// If we know this peer, send back its multiaddr
	if peerInfo := host.Peerstore().PeerInfo(peerID); len(peerInfo.Addrs) > 0 {
		multiaddr := fmt.Sprintf("%s/p2p/%s\n", peerInfo.Addrs[0], peerInfo.ID)
		rw.WriteString(multiaddr)
		rw.Flush()
	}

	s.Close()
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

	// Add debug logging for protocols
	log.Printf("Registered protocols:")
	for _, p := range host.Mux().Protocols() {
		log.Printf("- %s", p)
	}

	// Set stream handler for the file-sharing protocol
	host.SetStreamHandler("/fileshare/1.0.0", handleStream)

	// Add these new protocol handlers
	host.SetStreamHandler("/filetransfer/1.0.0", handleFileTransfer)

	// Add the peer discovery protocol handler
	host.SetStreamHandler("/peer-discovery/1.0.0", handlePeerDiscovery)

	// Connect to destination peer if provided
	if *dest != "" {
		err := connectToPeer(*dest)
		if err != nil {
			log.Fatalf("Failed to connect to peer: %v", err)
		}
	}

	logPeerConnections()

	select {}
}
