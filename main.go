package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/bermudi/cmd-code-proxy/internal/proxy"
)

const appVersion = "v1.0.8"
const repositoryURL = "https://github.com/bermudi/cmd-code-proxy"

const defaultPort = "55990"
const defaultHost = "127.0.0.1"

func main() {
	port := flag.String("port", "", "Port to run the server on (default: 55990)")
	host := flag.String("host", "", "Host to bind to (default: 127.0.0.1)")
	apiKey := flag.String("api-key", "", "API key for CommandCode (optional, can also be set via Authorization header)")
	listClosed := flag.Bool("list-closed-models", false, "Include closed/premium models (Claude, GPT) in /v1/models")
	captureDir := flag.String("capture-dir", "", "Directory to save raw upstream NDJSON streams (for debugging/fixture capture)")
	workingDir := flag.String("working-dir", "", "Working directory/project context to send to CommandCode (default: proxy process working directory)")
	showVersion := flag.Bool("version", false, "Print version and exit")
	debug := flag.Bool("debug", false, "Enable debug-level logging")
	flag.Parse()

	if *showVersion {
		fmt.Println(appVersion)
		return
	}

	bindHost := defaultHost
	if *host != "" {
		bindHost = *host
	}
	bindPort := defaultPort
	if *port != "" {
		bindPort = *port
	}

	proxy.InitLogger()
	if *debug {
		proxy.EnableDebugLogging()
	}

	p := proxy.NewProxy(*apiKey, proxy.NewCCAdapter())
	p.ListClosedModels = *listClosed
	p.WorkingDir = *workingDir

	if *captureDir != "" {
		if err := os.MkdirAll(*captureDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "capture-dir: %v\n", err)
			os.Exit(1)
		}
		p.CaptureDir = *captureDir
	}

	printStartupInfo(bindHost, bindPort)

	addr := bindHost + ":" + bindPort
	if err := http.ListenAndServe(addr, proxy.NewRouter(p)); err != nil {
		fmt.Fprintf(os.Stderr, "Server failed: %v\n", err)
		os.Exit(1)
	}
}

func printStartupInfo(host, port string) {
	fmt.Println("")
	fmt.Println("========================================")
	fmt.Println("  CommandCode Proxy Server")
	fmt.Println("========================================")
	fmt.Println("")
	fmt.Printf("  Version:     %s\n", appVersion)
	fmt.Printf("  Repository:  %s\n", repositoryURL)
	fmt.Printf("  Host:        %s\n", host)
	fmt.Printf("  Port:        %s\n", port)
	fmt.Println("  Base URL:    https://api.commandcode.ai")
	fmt.Println("")
	fmt.Println("  Endpoints:")
	fmt.Println("    POST /v1/chat/completions  (OpenAI-compatible)")
	fmt.Println("    POST /chat/completions     (OpenAI-compatible alias)")
	fmt.Println("    POST /v1/responses         (OpenAI Responses-compatible)")
	fmt.Println("    GET  /v1/models            (list models)")
	fmt.Println("    GET  /health               (health check)")
	fmt.Println("")
	fmt.Printf("  Server running on http://%s:%s\n", host, port)
	fmt.Println("")
	fmt.Println("  Press Ctrl+C to stop")
	fmt.Println("========================================")
}
