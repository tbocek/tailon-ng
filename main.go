// A webapp for looking at and searching through files.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
)

const scriptDescription = `
Usage: tailon [options] <filespec> [<filespec> ...]

Tailon is a webapp for searching through files and streams.
`

const scriptEpilog = `
Tailon is configured entirely through command-line flags.

The command-line interface expects one or more <filespec> arguments, which
specify the files to serve. The format is:

  [alias=name,group=name]<source>

The "source" specifier can be a file name, glob or directory. The optional
"alias=" and "group=" specifiers change the display name of files in the UI
and the group in which they appear.

A file specifier points to a single, possibly non-existent file. The file
name can be overwritten with "alias=". For example:

  tailon alias=error.log,/var/log/apache/error.log

A glob evaluates to the list of files that match a shell filename pattern.
The pattern is evaluated each time the file list is refreshed. An "alias="
specifier overwrites the parent directory of each matched file in the UI.

  tailon "/var/log/apache/*.log" "alias=nginx,/var/log/nginx/*.log"

If a directory is given, all files under it are served recursively.

  tailon /var/log/apache/ /var/log/nginx/

Example usage:
  tailon file1.txt file2.txt file3.txt
  tailon alias=messages,/var/log/messages "/var/log/*.log"
  tailon -b localhost:8080,localhost:8081 /var/log/messages
`

const scriptOptions = `  -a, --allow-download         Allow file downloads (default true)
  -b, --bind string            Address and port to listen on (default ":8080")
  -h, --help                   Show this help message and exit
  -r, --relative-root string   Webapp relative root (default "/")`

// FileSpec is an instance of a file to be monitored. These are mapped to the
// <filespec> command-line arguments.
type FileSpec struct {
	Path  string
	Type  string
	Alias string
	Group string
}

// Parse a string into a filespec. Example inputs are:
//
//	alias=1,group=2,/var/log/messages
//	/var/log/
//	/var/log/*
func parseFileSpec(spec string) (FileSpec, error) {
	var filespec FileSpec
	var path string
	parts := strings.Split(spec, ",")

	if length := len(parts); length == 1 {
		path = parts[0]
	} else {
		// The last part is the path. We'll probably need a more robust
		// solution in the future.
		path, parts = parts[len(parts)-1], parts[:len(parts)-1]
	}

	if strings.ContainsAny(path, "*?[]") {
		filespec.Type = "glob"
	} else {
		stat, err := os.Lstat(path)
		if os.IsNotExist(err) || stat.Mode().IsRegular() {
			filespec.Type = "file"
		} else if stat.Mode().IsDir() {
			filespec.Type = "dir"
		}
	}

	for _, part := range parts {
		if strings.HasPrefix(part, "group=") {
			group := strings.SplitN(part, "=", 2)[1]
			group = strings.Trim(group, "'\" ")
			filespec.Group = group
		} else if strings.HasPrefix(part, "alias=") {
			filespec.Alias = strings.SplitN(part, "=", 2)[1]
		}
	}

	if filespec.Type == "" {
		filespec.Type = "file"
	}
	filespec.Path = path
	return filespec, nil

}

// Config contains all backend and frontend configuration options and relevant state.
type Config struct {
	RelativeRoot  string
	BindAddr      []string
	AllowDownload bool

	FileSpecs []FileSpec
}

// defaultConfig returns Tailon's built-in configuration. There is no config
// file; settings come from command-line flags.
func defaultConfig() *Config {
	return &Config{
		RelativeRoot:  "/",
		BindAddr:      []string{":8080"},
		AllowDownload: true,
	}
}

var config = &Config{}

func main() {
	config = defaultConfig()

	var (
		printHelp bool
		bindAddr  = strings.Join(config.BindAddr, ",")
	)

	// The standard library flag package accepts both -name and --name. Register
	// a long and a short name for each option so that e.g. --bind and -b are
	// equivalent.
	flag.BoolVar(&printHelp, "help", false, "Show this help message and exit")
	flag.BoolVar(&printHelp, "h", false, "Show this help message and exit")
	flag.StringVar(&bindAddr, "bind", bindAddr, "Address and port to listen on")
	flag.StringVar(&bindAddr, "b", bindAddr, "Address and port to listen on")
	flag.StringVar(&config.RelativeRoot, "relative-root", config.RelativeRoot, "Webapp relative root")
	flag.StringVar(&config.RelativeRoot, "r", config.RelativeRoot, "Webapp relative root")
	flag.BoolVar(&config.AllowDownload, "allow-download", config.AllowDownload, "Allow file downloads")
	flag.BoolVar(&config.AllowDownload, "a", config.AllowDownload, "Allow file downloads")

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

	config.BindAddr = strings.Split(bindAddr, ",")

	// Ensure that relative root is always '/' or '/$arg/'.
	config.RelativeRoot = "/" + strings.TrimLeft(config.RelativeRoot, "/")
	config.RelativeRoot = strings.TrimRight(config.RelativeRoot, "/") + "/"

	// Handle command-line file specs.
	filespecs := make([]FileSpec, 0, len(flag.Args()))
	for _, spec := range flag.Args() {
		if filespec, err := parseFileSpec(spec); err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing argument '%s': %s\n", spec, err)
			os.Exit(1)
		} else {
			filespecs = append(filespecs, filespec)
		}
	}
	config.FileSpecs = filespecs

	if len(config.FileSpecs) == 0 {
		fmt.Fprintln(os.Stderr, "No files specified on the command-line")
		os.Exit(2)
	}

	log.Print("Generate initial file listing")
	createListing(config.FileSpecs)

	var wg sync.WaitGroup
	for _, addr := range config.BindAddr {
		wg.Add(1)
		go startServer(config, addr)
	}
	wg.Wait()

}

func startServer(config *Config, bindAddr string) {
	loggerHTML := log.New(os.Stdout, "", log.LstdFlags)
	loggerHTML.Printf("Server start, relative-root: %s, bind-addr: %s\n", config.RelativeRoot, bindAddr)

	server := setupServer(config, bindAddr, loggerHTML)

	if strings.Contains(bindAddr, ":") {
		server.ListenAndServe()
	} else {
		os.Remove(bindAddr)

		unixAddr, _ := net.ResolveUnixAddr("unix", bindAddr)
		unixListener, err := net.ListenUnix("unix", unixAddr)
		unixListener.SetUnlinkOnClose(true)

		if err != nil {
			panic(err)
		}

		defer unixListener.Close()
		server.Serve(unixListener)
	}
}
