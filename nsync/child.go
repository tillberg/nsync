package nsync

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/tillberg/ansi-log"
)

// XXX it would be better to look up the actual umask:
const MODE_MASK = 0777 ^ 022

func execChild() {
	alog.SetPrefix("")
	alog.Printf("@(dim:nsync child started, writing to) @(cyan:%s)\n", RootPath)

	go sendMessages(os.Stdout, MessagesToParent, make(chan error))
	go receiveMessages(os.Stdin, MessagesToChild, make(chan error))

	handleChildMessages()
}

func handleChildMessages() {
	for {
		select {
		case <-time.After(3 * keepAliveInterval):
			alog.Printf("Timed out after not receiving keepalive. Exiting.\n")
			return
		case message := <-MessagesToChild:
			if Opts.Verbose {
				alog.Printf("@(dim:Message received: op) @(cyan:%s)\n", message.Op)
			}
			switch message.Op {
			case OpDirUpdate:
				receiveDirUpdateMessage(message.Buf)
			case OpFileUpdate:
				receiveFileUpdateMessage(message.Buf)
			case OpKeepAlive:
			default:
				alog.Printf("@(error:Unknown op %s)\n", message.Op)
			}
		}
	}
}

func sendFileRequestMessage(path string) {
	rel := relPath(path)
	if rel == "" {
		return
	}
	msgSize := encodeSizeString(rel)
	fullbuf := make([]byte, msgSize)
	buf := fullbuf
	buf = encodeString(buf, rel)
	if len(buf) != 0 {
		alog.Println("Mis-allocated buffer in sendFileRequestMessage, bytes remaining:", len(buf))
	}
	alog.Printf("@(dim:Requesting update for) @(cyan:%s)\n", rel)
	MessagesToParent <- Message{
		Op:  OpFileRequest,
		Buf: fullbuf,
	}
}

func writeModTime(path string, modTime time.Time) bool {
	err := os.Chtimes(path, modTime, modTime)
	if err != nil {
		alog.Printf("@(error:Error writing access/mod times to %s: %v)\n", path, err)
		return false
	}
	return true
}

func ensureIsDirectory(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			alog.Printf("@(dim:Creating directory) @(cyan:%s)\n", path)
			err := os.MkdirAll(path, 0700)
			if err != nil {
				alog.Printf("@(error:Error creating directory %s: %v)\n", path, err)
				return false
			}
		} else {
			alog.Printf("@(error:Error lstat-ing %s: %v)\n", path, err)
			return false
		}
	} else if !info.IsDir() {
		alog.Printf("@(warn:Deleting file to make way for directory at) @(cyan:%s)\n", path)
		err := os.RemoveAll(path)
		if err != nil {
			alog.Printf("@(error:Error deleting %s to make way for directory in its place: %v)\n", path, err)
			return false
		}
		err = os.MkdirAll(path, 0700)
		if err != nil {
			alog.Printf("@(error:Error creating directory %s: %v)\n", path, err)
			return false
		}
	}
	return true
}

func receiveDirUpdateMessage(buf []byte) {
	srcFiles := map[string]FileStatus{}
	rel, buf := decodeString(buf)
	numFiles, buf := decodeInt(buf)
	dirStatus, buf := decodeFileStatus(buf)
	for i := 0; i < int(numFiles); i++ {
		var name string
		var fileStatus FileStatus
		name, buf = decodeString(buf)
		fileStatus, buf = decodeFileStatus(buf)
		srcFiles[name] = fileStatus
	}
	path := getAbsPath(rel)
	parentModTimes := preserveParentModTimes(rel)
	if !ensureIsDirectory(path) {
		return
	}
	fileInfos, err := ioutil.ReadDir(path)
	if err != nil {
		alog.Printf("@(error:Unable to list directory %s: %v)\n", path, err)
		return
	}
	for _, fileInfo := range fileInfos {
		name := fileInfo.Name()
		subpath := filepath.Join(path, name)
		srcFile, srcExists := srcFiles[name]
		destStatus := fileInfoToStatus(fileInfo)
		if srcExists {
			isSymlink := fileInfo.Mode()&os.ModeSymlink != 0
			isDir := fileInfo.IsDir()
			if !isSymlink && !isDir && (destStatus.ModTime.Unix() != srcFile.ModTime.Unix()) {
				alog.Printf("@(dim:Need update for %s, source newer)\n", subpath)
			} else if !isDir && (destStatus.Size != srcFile.Size) {
				alog.Printf("@(dim:Need update for %s, source diff size)\n", subpath)
			} else if MODE_MASK&destStatus.Mode&os.ModePerm != MODE_MASK&srcFile.Mode&os.ModePerm {
				alog.Printf("@(dim:Need update for %s, source diff permissions)\n", subpath)
			} else if destStatus.Mode&os.ModeSymlink != srcFile.Mode&os.ModeSymlink {
				alog.Printf("@(dim:Need update for %s, symlink/regular file mismatch)\n", subpath)
			} else if !isSymlink && destStatus.Uid != NoOwnerInfo && srcFile.Uid != NoOwnerInfo && (destStatus.Uid != srcFile.Uid) {
				alog.Printf("@(dim:Need update for %s, uid mismatch)\n", subpath)
			} else if !isSymlink && destStatus.Gid != NoOwnerInfo && srcFile.Gid != NoOwnerInfo && (destStatus.Gid != srcFile.Gid) {
				alog.Printf("@(dim:Need update for %s, gid mismatch)\n", subpath)
			} else {
				delete(srcFiles, name)
			}
		} else if shouldDelete(subpath) {
			alog.Printf("@(dim:Deleting) @(cyan:%s)\n", subpath)
			err := os.RemoveAll(subpath)
			if err != nil {
				alog.Printf("@(error:Error deleting %s: %v)\n", subpath, err)
			}
		}
	}
	if !writeModTime(path, dirStatus.ModTime) {
		return
	}
	if dirStatus.Uid != NoOwnerInfo && dirStatus.Gid != NoOwnerInfo {
		err := os.Chown(path, int(dirStatus.Uid), int(dirStatus.Gid))
		if err != nil {
			alog.Printf("@(error:Error chowning %s to %d/%d: %v)\n", path, dirStatus.Uid, dirStatus.Gid, err)
			return
		}
	}
	err = os.Chmod(path, dirStatus.Mode&os.ModePerm)
	if err != nil {
		alog.Printf("@(error:Error chmod-ing directory %s: %v)\n", path, err)
		return
	}
	restoreParentModTimes(rel, parentModTimes)
	if Opts.Verbose {
		alog.Printf("@(dim:Finished processing dir update for) @(cyan:%s)\n", path)
	}
	if len(srcFiles) > 0 {
		alog.Printf("@(dim:Requesting update for) @(cyan:%d) @(dim:files in) @(cyan:%s)\n", len(srcFiles), path)
		go func() {
			for name := range srcFiles {
				sendFileRequestMessage(filepath.Join(path, name))
			}
		}()
	}
}

func receiveFileUpdateMessage(buf []byte) {
	rel, buf := decodeString(buf)
	fileStatus, filebuf := decodeFileStatus(buf)
	path := getAbsPath(rel)
	parentPath := filepath.Clean(filepath.Join(path, ".."))
	if !ensureIsDirectory(parentPath) {
		return
	}
	parentModTimes := preserveParentModTimes(rel)
	if fileStatus.Exists {
		tmpPath := path + ".nsynctmp"
		objType := "file"
		if fileStatus.Mode.IsRegular() {
			file, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, fileStatus.Mode&os.ModePerm)
			if err != nil {
				alog.Printf("@(error:Error opening %s for writing: %v)\n", tmpPath, err)
				return
			}
			_, err = file.Write(filebuf)
			file.Close()
			if err != nil {
				alog.Printf("@(error:Error writing contents of %s: %v)\n", tmpPath, err)
				return
			}
			if fileStatus.Uid != NoOwnerInfo && fileStatus.Gid != NoOwnerInfo {
				err := os.Chown(tmpPath, int(fileStatus.Uid), int(fileStatus.Gid))
				if err != nil {
					alog.Printf("@(error:Error chowning %s to %d/%d: %v)\n", tmpPath, fileStatus.Uid, fileStatus.Gid, err)
					return
				}
			}
		} else {
			objType = "symlink"
			target := string(filebuf)
			err := os.Symlink(target, tmpPath)
			if err != nil {
				alog.Printf("@(error:Error writing symlink to %s from %s: %v)\n", target, tmpPath, err)
				return
			}
		}
		err := os.Rename(tmpPath, path)
		if err != nil {
			alog.Printf("@(error:Error moving %s to %s: %v)\n", tmpPath, path, err)
			return
		}
		if !writeModTime(path, fileStatus.ModTime) {
			return
		}
		alog.Printf("@(dim:Wrote %s) @(cyan:%s)@(dim:,) @(cyan:%d) @(dim:bytes.)\n", objType, path, len(filebuf))
	} else {
		err := os.RemoveAll(path)
		if err != nil {
			alog.Printf("@(error:Error deleting %s: %v)\n", path, err)
			return
		}
		alog.Printf("@(dim:Deleted) @(cyan:%s)@(dim:.)\n", path)
	}
	restoreParentModTimes(rel, parentModTimes)
}

func preserveParentModTimes(rel string) []time.Time {
	modTimes := []time.Time{}
	for rel != "." {
		rel = filepath.Clean(filepath.Join(rel, ".."))
		path := getAbsPath(rel)
		info, err := os.Lstat(path)
		if err != nil {
			alog.Printf("@(error:Error lstat-ing %s in preserveParentModTimes: %v)\n", path, err)
			modTimes = append(modTimes, time.Time{})
		} else {
			modTimes = append(modTimes, info.ModTime())
		}
	}
	return modTimes
}

func restoreParentModTimes(rel string, modTimes []time.Time) {
	paths := []string{}
	for rel != "." {
		rel = filepath.Clean(filepath.Join(rel, ".."))
		paths = append(paths, getAbsPath(rel))
	}
	if len(modTimes) < len(paths) {
		alog.Printf("@(error:modTimes too short in restoreParentModTimes for %s)\n", rel)
		return
	}
	for i := 0; i < len(paths); i++ {
		// Process paths in reverse order
		index := len(paths) - i - 1
		path := paths[index]
		modTime := modTimes[index]
		if !modTime.IsZero() {
			err := os.Chtimes(path, modTime, modTime)
			if err != nil {
				alog.Printf("@(error:Error restoring modTime for %s: %v)\n", path, err)
				return
			}
		}
	}
}
