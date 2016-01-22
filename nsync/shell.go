package nsync

import (
	"os"
	"strings"

	"github.com/jessevdk/go-flags"
	"github.com/tillberg/ansi-log"
	"github.com/tillberg/autorestart"
)

var Opts struct {
	Verbose bool `short:"v" long:"verbose" description:"Show verbose debug information"`
	NoColor bool `long:"no-color" description:"Disable ANSI colors"`
	Child   bool `long:"child" description:"Sets this instance into child/remote mode"`
}

var RootPath string
var MessagesToParent = make(chan Message)
var MessagesToChild = make(chan Message)

func Shell() {
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
	RootPath = args[0]
	remoteFullPath := args[1]
	remoteFullPathParts := strings.SplitN(remoteFullPath, ":", 2)
	remoteHost, remoteRoot := remoteFullPathParts[0], remoteFullPathParts[1]
	alog.Printf("@(dim:csync started, syncing) @(cyan:%s) @(dim:to) @(cyan:%s)@(dim::)@(cyan:%s)\n", RootPath, remoteHost, remoteRoot)
	go connectChildForever(remoteHost, remoteRoot)
	go execParent()
	<-sighup
	killChildSshProcess()
}
