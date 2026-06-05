package main

import (
	"flag"
	"fmt"

	"github.com/bermudi/cmd-code-proxy/internal/proxy"
	"github.com/bermudi/cmd-code-proxy/internal/server"
)

const appVersion = "v1.0.8"
const repositoryURL = "https://github.com/bermudi/cmd-code-proxy"
const debugLogging = false

func main() {
	port := flag.String("port", "", "Port to run the server on (default: 55990)")
	host := flag.String("host", "", "Host to bind to (default: 127.0.0.1)")
	apiKey := flag.String("api-key", "", "API key for CommandCode (optional, can also be set via Authorization header)")
	listClosed := flag.Bool("list-closed-models", false, "Include closed/premium models (Claude, GPT) in /v1/models")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(appVersion)
		return
	}

	proxy := proxy.NewProxy(*apiKey, proxy.NewCCAdapter().WithDebug(debugLogging))
	proxy.Debug = debugLogging
	proxy.ListClosedModels = *listClosed

	srv := server.NewServer(proxy)
	srv.SetPort(*port)
	srv.SetHost(*host)

	printStartupInfo(srv)

	srv.Start()
}

func printStartupInfo(srv *server.Server) {
	fmt.Println("")
	fmt.Println("========================================")
	fmt.Println("  CommandCode Proxy Server")
	fmt.Println("========================================")
	fmt.Println("")
	fmt.Printf("  Version:     %s\n", appVersion)
	fmt.Printf("  Repository:  %s\n", repositoryURL)
	fmt.Printf("  Host:        %s\n", srv.GetHost())
	fmt.Printf("  Port:        %s\n", srv.GetPort())
	fmt.Println("  Base URL:    https://api.commandcode.ai")
	fmt.Println("")
	fmt.Println("  Endpoints:")
	fmt.Println("    POST /v1/chat/completions  (OpenAI-compatible)")
	fmt.Println("    POST /chat/completions     (OpenAI-compatible alias)")
	fmt.Println("    POST /v1/responses         (OpenAI Responses-compatible)")
	fmt.Println("    GET  /v1/models            (list models)")
	fmt.Println("    GET  /health               (health check)")
	fmt.Println("")
	fmt.Printf("  Server running on http://%s:%s\n", srv.GetHost(), srv.GetPort())
	fmt.Println("")
	fmt.Println("  Press Ctrl+C to stop")
	fmt.Println("========================================")
}
