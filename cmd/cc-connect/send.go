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

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/daemon"
)

func runSend(args []string) {
	var project, sessionKey, dataDir, configPath, message string
	var useStdin bool

	var positional []string
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
		case "--message", "-m":
			if i+1 < len(args) {
				i++
				message = args[i]
			}
		case "--stdin":
			useStdin = true
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
			printSendUsage()
			return
		default:
			positional = append(positional, args[i])
		}
	}

	if useStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading stdin: %v\n", err)
			os.Exit(1)
		}
		message = strings.TrimSpace(string(data))
	}
	if message == "" {
		message = strings.Join(positional, " ")
	}
	if message == "" {
		fmt.Fprintln(os.Stderr, "Error: message is required")
		printSendUsage()
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
		"message":     message,
	})

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}

	resp, err := client.Post("http://unix/send", "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to connect: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", strings.TrimSpace(string(body)))
		os.Exit(1)
	}

	fmt.Println("Message sent successfully.")
}

func resolveSocketPath(dataDir, configPath string) (string, error) {
	if dataDir != "" {
		return filepath.Join(dataDir, "run", "api.sock"), nil
	}
	resolvedDataDir, err := resolveDefaultDataDir(configPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedDataDir, "run", "api.sock"), nil
}

func resolveDefaultDataDir(configPath string) (string, error) {
	if cfgPath := strings.TrimSpace(configPath); cfgPath != "" {
		dataDir, err := config.ResolveDataDirForConfigPath(cfgPath)
		if err != nil {
			return "", fmt.Errorf("resolve data_dir from config %s: %w", cfgPath, err)
		}
		return dataDir, nil
	}
	if meta, err := daemon.LoadMeta(); err == nil {
		if cfgPath := strings.TrimSpace(meta.ConfigPath); cfgPath != "" {
			if dataDir, err := config.ResolveDataDirForConfigPath(cfgPath); err == nil {
				return dataDir, nil
			}
		}
		if strings.TrimSpace(meta.DataDir) != "" {
			return meta.DataDir, nil
		}
	}
	if dataDir, ok := config.ResolveDefaultHomeDataDirFromHomeConfig(); ok {
		return dataDir, nil
	}
	return config.ResolveDefaultHomeDataDir(), nil
}

func printSendUsage() {
	fmt.Printf(`Usage: %s send [options] <message>
       %s send [options] -m <message>
       %s send [options] --stdin < file
       echo "msg" | %s send [options] --stdin

Send a message to an active %s session.

Options:
  -m, --message <text>     Message to send (preferred over positional args)
      --stdin              Read message from stdin (best for long/special-char messages)
  -p, --project <name>     Target project (optional if only one project)
  -s, --session <key>      Target session key (optional, picks first active)
      --config <path>      Config file used to resolve data_dir
      --data-dir <path>    Data directory (default: ~/.echo-client, fallback: ~/.cc-connect)
  -h, --help               Show this help

Examples:
  %s send "Daily summary: ..."
  %s send -m "Build completed successfully"
  %s send --stdin <<'EOF'
    Long message with "special" chars, $variables, and newlines
  EOF`, appName, appName, appName, appName, appName, appName, appName, appName)
}
