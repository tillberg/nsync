package nsync

import (
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tillberg/alog"
	"github.com/tillberg/notifywrap"
	"github.com/tillberg/stringset"
)

var pathUpdateRequests = make(chan string)

var PathSeparator = string(os.PathSeparator)

func execParent() {
	watcherOpts := notifywrap.Opts{
		DebounceDuration:           200 * time.Millisecond,
		CoalesceEventTypes:         true,
		NotifyDirectoriesOnStartup: true,
	}
	pathEvents := make(chan *notifywrap.EventInfo, 100)
	if len(Opts.IncludePath) == 0 {
		err := notifywrap.WatchRecursive(Opts.Positional.LocalPath, pathEvents, watcherOpts)
		alog.BailIf(err)
	} else {
		for _, includePath := range Opts.IncludePath {
			watchRoot := filepath.Join(Opts.Positional.LocalPath, includePath)
			err := notifywrap.WatchRecursive(watchRoot, pathEvents, watcherOpts)
			alog.BailIf(err)
		}
	}
	go readPathUpdates(pathEvents)
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
			alog.Printf("@(error:Unknown op %s)\n", message.Op)
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

func matchesIncludes(path string, isDir bool) bool {
	relPath, err := filepath.Rel(Opts.Positional.LocalPath, path)
	alog.BailIf(err)
	if relPath == "." {
		relPath = "/"
	} else {
		relPath = "/" + relPath
	}
	if isDir && includeDirs.Has(relPath) {
		if Opts.Verbose {
			alog.Printf("@(dim:Dir %q is not included.)\n", path)
		}
		return true
	}
	var relPathIter = relPath
	for relPathIter != "." && relPathIter != "/" {
		if includePath.Has(relPathIter) {
			return true
		}
		relPathIter = filepath.Dir(relPathIter)
	}
	if Opts.Verbose {
		alog.Printf("@(dim:Path %q [dir=%t] is not included.)\n", relPath, isDir)
	}
	return false
}

func matchesIgnores(path string) bool {
	return matches(path, ignorePart, ignoreSuffix, ignoreSubstring)
}

func shouldIgnore(path string, isDir bool) bool {
	if includePath.Len() != 0 && !matchesIncludes(path, isDir) {
		return true
	}
	return matchesIgnores(path)
}

func receiveFileRequestMessage(buf []byte) {
	rel, buf := decodeString(buf)
	path := getAbsPath(rel)
	if !shouldIgnore(path, true) { // We pass "true" because it *could* be a dir
		go func() {
			pathUpdateRequests <- path
		}()
	} else if Opts.Verbose {
		alog.Printf("@(dim:Ignoring request for %q)\n", rel)
	}
}

func sendDirUpdateMessage(path string) {
	if shouldIgnore(path, true) {
		if Opts.Verbose {
			alog.Printf("@(dim:Ignoring dir update for %q)\n", path)
		}
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
		if !shouldIgnore(filepath.Join(path, fileInfo.Name()), fileInfo.IsDir()) {
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
	if Opts.Verbose {
		alog.Printf("@(dim:Sending path update for %q to child.)\n", path)
	}
	MessagesToChild <- Message{
		Op:  OpDirUpdate,
		Buf: fullbuf,
	}
}

func sendFileUpdateMessage(path string, info os.FileInfo) {
	if shouldIgnore(path, false) {
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

func readPathUpdates(pathEvents <-chan *notifywrap.EventInfo) {
	var path string
	for {
		select {
		case pathEvent := <-pathEvents:
			path = pathEvent.Path
			if Opts.Verbose {
				alog.Printf("@(dim:fs event:) @(green:%s) @(cyan:%s)\n", pathEvent.Event, path)
			}
		case path = <-pathUpdateRequests:
			if Opts.Verbose {
				alog.Printf("@(dim:req:) @(cyan:%s)\n", path)
			}
		}
		doUpdatePath(path)
	}
}
