package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/allisonhere/rigwatch/internal"
	"github.com/allisonhere/rigwatch/internal/ui"
	"github.com/allisonhere/rigwatch/internal/web"
	tea "github.com/charmbracelet/bubbletea"
)

func validateInterval(seconds float64) time.Duration {
	if seconds < 0.01 || seconds > 3600 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

func main() {
	var showVersion bool
	var webMode bool
	var webPort int

	flag.Usage = func() {
		// HACK: make it look like python's argparse
		fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS] [HOST...]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Options:\n")
		fmt.Fprintf(os.Stderr, "  -n, --interval float  Update interval in seconds (default: 5, or SSH_DASHBOARD_INTERVAL env var)\n")
		fmt.Fprintf(os.Stderr, "  -v, --version         Show version information\n")
		fmt.Fprintf(os.Stderr, "  -w, --web             Start web dashboard server (no TUI)\n")
		fmt.Fprintf(os.Stderr, "  -p, --port int        Web server port (default: 8080, used with --web)\n")
		fmt.Fprintf(os.Stderr, "  -h, --help            Show this help message\n")
		fmt.Fprintf(os.Stderr, "\nArguments:\n")
		fmt.Fprintf(os.Stderr, "  HOST...               One or more hostnames from SSH config to connect to directly\n")
		fmt.Fprintf(os.Stderr, "                        Example: rigwatch myHost myOtherHost --web\n")
	}

	var updateIntervalVal float64
	flag.Float64Var(&updateIntervalVal, "n", 0, "")
	flag.Float64Var(&updateIntervalVal, "interval", 0, "")
	flag.BoolVar(&showVersion, "v", false, "")
	flag.BoolVar(&showVersion, "version", false, "")
	flag.BoolVar(&webMode, "w", false, "")
	flag.BoolVar(&webMode, "web", false, "")
	flag.IntVar(&webPort, "p", 8080, "")
	flag.IntVar(&webPort, "port", 8080, "")
	flag.Parse()

	requestedHosts := flag.Args()

	if showVersion {
		fmt.Printf("rigwatch version %s\n", internal.FullVersion())
		fmt.Printf("  git commit: %s\n", internal.GitCommit)
		fmt.Printf("  build date: %s\n", internal.BuildDate)
		fmt.Printf("  git tag:    %s\n", internal.GitTag)
		os.Exit(0)
	}

	updateInterval := &updateIntervalVal

	interval := 5 * time.Second

	if *updateInterval > 0 {
		if validated := validateInterval(*updateInterval); validated > 0 {
			interval = validated
		}
	} else if envInterval := os.Getenv("SSH_DASHBOARD_INTERVAL"); envInterval != "" {
		if seconds, err := strconv.ParseFloat(envInterval, 64); err == nil {
			if validated := validateInterval(seconds); validated > 0 {
				interval = validated
			}
		}
	}

	// Bootstrap the rigwatch-managed config Include so connection-manager
	// edits are visible to both rigwatch and the system ssh client.
	if err := internal.EnsureIncludeDirective(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not set up managed SSH config: %v\n", err)
	}

	hosts, err := internal.LoadAllHosts()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing SSH config: %v\n", err)
		os.Exit(1)
	}

	if len(hosts) == 0 {
		fmt.Fprintf(os.Stderr, "No hosts found in SSH config\n")
		os.Exit(1)
	}

	var initialModel ui.Model

	if len(requestedHosts) > 0 {
		var selectedHosts []internal.SSHHost
		hostMap := make(map[string]internal.SSHHost)

		for _, host := range hosts {
			hostMap[host.Name] = host
		}

		for _, requestedName := range requestedHosts {
			if host, found := hostMap[requestedName]; found {
				selectedHosts = append(selectedHosts, host)
			} else {
				fmt.Fprintf(os.Stderr, "Host '%s' not found in SSH config\n", requestedName)
				os.Exit(1)
			}
		}

		if webMode {
			svr := web.NewServer(selectedHosts, interval, webPort)
			if err := svr.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "Error starting web server: %v\n", err)
				os.Exit(1)
			}
			return
		}

		initialModel = ui.InitialModelWithHosts(hosts, selectedHosts, interval)
	} else {
		if webMode {
			// Web mode with no specific hosts: monitor all
			svr := web.NewServer(hosts, interval, webPort)
			if err := svr.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "Error starting web server: %v\n", err)
				os.Exit(1)
			}
			return
		}
		initialModel = ui.InitialModel(hosts, interval)
	}

	p := tea.NewProgram(initialModel, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error running program: %v\n", err)
		os.Exit(1)
	}

	if m, ok := finalModel.(ui.Model); ok {
		if sshHost := m.GetSSHOnExit(); sshHost != "" {
			sshPath, err := exec.LookPath("ssh")
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error finding ssh: %v\n", err)
				os.Exit(1)
			}

			args := []string{"ssh", sshHost}
			env := os.Environ()

			err = syscall.Exec(sshPath, args, env)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error executing ssh: %v\n", err)
				os.Exit(1)
			}
		}
	}
}
