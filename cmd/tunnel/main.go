package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/anivaryam/tunnel/internal/client"
	"github.com/anivaryam/tunnel/internal/logging"
	"github.com/anivaryam/tunnel/pkg/config"
	"github.com/spf13/cobra"
)

// parsePort parses and range-checks a TCP/UDP port (1-65535).
func parsePort(raw string) (int, error) {
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid port %q (must be 1-65535)", raw)
	}
	return port, nil
}

// validateServerURL accepts http(s):// or ws(s):// URLs only and refuses
// values without a host. Without this, set-server happily saves garbage
// like "not a url" and every subsequent tunnel command fails with an
// inscrutable parse error instead of a clear setup error.
func validateServerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https", "ws", "wss":
	default:
		return fmt.Errorf("URL must start with http://, https://, ws://, or wss://; got %q", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("URL %q has no host", raw)
	}
	return nil
}

var version = "dev"

func main() {
	var serverFlag string
	var tokenFlag string
	var inspectFlag bool
	var quietFlag bool
	var nameFlag string
	var listJSONFlag bool
	var debugFlag bool
	var logJSONFlag bool
	var daemonFlags daemonFlagSet

	rootCmd := &cobra.Command{
		Use:           "tunnel",
		Short:         "Expose local servers to the internet",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			logging.SetDebug(debugFlag)
			if logJSONFlag {
				logging.SetJSON(true)
			}
		},
	}

	rootCmd.PersistentFlags().BoolVarP(&quietFlag, "quiet", "q", false, "suppress display output")
	rootCmd.PersistentFlags().BoolVar(&debugFlag, "debug", false, "verbose debug logging")
	rootCmd.PersistentFlags().BoolVar(&logJSONFlag, "log-json", false, "emit logs as JSON")

	resolveConfig := func() (serverURL, token string, err error) {
		cfg, _ := config.Load(config.DefaultPath())

		serverURL = serverFlag
		if serverURL == "" {
			serverURL = cfg.ServerURL
		}
		if serverURL == "" {
			return "", "", fmt.Errorf("server URL required: use --server or `tunnel config set-server <url>`")
		}
		serverURL = strings.TrimRight(serverURL, "/")

		switch {
		case strings.HasPrefix(serverURL, "http://"):
			serverURL = "ws://" + serverURL[7:]
		case strings.HasPrefix(serverURL, "https://"):
			serverURL = "wss://" + serverURL[8:]
		case !strings.HasPrefix(serverURL, "ws://") && !strings.HasPrefix(serverURL, "wss://"):
			serverURL = "wss://" + serverURL
		}

		token = tokenFlag
		if token == "" {
			token = cfg.AuthToken
		}
		return serverURL, token, nil
	}

	makeContext := func() (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sig
			cancel()
		}()
		return ctx, cancel
	}

	httpCmd := &cobra.Command{
		Use:   "http <port>",
		Short: "Create an HTTP tunnel to a local port",
		Example: `  tunnel http 3000
  tunnel http 8080 --name myapp
  tunnel http 5173 --inspect
  tunnel http 3000 --silent --name api`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			port, err := parsePort(args[0])
			if err != nil {
				return err
			}

			serverURL, token, err := resolveConfig()
			if err != nil {
				return err
			}

			ctx, cancel := makeContext()
			defer cancel()

			c := client.NewClient(serverURL, token, port)
			c.Inspect = inspectFlag
			c.Display.Quiet = quietFlag
			c.Name = nameFlag

			if err := maybeDaemonize(daemonFlags, os.Args, c, "http"); err != nil {
				return err
			}
			if shouldExitParent(daemonFlags) {
				return nil
			}
			attachIPC(c, daemonFlags)
			defer shutdownChildLogging()
			return c.Run(ctx)
		},
	}

	httpCmd.Flags().StringVar(&serverFlag, "server", "", "relay server URL")
	httpCmd.Flags().StringVar(&tokenFlag, "token", "", "auth token")
	httpCmd.Flags().BoolVar(&inspectFlag, "inspect", false, "enable web inspector at localhost:4040")
	httpCmd.Flags().StringVar(&nameFlag, "name", "", "request a stable tunnel name (e.g. 'eservice')")
	registerDaemonFlags(httpCmd, &daemonFlags)

	tcpCmd := &cobra.Command{
		Use:   "tcp <port>",
		Short: "Create a TCP tunnel to a local port",
		Example: `  tunnel tcp 5432              # expose Postgres
  tunnel tcp 22 --name ssh     # named SSH tunnel`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			port, err := parsePort(args[0])
			if err != nil {
				return err
			}

			serverURL, token, err := resolveConfig()
			if err != nil {
				return err
			}

			ctx, cancel := makeContext()
			defer cancel()

			c := client.NewClient(serverURL, token, port)
			c.Mode = "tcp"
			c.Display.Quiet = quietFlag
			c.Name = nameFlag

			if err := maybeDaemonize(daemonFlags, os.Args, c, "tcp"); err != nil {
				return err
			}
			if shouldExitParent(daemonFlags) {
				return nil
			}
			attachIPC(c, daemonFlags)
			defer shutdownChildLogging()
			return c.Run(ctx)
		},
	}

	tcpCmd.Flags().StringVar(&serverFlag, "server", "", "relay server URL")
	tcpCmd.Flags().StringVar(&tokenFlag, "token", "", "auth token")
	tcpCmd.Flags().StringVar(&nameFlag, "name", "", "request a stable tunnel name (e.g. 'eservice')")
	registerDaemonFlags(tcpCmd, &daemonFlags)

	udpCmd := &cobra.Command{
		Use:   "udp <port>",
		Short: "Create a UDP tunnel to a local port",
		Example: `  tunnel udp 53                # expose DNS
  tunnel udp 51820 --name wg   # named WireGuard tunnel`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			port, err := parsePort(args[0])
			if err != nil {
				return err
			}

			serverURL, token, err := resolveConfig()
			if err != nil {
				return err
			}

			ctx, cancel := makeContext()
			defer cancel()

			c := client.NewClient(serverURL, token, port)
			c.Mode = "udp"
			c.Display.Quiet = quietFlag
			c.Name = nameFlag

			if err := maybeDaemonize(daemonFlags, os.Args, c, "udp"); err != nil {
				return err
			}
			if shouldExitParent(daemonFlags) {
				return nil
			}
			attachIPC(c, daemonFlags)
			defer shutdownChildLogging()
			return c.Run(ctx)
		},
	}

	udpCmd.Flags().StringVar(&serverFlag, "server", "", "relay server URL")
	udpCmd.Flags().StringVar(&tokenFlag, "token", "", "auth token")
	udpCmd.Flags().StringVar(&nameFlag, "name", "", "request a stable tunnel name (e.g. 'eservice')")
	registerDaemonFlags(udpCmd, &daemonFlags)

	configCmd := &cobra.Command{
		Use:   "config",
		Short: "Manage tunnel configuration",
	}

	setServerCmd := &cobra.Command{
		Use:     "set-server <url>",
		Short:   "Set the relay server URL",
		Example: "  tunnel config set-server https://relay.example.com",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := validateServerURL(args[0]); err != nil {
				return err
			}
			path := config.DefaultPath()
			cfg, _ := config.Load(path)
			cfg.ServerURL = args[0]
			if err := config.Save(path, cfg); err != nil {
				return err
			}
			fmt.Printf("Server URL set to: %s\n", args[0])
			return nil
		},
	}

	setTokenCmd := &cobra.Command{
		Use:     "set-token <token>",
		Short:   "Set the auth token",
		Example: "  tunnel config set-token your-secret-token",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if strings.TrimSpace(args[0]) == "" {
				return fmt.Errorf("token must not be empty")
			}
			path := config.DefaultPath()
			cfg, _ := config.Load(path)
			cfg.AuthToken = args[0]
			if err := config.Save(path, cfg); err != nil {
				return err
			}
			fmt.Println("Auth token saved.")
			return nil
		},
	}

	showCmd := &cobra.Command{
		Use:   "show",
		Short: "Show current configuration",
		RunE: func(_ *cobra.Command, _ []string) error {
			path := config.DefaultPath()
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			fmt.Printf("config file: %s\n", path)
			if cfg.ServerURL == "" {
				fmt.Println("server_url:  (not set — run `tunnel config set-server <url>`)")
			} else {
				fmt.Printf("server_url:  %s\n", cfg.ServerURL)
			}
			switch {
			case cfg.AuthToken == "":
				fmt.Println("auth_token:  (not set)")
			case len(cfg.AuthToken) <= 4:
				fmt.Println("auth_token:  ***")
			default:
				fmt.Printf("auth_token:  %s***\n", cfg.AuthToken[:4])
			}
			return nil
		},
	}

	configCmd.AddCommand(setServerCmd, setTokenCmd, showCmd)

	doctorCmd := &cobra.Command{
		Use:     "doctor",
		Short:   "Diagnose tunnel setup (config, relay reachability, common issues)",
		Example: "  tunnel doctor",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runDoctor(serverFlag, tokenFlag)
		},
	}
	doctorCmd.Flags().StringVar(&serverFlag, "server", "", "relay server URL (overrides config)")
	doctorCmd.Flags().StringVar(&tokenFlag, "token", "", "auth token (overrides config)")

	listCmd := &cobra.Command{
		Use:     "list",
		Short:   "List tunnels currently registered on the relay",
		Example: "  tunnel list\n  tunnel list --json",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runList(serverFlag, tokenFlag, listJSONFlag)
		},
	}
	listCmd.Flags().StringVar(&serverFlag, "server", "", "relay server URL (overrides config)")
	listCmd.Flags().StringVar(&tokenFlag, "token", "", "auth token (overrides config)")
	listCmd.Flags().BoolVar(&listJSONFlag, "json", false, "emit machine-readable JSON")

	manCmd := &cobra.Command{
		Use:    "__gen-man <dir>",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return generateManPages(cmd.Root(), args[0])
		},
	}

	rootCmd.AddCommand(httpCmd, tcpCmd, udpCmd, configCmd, doctorCmd, listCmd, manCmd)
	registerDaemonCommands(rootCmd, &daemonFlags)

	if err := rootCmd.Execute(); err != nil {
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "tunnel:", err)
		}
		os.Exit(1)
	}
}
