// A webapp for looking at and searching through files.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
)

const scriptDescription = `
Usage: tailon-ng [options] <path> [<path> ...]

Tailon-ng is a webapp for searching through log files from your browser.
`

const scriptEpilog = `
Tailon-ng is configured entirely through command-line flags.

Each <path> is a file, a directory, or a shell glob, where "*" matches within a
directory and "**" across them (so "/var/log/**.log" finds .log files at any
depth). Directories are served recursively, and new files are picked up as they
appear. Several paths can be given as separate arguments or comma-separated.

Rotation leftovers (.gz, .bz2, .xz, .zst, .1, -YYYYMMDD, .old, .bak) are listed
but excluded from live tailing and plain grep. The web UI's grep-all mode also
searches them, decompressed transparently.

On Linux, appended lines are pushed instantly via inotify; elsewhere, and on
filesystems without notification support, tailon-ng falls back to polling.

Example usage:
  tailon-ng /var/log/syslog /var/log/auth.log
  tailon-ng /var/log/nginx/,/var/log/apache/
  tailon-ng /var/log/remote/
  tailon-ng "/var/log/**.log"
  tailon-ng -b 127.0.0.1:8080 /var/log/messages
`

const scriptOptions = `  -b, --bind string            Address and port to listen on (default ":8080")
  -h, --help                   Show this help message and exit
  -r, --relative-root string   Webapp relative root (default "/")`

// gatherSources flattens the positional command-line arguments into a list of
// paths. Each argument may itself be a comma-separated list, so "tailon-ng a b"
// and "tailon-ng a,b" name the same two sources.
func gatherSources(args []string) []string {
	var sources []string
	for _, arg := range args {
		for _, s := range strings.Split(arg, ",") {
			if s = strings.TrimSpace(s); s != "" {
				sources = append(sources, s)
			}
		}
	}
	return sources
}

// version is stamped by the release build via -ldflags "-X main.version=vN"
// (see .github/workflows/build.yml), so the UI always shows the tag it was
// built from. Local builds show "dev".
var version = "dev"

// Config contains all backend and frontend configuration options and relevant state.
type Config struct {
	RelativeRoot string
	BindAddr     string
	Version      string

	Sources []string
}

// defaultConfig returns Tailon-ng's built-in configuration. There is no config
// file; settings come from command-line flags.
func defaultConfig() *Config {
	return &Config{
		RelativeRoot: "/",
		BindAddr:     ":8080",
		Version:      version,
	}
}

var config = &Config{}

func main() {
	config = defaultConfig()

	var printHelp bool

	// The standard library flag package accepts both -name and --name. Register
	// a long and a short name for each option so that e.g. --bind and -b are
	// equivalent.
	flag.BoolVar(&printHelp, "help", false, "Show this help message and exit")
	flag.BoolVar(&printHelp, "h", false, "Show this help message and exit")
	flag.StringVar(&config.BindAddr, "bind", config.BindAddr, "Address and port to listen on")
	flag.StringVar(&config.BindAddr, "b", config.BindAddr, "Address and port to listen on")
	flag.StringVar(&config.RelativeRoot, "relative-root", config.RelativeRoot, "Webapp relative root")
	flag.StringVar(&config.RelativeRoot, "r", config.RelativeRoot, "Webapp relative root")

	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, strings.TrimLeft(scriptDescription, "\n"))
		fmt.Fprintln(os.Stderr, scriptOptions)
		fmt.Fprintln(os.Stderr, strings.TrimRight(scriptEpilog, "\n"))
	}

	flag.Parse()

	if printHelp {
		flag.Usage()
		os.Exit(0)
	}

	// Ensure that relative root is always '/' or '/$arg/'.
	config.RelativeRoot = "/" + strings.TrimLeft(config.RelativeRoot, "/")
	config.RelativeRoot = strings.TrimRight(config.RelativeRoot, "/") + "/"

	config.Sources = gatherSources(flag.Args())
	if len(config.Sources) == 0 {
		fmt.Fprintln(os.Stderr, "No paths specified on the command-line")
		os.Exit(2)
	}

	log.Print("Generate initial file listing")
	createListing(config.Sources)

	startServer(config, config.BindAddr)
}

// startServer serves forever on bindAddr — a TCP "host:port" address, or a
// filesystem path to bind a unix socket (useful behind a reverse proxy).
func startServer(config *Config, bindAddr string) {
	logger := log.New(os.Stdout, "", log.LstdFlags)
	logger.Printf("Server start, relative-root: %s, bind-addr: %s\n", config.RelativeRoot, bindAddr)

	server := setupServer(config, bindAddr, logger)

	if strings.Contains(bindAddr, ":") {
		server.ListenAndServe()
	} else {
		os.Remove(bindAddr)

		unixAddr, _ := net.ResolveUnixAddr("unix", bindAddr)
		unixListener, err := net.ListenUnix("unix", unixAddr)
		if err != nil {
			panic(err)
		}
		unixListener.SetUnlinkOnClose(true)

		defer unixListener.Close()
		server.Serve(unixListener)
	}
}
