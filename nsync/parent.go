package nsync

import (
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tillberg/ansi-log"
	"github.com/tillberg/stringset"
	"github.com/tillberg/watcher"
)

var pathUpdateRequests = make(chan string)

var IGNORED = stringset.New(".gut", "pkg", "testdata")
var IGNORED_SUFFIX = stringset.New(".tmp", ".lock")
var EXTRA_IGNORED = []string{"/go/bin", "/go/tmp"}

func execParent() {
	listener := watcher.NewListener()
	listener.Path = RootPath
	listener.DebounceDuration = 100 * time.Millisecond
	listener.Ignored = IGNORED
	listener.NotifyDirectoriesOnStartup = true
	err := listener.Start()
	if err != nil {
		alog.Printf("@(error:Error starting watcher: %v)\n", err)
	}
	go readPathUpdates(listener.NotifyChan)
	go handleParentMessages()
	go sendKeepAlives()
}

func sendKeepAlives() {
	for {
		MessagesToChild <- Message{
			Op: OpKeepAlive,
		}
		time.Sleep(keepAliveInterval)
	}
}

func handleParentMessages() {
	for message := range MessagesToParent {
		if Opts.Verbose {
			alog.Printf("@(dim:Message received: op) @(cyan:%s)\n", message.Op)
		}
		switch message.Op {
		case OpFileRequest:
			receiveFileRequestMessage(message.Buf)
		default:
			alog.Printf("@(error:Unknown op %s)\b", message.Op)
		}
	}
}

func isIgnored(path string) bool {
	if IGNORED.Has(filepath.Base(path)) {
		return true
	}
	if IGNORED_SUFFIX.Has(filepath.Ext(path)) {
		return true
	}
	for _, str := range EXTRA_IGNORED {
		if strings.Contains(path, str) {
			return true
		}
	}
	return false
}

func receiveFileRequestMessage(buf []byte) {
	rel, buf := decodeString(buf)
	path := getAbsPath(rel)
	if !isIgnored(path) {
		go func() {
			pathUpdateRequests <- path
		}()
	}
}

func sendDirUpdateMessage(path string) {
	dirStat, err := os.Lstat(path)
	if err != nil {
		alog.Printf("@(error:Unable to lstat directory %s: %v)\n", path, err)
		return
	}
	_fileInfos, err := ioutil.ReadDir(path)
	if err != nil {
		alog.Printf("@(error:Unable to list directory %s: %v)\n", path, err)
		return
	}
	fileInfos := []os.FileInfo{}
	for _, fileInfo := range _fileInfos {
		if !isIgnored(filepath.Join(path, fileInfo.Name())) {
			fileInfos = append(fileInfos, fileInfo)
		}
	}
	rel := relPath(path)
	if rel == "" {
		return
	}
	msgSize := 0
	msgSize += encodeSizeString(rel)
	msgSize += encodeSizeInt
	msgSize += encodeSizeFileStatus
	for _, fileInfo := range fileInfos {
		msgSize += encodeSizeString(fileInfo.Name())
		msgSize += encodeSizeFileStatus
	}
	fullbuf := make([]byte, msgSize)
	buf := fullbuf
	buf = encodeString(buf, rel)
	buf = encodeInt(buf, int64(len(fileInfos)))
	buf = encodeFileStatus(buf, FileStatus{
		ModTime: dirStat.ModTime(),
		Mode:    dirStat.Mode(),
	})
	for _, fileInfo := range fileInfos {
		buf = encodeString(buf, fileInfo.Name())
		buf = encodeFileStatus(buf, FileStatus{
			Size:    fileInfo.Size(),
			ModTime: fileInfo.ModTime(),
			Mode:    fileInfo.Mode(),
		})
	}
	if len(buf) != 0 {
		alog.Println("Mis-allocated buffer in sendDirUpdateMessage, bytes remaining:", len(buf))
	}
	MessagesToChild <- Message{
		Op:  OpDirUpdate,
		Buf: fullbuf,
	}
}

func sendFileUpdateMessage(path string, info os.FileInfo) {
	if isIgnored(path) {
		return
	}
	rel := relPath(path)
	if rel == "" {
		return
	}
	var status FileStatus
	if info != nil {
		status.Size = info.Size()
		status.ModTime = info.ModTime()
		status.Mode = info.Mode()
		status.Exists = true
	} else {
		status.Size = 0
		status.Exists = false
	}
	msgSize := encodeSizeString(rel) + encodeSizeFileStatus + int(status.Size)
	fullbuf := make([]byte, msgSize)
	buf := fullbuf
	buf = encodeString(buf, rel)
	buf = encodeFileStatus(buf, status)
	if status.Exists {
		file, err := os.Open(path)
		if err != nil {
			alog.Printf("@(error:Error opening file %s: %v)\n", path, err)
			return
		}
		defer file.Close()
		_, err = io.ReadFull(file, buf)
		if err != nil {
			alog.Printf("@(error:Error reading file %s: %v)\n", path, err)
			return
		}
	}
	// The file might have grown, and we might not be at EOF. But we'll just ignore
	// that here and let the next FileUpdateMessage take care of it.
	MessagesToChild <- Message{
		Op:  OpFileUpdate,
		Buf: fullbuf,
	}
}

func doUpdatePath(path string) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			sendFileUpdateMessage(path, nil)
			return
		} else {
			alog.Printf("@(error:Error lstat-ing %s: %v)\n", path, err)
			return
		}
	}
	if info.IsDir() {
		sendDirUpdateMessage(path)
	} else if info.Mode()&os.ModeSymlink == 0 {
		// it's a regular file... probably?
		sendFileUpdateMessage(path, info)
	}
}

func readPathUpdates(fsPathUpdates <-chan string) {
	var path string
	for {
		select {
		case path = <-fsPathUpdates:
			// alog.Printf("@(dim:fs:) @(cyan:%s)\n", path)
		case path = <-pathUpdateRequests:
			alog.Printf("@(dim:req:) @(cyan:%s)\n", path)
		}
		doUpdatePath(path)
	}
}
