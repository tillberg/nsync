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
	Positional struct {
		LocalPath          string `positional-arg-name:"localpath"`
		RemoteUserHostPath string `positional-arg-name:"[remoteuser@]remotehost:remotepath"`
	} `positional-args:"yes" required:"yes"`

	IncludePath []string `long:"include-path" description:"Restrict syncing to this path, relative to the root synced path. Ignores still apply afterward. Repeat to specify multiple paths."`

	IgnorePart      []string `long:"ignore-part" description:"Ignore syncing any files/folders with these names, e.g. node_modules or .git. Repeat to specify multiple names."`
	IgnoreSuffix    []string `long:"ignore-suffix" description:"Ignore syncing any files/folders with these extensions, e.g. .a or .pyc. Repeat to specify multiple extensions."`
	IgnoreSubstring []string `long:"ignore-substring" description:"Ignore syncing any files/folders with these substrings in their whole paths, e.g. go/bin or some/node_modules. Repeat to specify multiple substrings."`

	RemoteUser  string   `long:"remote-user" description:"Execute nsync as a different user on the remote host."`
	RewriteUids []string `long:"rewrite-uid" description:"Rewrite UIDs from local to remote host, e.g. 501:1000. Repeat to specify multiple mappings."`
	RewriteGids []string `long:"rewrite-gid" description:"Rewrite GIDs from local to remote host, e.g. 20:1000. Repeat to specify multiple mappings."`

	NsyncPath string `long:"nsync-path" description:"Path to Nsync binary on the remote host. This is required for cross-architecture syncing. If not specified, Nsync uses rsync to copy itself to the remote host first."`

	Verbose bool `short:"v" long:"verbose" description:"Show verbose debug information"`
	NoColor bool `long:"no-color" description:"Disable ANSI colors"`
	Child   bool `long:"child" hidden:"true" description:"Internal use only. Puts this nsync instance into child/remote mode."`
}

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
	if len(args) > 0 {
		alog.Fatalf("Unexpected extra conditional args: %q\n", args)
	}
	if Opts.NoColor {
		alog.DisableColor()
	}
	includePath = stringset.New()
	for i, path := range Opts.IncludePath {
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
			Opts.IncludePath[i] = path
		}
		for strings.HasSuffix(path, "/") {
			path = path[:len(path)-1]
		}
		includePath.Add(path)
	}
	ignorePart = stringset.New(Opts.IgnorePart...)
	ignoreSuffix = Opts.IgnoreSuffix
	ignoreSubstring = Opts.IgnoreSubstring
	rewriteUidMap = parseIdMap(Opts.RewriteUids)
	rewriteGidMap = parseIdMap(Opts.RewriteGids)
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
	if Opts.Child {
		execChild()
		return
	}
	remoteFullPathParts := strings.SplitN(Opts.Positional.RemoteUserHostPath, ":", 2)
	remoteHost, remoteRoot := remoteFullPathParts[0], remoteFullPathParts[1]
	alog.Printf("@(dim:nsync started, syncing) @(cyan:%s) @(dim:to) @(cyan:%s)@(dim::)@(cyan:%s)\n", Opts.Positional.LocalPath, remoteHost, remoteRoot)
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

func parseIdMap(mapStrs []string) map[uint32]uint32 {
	m := map[uint32]uint32{}
	for _, part := range mapStrs {
		kvParts := strings.Split(part, ":")
		if len(kvParts) != 2 {
			alog.Panicf("Error parsing id mapping %q\n", part)
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
