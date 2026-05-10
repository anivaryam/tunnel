package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/anivaryam/tunnel/pkg/config"
)

// runDoctor walks through every common setup failure mode and prints a
// pass/fail line per check. Designed to be the first thing a confused user
// runs when `tunnel http <port>` doesn't work.
func runDoctor(serverFlag, tokenFlag string) error {
	cfgPath := config.DefaultPath()
	cfg, _ := config.Load(cfgPath)

	server := serverFlag
	if server == "" {
		server = cfg.ServerURL
	}
	token := tokenFlag
	if token == "" {
		token = cfg.AuthToken
	}

	pass, fail, warn := green("✓"), red("✗"), yellow("!")

	fmt.Printf("config file: %s\n", cfgPath)
	if _, statErr := os.Stat(cfgPath); os.IsNotExist(statErr) {
		fmt.Printf("  %s config file does not exist — run `tunnel config set-server <url>`\n", warn)
	} else if info, _ := os.Stat(cfgPath); info != nil && info.Mode().Perm()&0o077 != 0 {
		fmt.Printf("  %s mode %o — token is readable by other users; recommended 0600\n", warn, info.Mode().Perm())
	} else {
		fmt.Printf("  %s permissions OK\n", pass)
	}

	if server == "" {
		fmt.Printf("server url:  (not set)\n  %s run `tunnel config set-server <url>`\n", fail)
		return fmt.Errorf("server URL not configured")
	}
	fmt.Printf("server url:  %s\n", server)
	u, err := url.Parse(server)
	if err != nil {
		fmt.Printf("  %s parse error: %v\n", fail, err)
		return err
	}
	switch u.Scheme {
	case "http", "https", "ws", "wss":
		fmt.Printf("  %s scheme %q OK\n", pass, u.Scheme)
	default:
		fmt.Printf("  %s scheme %q invalid — must be http/https/ws/wss\n", fail, u.Scheme)
	}
	if u.Host == "" {
		fmt.Printf("  %s no host\n", fail)
	} else {
		fmt.Printf("  %s host %q\n", pass, u.Host)
	}

	healthURL := strings.TrimSuffix(server, "/") + "/health"
	if strings.HasPrefix(healthURL, "ws://") {
		healthURL = "http://" + healthURL[5:]
	} else if strings.HasPrefix(healthURL, "wss://") {
		healthURL = "https://" + healthURL[6:]
	}

	fmt.Printf("relay reachability: GET %s\n", healthURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", healthURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("  %s %v\n", fail, err)
		fmt.Printf("    is the relay running? is the URL correct?\n")
	} else {
		resp.Body.Close()
		if resp.StatusCode == 200 {
			fmt.Printf("  %s relay responded 200 OK\n", pass)
		} else {
			fmt.Printf("  %s relay responded %d %s\n", fail, resp.StatusCode, resp.Status)
		}
	}

	fmt.Printf("auth token:  ")
	switch {
	case token == "":
		fmt.Printf("(not set)\n  %s if the relay enforces TUNNEL_AUTH_TOKENS, you need a token\n", warn)
	case len(token) <= 4:
		fmt.Printf("***\n  %s token unusually short\n", warn)
	default:
		fmt.Printf("%s***\n  %s token present\n", token[:4], pass)
	}

	fmt.Println()
	fmt.Println("If everything above is OK, try: tunnel http <port>")
	return nil
}

// runList queries the relay's /dashboard/api endpoint to enumerate active
// tunnels. Requires the same auth token as the dashboard.
func runList(serverFlag, tokenFlag string, jsonOut bool) error {
	cfg, _ := config.Load(config.DefaultPath())
	server := serverFlag
	if server == "" {
		server = cfg.ServerURL
	}
	if server == "" {
		return fmt.Errorf("server URL required: use --server or `tunnel config set-server <url>`")
	}
	token := tokenFlag
	if token == "" {
		token = cfg.AuthToken
	}

	apiURL := strings.TrimSuffix(server, "/") + "/dashboard/api"
	if strings.HasPrefix(apiURL, "ws://") {
		apiURL = "http://" + apiURL[5:]
	} else if strings.HasPrefix(apiURL, "wss://") {
		apiURL = "https://" + apiURL[6:]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", apiURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		return fmt.Errorf("unauthorized — set the same token used by the relay's TUNNEL_AUTH_TOKENS")
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("relay returned %d %s", resp.StatusCode, resp.Status)
	}

	var payload struct {
		Tunnels []struct {
			ID          string `json:"id"`
			Mode        string `json:"mode"`
			RemoteAddr  string `json:"remote_addr"`
			ConnectedAt string `json:"connected_at"`
			Duration    string `json:"duration"`
		} `json:"tunnels"`
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(payload)
	}

	if payload.Count == 0 {
		fmt.Println("no active tunnels")
		return nil
	}
	fmt.Printf("%-20s %-6s %-22s %s\n", "ID", "MODE", "REMOTE", "UPTIME")
	for _, t := range payload.Tunnels {
		fmt.Printf("%-20s %-6s %-22s %s\n", t.ID, t.Mode, t.RemoteAddr, t.Duration)
	}
	return nil
}

func green(s string) string  { return colorIf("\x1b[32m", s) }
func red(s string) string    { return colorIf("\x1b[31m", s) }
func yellow(s string) string { return colorIf("\x1b[33m", s) }

func colorIf(code, s string) string {
	if !diagColorEnabled {
		return s
	}
	return code + s + "\x1b[0m"
}

var diagColorEnabled = func() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if v := os.Getenv("CLICOLOR"); v == "0" {
		return false
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	if v := os.Getenv("CLICOLOR_FORCE"); v != "" && v != "0" {
		return true
	}
	// Default: assume colors OK when stdout is connected somewhere.
	// (Doctor output is human-targeted; over-conservatism would mute too often.)
	return true
}()
