package nsync

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/tillberg/alog"
	"github.com/tillberg/bismuth"
)

//go:generate stringer -type=OpByte

type OpByte byte

const (
	OpNone OpByte = iota
	// parent -> child
	OpDirUpdate
	OpFileUpdate
	OpKeepAlive
	// child -> parent
	OpFileRequest
)

type Message struct {
	Op  OpByte
	Buf []byte
}

var childSshProcess *os.Process
var childSshProcessMutex sync.Mutex

func killChildSshProcess() {
	childSshProcessMutex.Lock()
	defer childSshProcessMutex.Unlock()
	if childSshProcess != nil {
		done := make(chan error)
		go func() {
			alog.Printf("@(dim:Killing child ssh process...)\n")
			done <- childSshProcess.Kill()
			alog.Printf("@(dim:Child ssh process killed...)\n")
		}()
		select {
		case <-done:
			break
		case <-time.After(1 * time.Second):
			alog.Printf("@(warn:Timed out waiting to kill child ssh process)\n")
			break
		}
	}
}

var keepAliveInterval = 60 * time.Second

func receiveMessages(reader io.Reader, messages chan<- Message, done chan error) {
	// lg := alog.New(os.Stderr, "", 0)
	for {
		var op OpByte
		// lg.Replacef("")
		err := binary.Read(reader, binary.LittleEndian, &op)
		if err != nil {
			alog.Printf("@(error:Error reading message: %v)\n", err)
			done <- err
			return
		}
		// alog.Println("READ OP", op.String())
		var size uint32
		err = binary.Read(reader, binary.LittleEndian, &size)
		if err != nil {
			alog.Printf("@(error:Error reading message: %v)\n", err)
			done <- err
			return
		}
		// alog.Println("READ SIZE", size)
		var buf = make([]byte, size)
		// for {
		// lg.Replacef("Reading %d of %d bytes...", 0, size)

		// alog.Println("READING BUF", size)
		_, err = io.ReadFull(reader, buf)
		if err != nil {
			alog.Printf("@(error:Error reading message: %v)\n", err)
			done <- err
			return
		}
		// 	break
		// }
		// alog.Println("READ DONE")
		messages <- Message{
			Op:  op,
			Buf: buf,
		}
	}
}
func sendMessages(writer io.Writer, messages <-chan Message, done chan error) {
	bufWriter := bufio.NewWriter(writer)
	for message := range messages {
		// alog.Println("SENDING OP", message.Op.String())
		err := binary.Write(bufWriter, binary.LittleEndian, message.Op)
		if err != nil {
			alog.Printf("@(error:Error writing message: %v)\n", err)
			done <- err
			return
		}
		// alog.Println("SENDING SIZE", len(message.Buf))
		err = binary.Write(bufWriter, binary.LittleEndian, uint32(len(message.Buf)))
		if err != nil {
			alog.Printf("@(error:Error writing message: %v)\n", err)
			done <- err
			return
		}
		// alog.Println("SENDING BUF", len(message.Buf))
		_, err = bufWriter.Write(message.Buf)
		if err != nil {
			alog.Printf("@(error:Error writing message: %v)\n", err)
			done <- err
			return
		}
		// alog.Println("SEND DONE")
		err = bufWriter.Flush()
		if err != nil {
			alog.Printf("@(error:Error writing message: %v)\n", err)
			done <- err
			return
		}
	}
}

func connectChild(remoteHost, remoteRoot, remoteUser, nsyncPath string) {
	lg := alog.New(os.Stderr, "@(dim:{isodate} [remote]) ", 0)
	ctx := bismuth.NewExecContext()
	ctx.Connect()
	if nsyncPath == "" {
		nsyncPath = "~/.nsync/nsync"
		exePath, err := exec.LookPath(os.Args[0])
		alog.BailIf(err)
		// alog.Println("My path is", exePath)
		ctx.Quote("copy-mkdir", "ssh", remoteHost, "mkdir -p ~/.nsync")
		retCode, err := ctx.Quote("copy", "rsync", "-a", exePath, remoteHost+":~/.nsync/nsync")
		if retCode != 0 {
			alog.Println("copy retCode", retCode)
		}
		if err != nil {
			alog.Println(err)
			return
		}
	}
	sshArgs := []string{
		remoteHost,
	}
	if remoteUser != "" {
		sshArgs = append(sshArgs, []string{"sudo", "-u", remoteUser}...)
	}
	sshArgs = append(sshArgs, []string{nsyncPath, "--child", remoteRoot}...)
	cmd := exec.Command("ssh", sshArgs...)
	if Opts.Verbose {
		cmd.Args = append(cmd.Args, "--verbose")
	}
	if Opts.IgnorePart != "" {
		cmd.Args = append(cmd.Args, "--ignore-part", Opts.IgnorePart)
	}
	if Opts.IgnoreSuffix != "" {
		cmd.Args = append(cmd.Args, "--ignore-suffix", Opts.IgnoreSuffix)
	}
	if Opts.IgnoreSubstring != "" {
		cmd.Args = append(cmd.Args, "--ignore-substring", Opts.IgnoreSubstring)
	}
	if Opts.DeletePart != "" {
		cmd.Args = append(cmd.Args, "--delete-part", Opts.DeletePart)
	}
	if Opts.DeleteSuffix != "" {
		cmd.Args = append(cmd.Args, "--delete-suffix", Opts.DeleteSuffix)
	}
	if Opts.DeleteSubstring != "" {
		cmd.Args = append(cmd.Args, "--delete-substring", Opts.DeleteSubstring)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		lg.Printf("@(error:Error getting stdin pipe: %v)\n", err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		lg.Printf("@(error:Error getting stdout pipe: %v)\n", err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		lg.Printf("@(error:Error getting stderr pipe: %v)\n", err)
		return
	}
	if err := cmd.Start(); err != nil {
		lg.Printf("@(error:Error starting %s: %v)\n", cmd.Args[0], err)
		return
	}
	childSshProcessMutex.Lock()
	childSshProcess = cmd.Process
	childSshProcessMutex.Unlock()
	done := make(chan error)
	go sendMessages(stdin, MessagesToChild, done)
	go receiveMessages(stdout, MessagesToParent, done)
	go func() {
		_, err := io.Copy(lg, stderr)
		done <- err
	}()
	for i := 0; i < 2; i++ {
		err := <-done
		if err != nil && err != io.EOF {
			lg.Printf("@(error:Error reading subprocess output: %v)\n", err)

			return
		}
	}
	if err := cmd.Wait(); err != nil {
		lg.Printf("@(error:Error on subprocess exit: %s)\n", err)
	}
	childSshProcessMutex.Lock()
	childSshProcess = nil
	childSshProcessMutex.Unlock()
}

func connectChildForever(remoteHost, remoteRoot, remoteUser, nsyncPath string, onChildExit chan error) {
	connectChild(remoteHost, remoteRoot, remoteUser, nsyncPath)
	alog.Printf("@(dim:Child disconnected. Restarting...)\n")
	onChildExit <- errors.New("Child disconnected.")
}

func getAbsPath(rel string) string {
	return filepath.Join(RootPath, rel)
}

func relPath(path string) string {
	rel, err := filepath.Rel(RootPath, path)
	if err != nil {
		alog.Printf("@(Error:Unable to get relative path to %s: %v)\n", path, err)
		return ""
	}
	return rel
}

func fileInfoToStatus(fileInfo os.FileInfo) FileStatus {
	if fileInfo != nil {
		uid := NoOwnerInfo
		gid := NoOwnerInfo
		if statT, ok := fileInfo.Sys().(*syscall.Stat_t); ok {
			uid = statT.Uid
			gid = statT.Gid
			// This only happens in the parent:
			if remoteUid, ok := rewriteUidMap[uid]; ok {
				uid = remoteUid
			}
			if remoteGid, ok := rewriteGidMap[gid]; ok {
				gid = remoteGid
			}
		}
		return FileStatus{
			Size:    fileInfo.Size(),
			ModTime: fileInfo.ModTime(),
			Mode:    fileInfo.Mode(),
			Uid:     uid,
			Gid:     gid,
			Exists:  true,
		}
	} else {
		return FileStatus{
			Size:   0,
			Exists: false,
		}
	}
}

const encodeSizeInt = 8
const encodeSizeFileStatus = 5 * encodeSizeInt

var NoOwnerInfo = uint32(0x7fffffff)

type FileStatus struct {
	Size    int64
	ModTime time.Time
	Mode    os.FileMode
	Uid     uint32
	Gid     uint32
	Exists  bool
}

func encodeSizeString(v string) int {
	return encodeSizeInt + len(v)
}
func encodeInt(buf []byte, v int64) []byte {
	binary.LittleEndian.PutUint64(buf[:encodeSizeInt], uint64(v))
	return buf[encodeSizeInt:]
}
func encodeString(buf []byte, v string) []byte {
	buf = encodeInt(buf, int64(len(v)))
	copy(buf[:len(v)], v)
	return buf[len(v):]
}
func encodeFileStatus(buf []byte, v FileStatus) []byte {
	buf = encodeInt(buf, int64(v.Size))
	buf = encodeInt(buf, v.ModTime.UnixNano())
	buf = encodeInt(buf, int64(v.Mode))
	buf = encodeInt(buf, int64((int64(v.Uid)<<32)|int64(v.Gid)))
	exists := int64(0)
	if v.Exists {
		exists = 1
	}
	buf = encodeInt(buf, exists)
	return buf
}

func decodeInt(buf []byte) (int64, []byte) {
	v := binary.LittleEndian.Uint64(buf[:encodeSizeInt])
	return int64(v), buf[encodeSizeInt:]
}
func decodeString(buf []byte) (string, []byte) {
	size, buf := decodeInt(buf)
	v := string(buf[:size])
	return v, buf[size:]
}
func decodeFileStatus(buf []byte) (FileStatus, []byte) {
	size, buf := decodeInt(buf)
	modTime, buf := decodeInt(buf)
	mode, buf := decodeInt(buf)
	owners, buf := decodeInt(buf)
	exists, buf := decodeInt(buf)
	return FileStatus{
		Size:    size,
		ModTime: time.Unix(modTime/1e9, modTime%1e9),
		Mode:    os.FileMode(mode),
		Uid:     uint32(owners >> 32),
		Gid:     uint32(owners),
		Exists:  exists != 0,
	}, buf
}
