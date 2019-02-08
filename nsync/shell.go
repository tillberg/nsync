package nsync

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jessevdk/go-flags"
	"github.com/tillberg/alog"
	"github.com/tillberg/autorestart"
	"github.com/tillberg/stringset"
)

var Opts struct {
	IncludePath string `long:"include-path"`

	IgnorePart      string `long:"ignore-part"`
	IgnoreSuffix    string `long:"ignore-suffix"`
	IgnoreSubstring string `long:"ignore-substring"`

	RemoteUser  string `long:"remote-user"`
	RewriteUids string `long:"rewrite-uids"`
	RewriteGids string `long:"rewrite-gids"`

	NsyncPath string `long:"nsync-path"`

	Verbose bool `short:"v" long:"verbose" description:"Show verbose debug information"`
	NoColor bool `long:"no-color" description:"Disable ANSI colors"`
	Child   bool `long:"child" description:"Sets this instance into child/remote mode"`
}

var RootPath string
var MessagesToParent = make(chan Message)
var MessagesToChild = make(chan Message)

var includePath = stringset.New()
var includeDirs = stringset.New()

var ignorePart = stringset.New()
var ignoreSuffix []string
var ignoreSubstring []string
var rewriteUidMap map[uint32]uint32
var rewriteGidMap map[uint32]uint32

func Shell() {
	// alog.EnableMultilineMode()
	sighup := autorestart.NotifyOnSighup()
	args, err := flags.ParseArgs(&Opts, os.Args[1:])
	if err != nil {
		err2, ok := err.(*flags.Error)
		if ok && err2.Type == flags.ErrHelp {
			return
		}
		alog.Printf("Error parsing command-line options: %s\n", err)
		return
	}
	if Opts.NoColor {
		alog.DisableColor()
	}
	includePath = stringset.New()
	for _, path := range filepath.SplitList(Opts.IncludePath) {
		for strings.HasSuffix(path, "/") {
			path = path[:len(path)-1]
		}
		includePath.Add(path)
	}
	ignorePart = stringset.New(filepath.SplitList(Opts.IgnorePart)...)
	ignoreSuffix = filepath.SplitList(Opts.IgnoreSuffix)
	ignoreSubstring = filepath.SplitList(Opts.IgnoreSubstring)
	rewriteUidMap = parseIdMap(Opts.RewriteUids)
	rewriteGidMap = parseIdMap(Opts.RewriteGids)
	if Opts.Child {
		if len(args) < 1 {
			alog.Fatalln("Not enough arguments, need 1")
		}
		RootPath = args[0]
		execChild()
		return
	}
	if len(args) < 2 {
		alog.Fatalln("Not enough arguments, need 2")
	}
	for path := range includePath.Raw() {
		for path != "." && path != "/" {
			includeDirs.Add(path)
			path = filepath.Dir(path)
		}
		includeDirs.Add("/")
	}
	if includeDirs.Len() != 0 {
		alog.Printf("Include paths: %q\n", includePath.All())
		alog.Printf("Include dirs: %q\n", includeDirs.All())
	}
	RootPath = args[0]
	remoteFullPath := args[1]
	remoteFullPathParts := strings.SplitN(remoteFullPath, ":", 2)
	remoteHost, remoteRoot := remoteFullPathParts[0], remoteFullPathParts[1]
	alog.Printf("@(dim:nsync started, syncing) @(cyan:%s) @(dim:to) @(cyan:%s)@(dim::)@(cyan:%s)\n", RootPath, remoteHost, remoteRoot)
	if len(rewriteUidMap)|len(rewriteGidMap) != 0 {
		alog.Printf("@(dim:using uidMap: %v, gidMap %v)\n", rewriteUidMap, rewriteGidMap)
	}
	onChildExit := make(chan error)
	go connectChildForever(remoteHost, remoteRoot, Opts.RemoteUser, Opts.NsyncPath, onChildExit)
	go execParent()
	select {
	case <-sighup:
		killChildSshProcess()
	case <-onChildExit:
		// Restart everything if the child drops out (this is kludgy but effective)
	}
}

func parseIdMap(mapStr string) map[uint32]uint32 {
	m := map[uint32]uint32{}
	for _, part := range strings.Split(mapStr, ",") {
		if len(part) == 0 {
			continue
		}
		kvParts := strings.Split(part, ":")
		if len(kvParts) != 2 {
			alog.Panicf("Error parsing %q\n", mapStr)
		}
		kStr, vStr := kvParts[0], kvParts[1]
		k, err := strconv.ParseInt(kStr, 10, 32)
		alog.BailIf(err)
		v, err := strconv.ParseInt(vStr, 10, 32)
		alog.BailIf(err)
		m[uint32(k)] = uint32(v)
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
