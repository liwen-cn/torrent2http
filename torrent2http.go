package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"runtime"
	"syscall"
	"time"

	"github.com/steeve/libtorrent-go"
)

type FileStatusInfo struct {
	Name        string  `json:"name"`
	Size        int64   `json:"size"`
	Offset      int64   `json:"offset"`
	TotalPieces int     `json:"total_pieces"`
	Buffer      float64 `json:"buffer"`
}

type LsInfo struct {
	Files []FileStatusInfo `json:"files"`
}

type SessionStatus struct {
	Name         string  `json:"name"`
	State        int     `json:"state"`
	Progress     float32 `json:"progress"`
	DownloadRate float32 `json:"download_rate"`
	UploadRate   float32 `json:"upload_rate"`
	NumPeers     int     `json:"num_peers"`
	NumSeeds     int     `json:"num_seeds"`
	TotalSeeds   int     `json:"total_seeds"`
	TotalPeers   int     `json:"total_peers"`
}

type Config struct {
	uri             string
	bindAddress     string
	maxUploadRate   int
	maxDownloadRate int
	downloadPath    string
	keepFiles       bool
	encryption      int
	noSparseFile    bool
	idleTimeout     int
	portLower       int
	portUpper       int
	buffer          float64
}

type Instance struct {
	config        Config
	session       libtorrent.Session
	torrentHandle libtorrent.Torrent_handle
	torrentFS     *TorrentFS
}

var instance = Instance{}
var mainFuncChan = make(chan func())

func runInMainThread(f interface{}) interface{} {
	done := make(chan interface{}, 1)
	mainFuncChan <- func() {
		switch f := f.(type) {
		case func():
			f()
			done <- true
		case func() interface{}:
			done <- f()
		}
	}
	return <-done
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var status SessionStatus
	if instance.torrentHandle == nil {
		status = SessionStatus{State: -1}
	} else {
		tstatus := instance.torrentHandle.Status()
		status = SessionStatus{
			Name:         instance.torrentHandle.Name(),
			State:        int(tstatus.GetState()),
			Progress:     tstatus.GetProgress(),
			DownloadRate: float32(tstatus.GetDownload_rate()) / 1000,
			UploadRate:   float32(tstatus.GetUpload_rate()) / 1000,
			NumPeers:     tstatus.GetNum_peers(),
			TotalPeers:   tstatus.GetNum_incomplete(),
			NumSeeds:     tstatus.GetNum_seeds(),
			TotalSeeds:   tstatus.GetNum_complete()}
	}

	output, _ := json.Marshal(status)
	w.Write(output)
}

func lsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	dir, _ := instance.torrentFS.TFSOpen("/")
	files, _ := dir.TFSReaddir(-1)
	retFiles := LsInfo{}

	for _, file := range files {
		startPiece, endPiece := file.Pieces()

		pieces := int(math.Ceil(instance.config.buffer * float64(endPiece-startPiece)))
		if pieces < 1 {
			pieces = 1
		}
		buffer := 0.0
		for piece := 0; piece < pieces; piece++ {
			buffer += float64(libtorrent.Get_piece_progress(instance.torrentHandle, piece))
		}
		buffer = buffer / float64(pieces)

		fi := FileStatusInfo{
			Name:        file.Name(),
			Size:        file.Size(),
			Offset:      file.Offset(),
			TotalPieces: int(math.Max(float64(endPiece-startPiece), 1)),
			Buffer:      buffer,
		}
		retFiles.Files = append(retFiles.Files, fi)
	}

	output, _ := json.Marshal(retFiles)
	w.Write(output)
}

func startServices() {
	log.Println("Starting DHT...")
	instance.session.Start_dht()

	log.Println("Starting LSD...")
	instance.session.Start_lsd()

	log.Println("Starting UPNP...")
	instance.session.Start_upnp()

	log.Println("Starting NATPMP...")
	instance.session.Start_natpmp()
}

func stopServices() {
	log.Println("Stopping DHT...")
	instance.session.Stop_dht()

	log.Println("Stopping LSD...")
	instance.session.Stop_lsd()

	log.Println("Stopping UPNP...")
	instance.session.Stop_upnp()

	log.Println("Stopping NATPMP...")
	instance.session.Stop_natpmp()
}

func removeFiles() {
	if instance.torrentHandle.Status().GetHas_metadata() == false {
		return
	}

	torrentInfo := instance.torrentHandle.Get_torrent_info()
	for i := 0; i < torrentInfo.Num_files(); i++ {
		os.RemoveAll(path.Join(instance.torrentHandle.Save_path(), torrentInfo.File_at(i).GetPath()))
	}
}

func shutdown() {
	log.Println("Stopping torrent2http...")

	stopServices()

	log.Println("Removing torrent...")

	if instance.config.keepFiles == false {
		instance.session.Set_alert_mask(libtorrent.AlertStorage_notification)
		instance.session.Remove_torrent(instance.torrentHandle, 1)
		log.Println("Waiting for files to be removed...")
		for {
			if instance.session.Wait_for_alert(libtorrent.Seconds(30)).Swigcptr() == 0 {
				break
			}
			if instance.session.Pop_alert2().What() == "cache_flushed_alert" {
				break
			}
		}
		// Just in case
		removeFiles()
	}

	log.Println("Bye bye")
	os.Exit(0)
}

func parseFlags() {
	config := Config{}
	flag.StringVar(&config.uri, "uri", "", "Magnet URI or .torrent file URL")
	flag.StringVar(&config.bindAddress, "bind", ":5001", "Bind address of torrent2http")
	flag.IntVar(&config.maxDownloadRate, "dlrate", 0, "Max Download Rate")
	flag.IntVar(&config.maxUploadRate, "ulrate", 0, "Max Upload Rate")
	flag.StringVar(&config.downloadPath, "dlpath", ".", "Download path")
	flag.BoolVar(&config.keepFiles, "keep", false, "Keep files after exiting")
	flag.BoolVar(&config.noSparseFile, "no-sparse", false, "Do not use sparse file allocation.")
	flag.IntVar(&config.encryption, "encryption", 1, "Encryption: 0=forced 1=enabled (default) 2=disabled")
	flag.IntVar(&config.idleTimeout, "max-idle", -1, "Automatically shutdown if no connection are active after a timeout.")
	flag.IntVar(&config.portLower, "port-lower", 6900, "Lower bound for listen port.")
	flag.IntVar(&config.portUpper, "port-upper", 6999, "Upper bound for listen port.")
	flag.Float64Var(&config.buffer, "buffer", 0.01, "Buffer percentage from start of file.")
	flag.Parse()

	if config.uri == "" {
		flag.Usage()
		os.Exit(1)
	}

	instance.config = config
}

func configureSession() {
	settings := instance.session.Settings()

	log.Println("Setting Session settings...")

	settings.SetUser_agent("")

	settings.SetRequest_timeout(5)
	settings.SetPeer_connect_timeout(2)
	settings.SetAnnounce_to_all_trackers(true)
	settings.SetAnnounce_to_all_tiers(true)
	settings.SetConnection_speed(100)
	if instance.config.maxDownloadRate > 0 {
		settings.SetDownload_rate_limit(instance.config.maxDownloadRate * 1024)
	}
	if instance.config.maxUploadRate > 0 {
		settings.SetUpload_rate_limit(instance.config.maxUploadRate * 1024)
	}

	settings.SetTorrent_connect_boost(100)
	settings.SetRate_limit_ip_overhead(true)

	instance.session.Set_settings(settings)

	log.Println("Setting Encryption settings...")
	encryptionSettings := libtorrent.NewPe_settings()
	encryptionSettings.SetOut_enc_policy(libtorrent.LibtorrentPe_settingsEnc_policy(instance.config.encryption))
	encryptionSettings.SetIn_enc_policy(libtorrent.LibtorrentPe_settingsEnc_policy(instance.config.encryption))
	encryptionSettings.SetAllowed_enc_level(libtorrent.Pe_settingsBoth)
	encryptionSettings.SetPrefer_rc4(true)
	instance.session.Set_pe_settings(encryptionSettings)
}

func NewConnectionCounterHandler(connTrackChannel chan int, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connTrackChannel <- 1
		handler.ServeHTTP(w, r)
		connTrackChannel <- -1
	})
}

func inactiveAutoShutdown(connTrackChannel chan int) {
	activeConnections := 0

	for {
		if activeConnections == 0 {
			select {
			case inc := <-connTrackChannel:
				activeConnections += inc
			case <-time.After(time.Duration(instance.config.idleTimeout) * time.Second):
				go shutdown()
			}
		} else {
			activeConnections += <-connTrackChannel
		}
	}
}

func startHTTP() {
	log.Println("Starting HTTP Server...")

	mux := http.NewServeMux()
	mux.HandleFunc("/status", statusHandler)
	mux.HandleFunc("/ls", lsHandler)
	mux.Handle("/files/", http.StripPrefix("/files/", http.FileServer(instance.torrentFS)))
	mux.Handle("/shutdown", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		go shutdown()
		fmt.Fprintf(w, "OK")
	}))

	handler := http.Handler(mux)
	if instance.config.idleTimeout > 0 {
		connTrackChannel := make(chan int, 10)
		handler = NewConnectionCounterHandler(connTrackChannel, mux)
		go inactiveAutoShutdown(connTrackChannel)
	}

	log.Printf("Listening HTTP on %s...\n", instance.config.bindAddress)
	http.ListenAndServe(instance.config.bindAddress, handler)
}

func watchParent() {
	for {
		// did the parent die? shutdown!
		if os.Getppid() == 1 {
			go shutdown()
			break
		}
		time.Sleep(2 * time.Second)
	}
}

// Handle SIGTERM (Ctrl-C)
func handleSignals() {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)
	<-signalChan
	go shutdown()
}

func ensureSeeding() {
	log.Println("Starting seeding watcher")
	for {
		tstatus := instance.torrentHandle.Status()
		if tstatus.GetIs_seeding() || tstatus.GetIs_finished() {
			break
		}
		time.Sleep(1 * time.Second)
	}
	log.Println("Now seeding, setting priorities")
	numPieces := instance.torrentFS.ti.Num_pieces()
	for i := 0; i < numPieces; i++ {
		instance.torrentHandle.Piece_priority(i, 1)
	}
}

func main() {
	// Make sure we are properly multithreaded, on a minimum of 2 threads
	// because we lock the main thread for libtorrent.
	runtime.GOMAXPROCS(runtime.NumCPU())

	parseFlags()

	torrentParams := libtorrent.NewAdd_torrent_params()

	fileUri, err := url.Parse(instance.config.uri)
	if err != nil {
		log.Fatal(err)
	}
	if fileUri.Scheme == "file" {
		log.Printf("Opening local file %s\n", fileUri.Path)
		torrentInfo := libtorrent.NewTorrent_info(fileUri.Path)
		torrentParams.SetTi(torrentInfo)
	} else {
		log.Println("Fetching link")
		torrentParams.SetUrl(instance.config.uri)
	}

	log.Println("Setting save path")
	torrentParams.SetSave_path(instance.config.downloadPath)

	if instance.config.noSparseFile {
		log.Println("Disabling sparse file support...")
		torrentParams.SetStorage_mode(libtorrent.Storage_mode_allocate)
	}

	log.Println("Starting BT engine...")
	instance.session = libtorrent.NewSession()
	instance.session.Listen_on(libtorrent.NewPair_int_int(instance.config.portLower, instance.config.portUpper))

	configureSession()
	startServices()

	log.Println("Adding torrent")
	instance.torrentHandle = instance.session.Add_torrent(torrentParams)

	log.Println("Enabling sequential download")
	instance.torrentHandle.Set_sequential_download(true)

	log.Printf("Downloading: %s\n", instance.torrentHandle.Name())

	instance.torrentFS = NewTorrentFS(instance.torrentHandle)

	// go func() {
	// 	for {
	// 		log.Println(libtorrent.Get_piece_progress(instance.torrentHandle, 1))
	// 		time.Sleep(1 * time.Second)
	// 	}
	// }()

	go handleSignals()
	go watchParent()

	startHTTP()
}
