// A webapp for looking at and searching through files.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
appear. Several paths can be given as separate arguments.

Rotation leftovers (.gz, .bz2, .xz, .zst, .1, -YYYYMMDD, .old, .bak) never get
appended, so they stay hidden until the "gz" toggle (web UI, in find
mode's search input) turns them on: the file selector then lists them, and find searches
them too, decompressed transparently.

On Linux, appended lines are pushed instantly via inotify; elsewhere, and on
filesystems without notification support, tailon-ng falls back to polling.

Example usage:
  tailon-ng /var/log/syslog /var/log/auth.log
  tailon-ng /var/log/nginx/ /var/log/apache/
  tailon-ng /var/log/remote/
  tailon-ng "/var/log/**.log"
  tailon-ng -b 127.0.0.1:8080 /var/log/messages
`

const scriptOptions = `  -b, --bind string            Address and port to listen on (default ":8080")
  -h, --help                   Show this help message and exit
  -r, --relative-root string   Webapp relative root (default "/")`

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

	// Each positional argument is one path, taken verbatim (commas and spaces
	// are legal in filenames); empty arguments are skipped.
	for _, arg := range flag.Args() {
		if arg != "" {
			config.Sources = append(config.Sources, arg)
		}
	}
	if len(config.Sources) == 0 {
		fmt.Fprintln(os.Stderr, "No paths specified on the command-line")
		os.Exit(2)
	}

	slog.Info("generating initial file listing")
	createListing(config.Sources)

	startServer(config, config.BindAddr)
}

// startServer serves forever on bindAddr — a TCP "host:port" address, or a
// filesystem path to bind a unix socket (useful behind a reverse proxy). It
// only returns to exit the process: there is no graceful-shutdown path, so
// Serve coming back always means failure.
func startServer(config *Config, bindAddr string) {
	slog.Info("server start", "relative-root", config.RelativeRoot, "bind-addr", bindAddr)

	server := setupServer(config, bindAddr)

	var err error
	if strings.Contains(bindAddr, ":") {
		err = server.ListenAndServe()
	} else {
		err = serveUnix(server, bindAddr)
	}
	slog.Error("server failed", "err", err)
	os.Exit(1)
}

// serveUnix serves on a unix socket at path. The socket file is removed again
// when the listener closes (net's default for sockets it creates).
func serveUnix(server *http.Server, path string) error {
	// Remove a stale socket left by an unclean shutdown — binding over an
	// existing file fails with "address already in use". Only ever remove a
	// socket: a typo'd -b must not delete a regular file.
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refusing to bind %q: it exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer listener.Close()
	return server.Serve(listener)
}
