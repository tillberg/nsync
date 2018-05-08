package nsync

import (
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tillberg/alog"
	"github.com/tillberg/stringset"
	"github.com/tillberg/watcher"
)

var pathUpdateRequests = make(chan string)

var PathSeparator = string(os.PathSeparator)

func execParent() {
	listener := watcher.NewListener()
	listener.Path = RootPath
	listener.DebounceDuration = 100 * time.Millisecond
	listener.IgnorePart = deletePart
	listener.IgnoreSuffix = deleteSuffix
	listener.IgnoreSubstring = deleteSubstring
	listener.NotifyDirectoriesOnStartup = true
	err := listener.Start()
	if err != nil {
		alog.Printf("@(error:Error starting watcher for %s: %v)\n", RootPath, err)
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

func matches(path string, parts *stringset.StringSet, suffixes, substrings []string) bool {
	rel := relPath(path)
	if parts != nil {
		for _, part := range strings.Split(rel, PathSeparator) {
			if parts.Has(part) {
				return true
			}
		}
	}
	for _, suffix := range suffixes {
		if strings.HasSuffix(rel, suffix) {
			return true
		}
	}
	wrappedRel := "^"
	if !strings.HasPrefix(rel, "/") {
		wrappedRel += PathSeparator
	}
	wrappedRel += rel
	// XXX This should really only be added for directories:
	if !strings.HasSuffix(rel, "/") {
		wrappedRel += PathSeparator
	}
	wrappedRel += "$"
	for _, str := range substrings {
		if strings.Contains(wrappedRel, str) {
			return true
		}
	}
	return false
}

func matchesIgnores(path string) bool {
	return matches(path, ignorePart, ignoreSuffix, ignoreSubstring)
}

func matchesDeletes(path string) bool {
	return matches(path, deletePart, deleteSuffix, deleteSubstring)
}

func shouldIgnore(path string) bool {
	return matchesIgnores(path) || matchesDeletes(path)
}

func shouldDelete(path string) bool {
	return matchesDeletes(path) || !matchesIgnores(path)
}

func receiveFileRequestMessage(buf []byte) {
	rel, buf := decodeString(buf)
	path := getAbsPath(rel)
	if !shouldIgnore(path) {
		go func() {
			pathUpdateRequests <- path
		}()
	}
}

func sendDirUpdateMessage(path string) {
	if shouldIgnore(path) {
		return
	}
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
		if !shouldIgnore(filepath.Join(path, fileInfo.Name())) {
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
	buf = encodeFileStatus(buf, fileInfoToStatus(dirStat))
	for _, fileInfo := range fileInfos {
		buf = encodeString(buf, fileInfo.Name())
		buf = encodeFileStatus(buf, fileInfoToStatus(fileInfo))
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
	if shouldIgnore(path) {
		return
	}
	rel := relPath(path)
	if rel == "" {
		return
	}
	status := fileInfoToStatus(info)
	msgSize := encodeSizeString(rel) + encodeSizeFileStatus + int(status.Size)
	fullbuf := make([]byte, msgSize)
	buf := fullbuf
	buf = encodeString(buf, rel)
	buf = encodeFileStatus(buf, status)
	if status.Exists {
		if info.Mode().IsRegular() {
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
		} else {
			linkStr, err := os.Readlink(path)
			if err != nil {
				alog.Printf("@(error:Error reading symlink %s: %v)\n", path, err)
				return
			}
			copy(buf, linkStr)
		}
	}
	alog.Printf("Sending update for %s, %d bytes\n", path, len(fullbuf))
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
	} else {
		// it's a regular file or a symlink
		sendFileUpdateMessage(path, info)
	}
}

func readPathUpdates(fsPathUpdates <-chan watcher.PathEvent) {
	var path string
	for {
		select {
		case pathEvent := <-fsPathUpdates:
			path = pathEvent.Path
			// alog.Printf("@(dim:fs:) @(cyan:%s)\n", path)
		case path = <-pathUpdateRequests:
			if Opts.Verbose {
				alog.Printf("@(dim:req:) @(cyan:%s)\n", path)
			}
		}
		doUpdatePath(path)
	}
}
