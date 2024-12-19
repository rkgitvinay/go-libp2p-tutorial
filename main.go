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

// Handle incoming file requests
func handleFileRequest(s network.Stream) {
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

	// Check if we have the file
	filePath := filepath.Join(filesDir, fileReq.FileName)
	fileInfo, err := os.Stat(filePath)

	response := FileResponse{}
	if err != nil {
		response.Success = false
		response.Message = "File not found"
	} else {
		response.Success = true
		response.Message = "File found"
		response.Size = fileInfo.Size()
	}

	// Send response
	respData, _ := json.Marshal(response)
	rw.WriteString(fmt.Sprintf("%s\n", respData))
	rw.Flush()

	s.Close()
}

// Handle actual file transfer
func handleFileTransfer(s network.Stream) {
	fileName := filepath.Base(s.Conn().RemoteMultiaddr().String())
	filePath := filepath.Join(filesDir, fileName)

	file, err := os.Open(filePath)
	if err != nil {
		log.Printf("Error opening file: %v", err)
		s.Close()
		return
	}
	defer file.Close()

	// Send the file
	_, err = io.Copy(s, file)
	if err != nil {
		log.Printf("Error sending file: %v", err)
	}

	s.Close()
}

// Function to download a file from a peer
func downloadFile(peerID string, fileName string) error {

	// First check if trying to download from self
	if peerID == host.ID().String() {
		// If it's a local file, just copy it to downloads directory
		sourcePath := filepath.Join(filesDir, fileName)
		downloadDir := "./received"
		targetPath := filepath.Join(downloadDir, fileName)

		// Check if source file exists
		if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
			return fmt.Errorf("file not found in local shared directory")
		}

		// Create downloads directory if it doesn't exist
		os.MkdirAll(downloadDir, os.ModePerm)

		// Copy the file
		source, err := os.Open(sourcePath)
		if err != nil {
			return fmt.Errorf("failed to open source file: %v", err)
		}
		defer source.Close()

		destination, err := os.Create(targetPath)
		if err != nil {
			return fmt.Errorf("failed to create destination file: %v", err)
		}
		defer destination.Close()

		_, err = io.Copy(destination, source)
		if err != nil {
			return fmt.Errorf("failed to copy file: %v", err)
		}

		return nil
	}

	// Find peer info
	peerIDObj, err := peer.Decode(peerID)
	if err != nil {
		return fmt.Errorf("invalid peer ID: %v", err)
	}

	// Open stream for file request
	ctx := context.Background()
	requestStream, err := host.NewStream(ctx, peerIDObj, "/filerequest/1.0.0")
	if err != nil {
		return fmt.Errorf("failed to open request stream: %v", err)
	}

	// Send file request
	rw := bufio.NewReadWriter(bufio.NewReader(requestStream), bufio.NewWriter(requestStream))
	request := FileRequest{FileName: fileName}
	reqData, _ := json.Marshal(request)
	rw.WriteString(fmt.Sprintf("%s\n", reqData))
	rw.Flush()

	// Read response
	respData, err := rw.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read response: %v", err)
	}

	var response FileResponse
	if err := json.Unmarshal([]byte(respData), &response); err != nil {
		return fmt.Errorf("failed to parse response: %v", err)
	}

	if !response.Success {
		return fmt.Errorf("peer response: %s", response.Message)
	}

	requestStream.Close()

	// Open stream for file transfer
	transferStream, err := host.NewStream(ctx, peerIDObj, "/filetransfer/1.0.0")
	if err != nil {
		return fmt.Errorf("failed to open transfer stream: %v", err)
	}
	defer transferStream.Close()

	// Create download directory if it doesn't exist
	downloadDir := "./received"
	os.MkdirAll(downloadDir, os.ModePerm)

	// Create the file
	filePath := filepath.Join(downloadDir, fileName)
	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create file: %v", err)
	}
	defer file.Close()

	// Receive the file
	_, err = io.Copy(file, transferStream)
	if err != nil {
		return fmt.Errorf("failed to save file: %v", err)
	}

	return nil
}

// Add new HTTP handler for initiating downloads
func handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST is supported", http.StatusMethodNotAllowed)
		return
	}

	var downloadRequest struct {
		PeerID   string `json:"peer_id"`
		FileName string `json:"file_name"`
	}

	if err := json.NewDecoder(r.Body).Decode(&downloadRequest); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	err := downloadFile(downloadRequest.PeerID, downloadRequest.FileName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, "File downloaded successfully")
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

	// Add these new protocol handlers
	host.SetStreamHandler("/filerequest/1.0.0", handleFileRequest)
	host.SetStreamHandler("/filetransfer/1.0.0", handleFileTransfer)

	// Connect to destination peer if provided
	if *dest != "" {
		err := connectToPeer(*dest)
		if err != nil {
			log.Fatalf("Failed to connect to peer: %v", err)
		}
	}

	select {}
}
