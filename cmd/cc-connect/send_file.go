package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func runSendFile(args []string) {
	var project, sessionKey, dataDir, configPath, path, caption string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--project", "-p":
			if i+1 < len(args) {
				i++
				project = args[i]
			}
		case "--session", "-s":
			if i+1 < len(args) {
				i++
				sessionKey = args[i]
			}
		case "--path":
			if i+1 < len(args) {
				i++
				path = args[i]
			}
		case "--caption":
			if i+1 < len(args) {
				i++
				caption = args[i]
			}
		case "--data-dir":
			if i+1 < len(args) {
				i++
				dataDir = args[i]
			}
		case "--config":
			if i+1 < len(args) {
				i++
				configPath = args[i]
			}
		case "--help", "-h":
			printSendFileUsage()
			return
		default:
			if strings.HasPrefix(args[i], "-") {
				fmt.Fprintf(os.Stderr, "Error: unknown option %s\n", args[i])
				printSendFileUsage()
				os.Exit(1)
			}
			if path == "" {
				path = args[i]
			} else {
				caption = strings.TrimSpace(strings.Join([]string{caption, args[i]}, " "))
			}
		}
	}

	project, sessionKey = resolveSendFileTarget(project, sessionKey)

	if strings.TrimSpace(path) == "" {
		fmt.Fprintln(os.Stderr, "Error: path is required")
		printSendFileUsage()
		os.Exit(1)
	}
	if !filepath.IsAbs(path) {
		fmt.Fprintln(os.Stderr, "Error: path must be absolute")
		printSendFileUsage()
		os.Exit(1)
	}

	sockPath, err := resolveSocketPath(dataDir, configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stat(sockPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: %s is not running (socket not found: %s)\n", appName, sockPath)
		os.Exit(1)
	}

	payload, _ := json.Marshal(map[string]string{
		"project":     project,
		"session_key": sessionKey,
		"path":        path,
		"caption":     caption,
	})

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}

	resp, err := client.Post("http://unix/send-file", "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to connect: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", strings.TrimSpace(string(body)))
		os.Exit(1)
	}

	fmt.Println("File sent successfully.")
}

func resolveSendFileTarget(project, sessionKey string) (string, string) {
	project = strings.TrimSpace(project)
	if project == "" {
		project = strings.TrimSpace(os.Getenv("CC_PROJECT"))
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		sessionKey = strings.TrimSpace(os.Getenv("CC_SESSION_KEY"))
	}
	return project, sessionKey
}

func printSendFileUsage() {
	fmt.Printf(`Usage: %s send-file [options] --path <absolute-path>
       %s send-file [options] <absolute-path>

Send a local file to an active %s session.

Options:
      --path <path>        Absolute local file path to send
      --caption <text>     Optional short caption sent alongside the file
  -p, --project <name>     Target project (auto-detected from CC_PROJECT env)
  -s, --session <key>      Target session (auto-detected from CC_SESSION_KEY env)
      --config <path>      Config file used to resolve data_dir
      --data-dir <path>    Data directory (default: ~/.echo-client, fallback: ~/.cc-connect)
  -h, --help               Show this help

Examples:
  %s send-file --path /tmp/report.pdf
  %s send-file --path /tmp/screenshot.png --caption "Latest screenshot"`, appName, appName, appName, appName, appName)
}
