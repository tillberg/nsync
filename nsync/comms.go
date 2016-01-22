package nsync

import (
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/tillberg/ansi-log"
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
			done <- childSshProcess.Kill()
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

var keepAliveInterval = 30 * time.Second

func receiveMessages(reader io.Reader, messages chan<- Message, done chan error) {
	for {
		var op OpByte
		// alog.Println("READING OP")
		err := binary.Read(reader, binary.LittleEndian, &op)
		if err != nil {
			alog.Printf("@(error:Error reading message: %v)\n", err)
			done <- err
			return
		}
		var size uint32
		// alog.Println("READING SIZE")
		err = binary.Read(reader, binary.LittleEndian, &size)
		if err != nil {
			alog.Printf("@(error:Error reading message: %v)\n", err)
			done <- err
			return
		}
		var buf = make([]byte, size)
		// alog.Println("READING BUF", size)
		_, err = io.ReadFull(reader, buf)
		if err != nil {
			alog.Printf("@(error:Error reading message: %v)\n", err)
			done <- err
			return
		}
		// alog.Println("READ DONE")
		messages <- Message{
			Op:  op,
			Buf: buf,
		}
	}
}
func sendMessages(writer io.Writer, messages <-chan Message, done chan error) {
	for message := range messages {
		// alog.Println("SENDING OP")
		err := binary.Write(writer, binary.LittleEndian, message.Op)
		if err != nil {
			alog.Printf("@(error:Error writing message: %v)\n", err)
			done <- err
			return
		}
		// alog.Println("SENDING SIZE")
		err = binary.Write(writer, binary.LittleEndian, uint32(len(message.Buf)))
		if err != nil {
			alog.Printf("@(error:Error writing message: %v)\n", err)
			done <- err
			return
		}
		// alog.Println("SENDING BUF", len(message.Buf))
		_, err = writer.Write(message.Buf)
		if err != nil {
			alog.Printf("@(error:Error writing message: %v)\n", err)
			done <- err
			return
		}
		// alog.Println("SEND DONE")
	}
}

func connectChild(remoteHost, remoteRoot string) {
	lg := alog.New(os.Stderr, "@(dim:{isodate} [remote]) ", 0)
	exePath, err := exec.LookPath(os.Args[0])
	// alog.Println("My path is", exePath)
	ctx := bismuth.NewExecContext()
	ctx.Connect()
	ctx.Quote("copy-mkdir", "ssh", remoteHost, "mkdir -p ~/.csync")
	retCode, err := ctx.Quote("copy", "rsync", "-a", exePath, remoteHost+":~/.csync/csync")
	if retCode != 0 {
		alog.Println("copy retCode", retCode)
	}
	if err != nil {
		alog.Println(err)
		return
	}
	cmd := exec.Command("ssh", remoteHost, "~/.csync/csync", "--child", remoteRoot)
	if Opts.Verbose {
		cmd.Args = append(cmd.Args, "--verbose")
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
		}
	}
	if err := cmd.Wait(); err != nil {
		lg.Printf("@(error:Error on subprocess exit: %s)\n", err)
	}
	childSshProcessMutex.Lock()
	childSshProcess = nil
	childSshProcessMutex.Unlock()
}

func connectChildForever(remoteHost, remoteRoot string) {
	for {
		connectChild(remoteHost, remoteRoot)
		time.Sleep(5 * time.Second)
		alog.Printf("@(dim:Reconnecting child...)\n")
	}
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

const encodeSizeInt = 8
const encodeSizeFileStatus = 4 * encodeSizeInt

type FileStatus struct {
	Size    int64
	ModTime time.Time
	Mode    os.FileMode
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
	exists, buf := decodeInt(buf)
	return FileStatus{
		Size:    size,
		ModTime: time.Unix(modTime/1e9, modTime%1e9),
		Mode:    os.FileMode(mode),
		Exists:  exists != 0,
	}, buf
}