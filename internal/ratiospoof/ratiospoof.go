package ratiospoof

import (
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ap-pauloafonso/ratio-spoof/internal/bencode"
	"github.com/ap-pauloafonso/ratio-spoof/internal/emulation"
	"github.com/ap-pauloafonso/ratio-spoof/internal/input"
	"github.com/ap-pauloafonso/ratio-spoof/internal/printer"
	"github.com/ap-pauloafonso/ratio-spoof/internal/tracker"
	"github.com/gammazero/deque"
)

const (
	maxAnnounceHistory = 10
)

type RatioSpoof struct {
	TorrentInfo      *bencode.TorrentInfo
	Input            *input.InputParsed
	Tracker          *tracker.HttpTracker
	BitTorrentClient *emulation.Emulation
	AnnounceInterval int
	NumWant          int
	Seeders          int
	Leechers         int
	AnnounceCount    int
	Status           string
	AnnounceHistory  announceHistory
}

type announceHistory struct {
	deque.Deque
}

func NewRatioSpoofState(input input.InputArgs) (*RatioSpoof, error) {
	dat, err := os.ReadFile(input.TorrentPath)
	if err != nil {
		return nil, err
	}

	client, err := emulation.NewEmulation(input.Client)
	if err != nil {
		return nil, errors.New("Error building the emulated client with the code")
	}

	torrentInfo, err := bencode.TorrentDictParse(dat)
	if err != nil {
		return nil, errors.New("failed to parse the torrent file")
	}

	httpTracker, err := tracker.NewHttpTracker(torrentInfo)
	if err != nil {
		return nil, err
	}

	inputParsed, err := input.ParseInput(torrentInfo)
	if err != nil {
		return nil, err
	}

	return &RatioSpoof{
		BitTorrentClient: client,
		TorrentInfo:      torrentInfo,
		Tracker:          httpTracker,
		Input:            inputParsed,
		NumWant:          200,
		Status:           "started",
	}, nil
}

func (a *announceHistory) pushValueHistory(value struct {
	Count             int
	Downloaded        int
	PercentDownloaded float32
	Uploaded          int
	Left              int
}) {
	if a.Len() >= maxAnnounceHistory {
		a.PopFront()
	}
	a.PushBack(value)
}

func (r *RatioSpoof) gracefullyExit() {
	fmt.Printf("\nGracefully exiting...\n")
	r.Status = "stopped"
	r.NumWant = 0
	r.fireAnnounce(false)
	fmt.Printf("Gracefully exited successfully.\n")

}

func (r *RatioSpoof) Run() {
	rand.Seed(time.Now().UnixNano())
	sigCh := make(chan os.Signal)

	signal.Notify(sigCh, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	r.firstAnnounce()

	p := printer.NewPrinter(&r.AnnounceCount, &r.Seeders, &r.Leechers, &r.Tracker.RetryAttempt, &r.Input.DownloadSpeed, &r.Input.UploadSpeed, &r.Input.Port, &r.TorrentInfo.TotalSize, &r.TorrentInfo.Name, &r.TorrentInfo.TrackerInfo.Main, &r.BitTorrentClient.Name, &r.Tracker.LastAnounceRequest, &r.Tracker.LastTackerResponse, &r.Input.Debug, &r.Tracker.EstimatedTimeToAnnounce, &r.AnnounceHistory.Deque)
	p.Start()
	go func() {
		for {
			r.generateNextAnnounce()
			time.Sleep(time.Duration(r.AnnounceInterval) * time.Second)
			r.fireAnnounce(true)
		}
	}()
	<-sigCh
	p.Stop()
	r.gracefullyExit()
}
func (r *RatioSpoof) firstAnnounce() {
	r.addAnnounce(r.Input.InitialDownloaded, r.Input.InitialUploaded, calculateBytesLeft(r.Input.InitialDownloaded, r.TorrentInfo.TotalSize), (float32(r.Input.InitialDownloaded)/float32(r.TorrentInfo.TotalSize))*100)
	r.fireAnnounce(false)
}

func (r *RatioSpoof) updateSeedersAndLeechers(resp tracker.TrackerResponse) {
	r.Seeders = resp.Seeders
	r.Leechers = resp.Leechers
}
func (r *RatioSpoof) addAnnounce(currentDownloaded, currentUploaded, currentLeft int, percentDownloaded float32) {
	r.AnnounceCount++
	r.AnnounceHistory.pushValueHistory(struct {
		Count             int
		Downloaded        int
		PercentDownloaded float32
		Uploaded          int
		Left              int
	}{Count: r.AnnounceCount,
		Downloaded:        currentDownloaded,
		Uploaded:          currentUploaded,
		Left:              currentLeft,
		PercentDownloaded: percentDownloaded})
}
func (r *RatioSpoof) fireAnnounce(retry bool) error {
	lastAnnounce := r.AnnounceHistory.Back().(struct {
		Count             int
		Downloaded        int
		PercentDownloaded float32
		Uploaded          int
		Left              int
	})
	replacer := strings.NewReplacer("{infohash}", r.TorrentInfo.InfoHashURLEncoded,
		"{port}", fmt.Sprint(r.Input.Port),
		"{peerid}", r.BitTorrentClient.PeerId(),
		"{uploaded}", fmt.Sprint(lastAnnounce.Uploaded),
		"{downloaded}", fmt.Sprint(lastAnnounce.Downloaded),
		"{left}", fmt.Sprint(lastAnnounce.Left),
		"{key}", r.BitTorrentClient.Key(),
		"{event}", r.Status,
		"{numwant}", fmt.Sprint(r.NumWant))
	query := replacer.Replace(r.BitTorrentClient.Query)
	trackerResp, err := r.Tracker.Announce(query, r.BitTorrentClient.Headers, retry)
	if err != nil {
		log.Fatalf("failed to reach the tracker:\n%s ", err.Error())
	}

	if trackerResp != nil {
		r.updateSeedersAndLeechers(*trackerResp)
		r.AnnounceInterval = trackerResp.Interval
	}
	return nil
}
func (r *RatioSpoof) generateNextAnnounce() {
	lastAnnounce := r.AnnounceHistory.Back().(struct {
		Count             int
		Downloaded        int
		PercentDownloaded float32
		Uploaded          int
		Left              int
	})
	currentDownloaded := lastAnnounce.Downloaded
	var downloadCandidate int

	if currentDownloaded < r.TorrentInfo.TotalSize {
		downloadCandidate = calculateNextTotalSizeByte(r.Input.DownloadSpeed, currentDownloaded, r.TorrentInfo.PieceSize, r.AnnounceInterval, r.TorrentInfo.TotalSize)
	} else {
		downloadCandidate = r.TorrentInfo.TotalSize
	}

	currentUploaded := lastAnnounce.Uploaded
	uploadCandidate := calculateNextTotalSizeByte(r.Input.UploadSpeed, currentUploaded, r.TorrentInfo.PieceSize, r.AnnounceInterval, 0)

	leftCandidate := calculateBytesLeft(downloadCandidate, r.TorrentInfo.TotalSize)

	d, u, l := r.BitTorrentClient.Round(downloadCandidate, uploadCandidate, leftCandidate, r.TorrentInfo.PieceSize)

	r.addAnnounce(d, u, l, (float32(d)/float32(r.TorrentInfo.TotalSize))*100)
}

func calculateNextTotalSizeByte(speedBytePerSecond, currentByte, pieceSizeByte, seconds, limitTotalBytes int) int {
	if speedBytePerSecond == 0 {
		return currentByte
	}
	totalCandidate := currentByte + (speedBytePerSecond * seconds)
	randomPieces := rand.Intn(10-1) + 1
	totalCandidate = totalCandidate + (pieceSizeByte * randomPieces)

	if limitTotalBytes != 0 && totalCandidate > limitTotalBytes {
		return limitTotalBytes
	}
	return totalCandidate
}

func calculateBytesLeft(currentBytes, totalBytes int) int {
	return totalBytes - currentBytes
}
