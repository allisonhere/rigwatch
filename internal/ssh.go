package internal

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/allisonhere/rigwatch/internal/gpu"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

var commandTimeout = 10 * time.Second

type SSHHost struct {
	Name         string
	Hostname     string
	User         string
	Port         string
	IdentityFile string
	Local        bool
	Managed      bool

	// PasswordAuth marks a host that authenticates with a password instead of a
	// key. It is persisted as `PreferredAuthentications password` in the managed
	// config so the system ssh client honors it too.
	PasswordAuth bool

	// Password is the in-memory password for a PasswordAuth host, supplied at
	// connect time. It is never serialized to disk or parsed from config.
	Password string
}

type SSHClient struct {
	client *ssh.Client
	config *SSHHost
}

func ParseSSHConfig(configPath string) ([]SSHHost, error) {
	if configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		configPath = filepath.Join(home, ".ssh", "config")
	}

	visited := make(map[string]bool)
	return parseSSHConfigRecursive(configPath, visited)
}

func EnsureLocalhost(hosts []SSHHost) []SSHHost {
	for _, host := range hosts {
		if host.Name == "localhost" {
			return hosts
		}
	}

	local := SSHHost{
		Name:     "localhost",
		Hostname: "localhost",
		User:     getValidatedUsername(),
		Local:    true,
	}
	return append([]SSHHost{local}, hosts...)
}

func parseSSHConfigRecursive(configPath string, visited map[string]bool) ([]SSHHost, error) {
	absPath, err := filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}

	if visited[absPath] {
		return nil, nil
	}
	visited[absPath] = true

	file, err := os.Open(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	var hosts []SSHHost
	var currentHost *SSHHost

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}

		key := strings.ToLower(parts[0])
		value := strings.Join(parts[1:], " ")

		if key == "include" {
			includePath := value
			if strings.HasPrefix(includePath, "~/") || filepath.IsAbs(includePath) {
				includePath = expandPath(includePath)
			}

			if !filepath.IsAbs(includePath) {
				configDir := filepath.Dir(configPath)
				includePath = filepath.Join(configDir, includePath)
			}

			matches, err := filepath.Glob(includePath)
			if err != nil {
				continue
			}

			for _, match := range matches {
				includedHosts, err := parseSSHConfigRecursive(match, visited)
				if err != nil {
					continue
				}
				hosts = append(hosts, includedHosts...)
			}
			continue
		}

		if key == "host" {
			if currentHost != nil {
				hosts = append(hosts, concreteHosts(*currentHost)...)
			}

			currentHost = &SSHHost{
				Name: value,
				Port: "22",
			}
		} else if currentHost != nil {
			switch key {
			case "hostname":
				currentHost.Hostname = value
			case "user":
				currentHost.User = value
			case "port":
				currentHost.Port = value
			case "identityfile":
				currentHost.IdentityFile = expandPath(value)
			case "preferredauthentications":
				currentHost.PasswordAuth = strings.Contains(strings.ToLower(value), "password")
			}
		}
	}

	if currentHost != nil {
		hosts = append(hosts, concreteHosts(*currentHost)...)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return hosts, nil
}

func concreteHosts(host SSHHost) []SSHHost {
	patterns := strings.Fields(host.Name)
	if len(patterns) == 0 {
		return nil
	}

	hosts := make([]SSHHost, 0, len(patterns))
	for _, pattern := range patterns {
		if strings.Contains(pattern, "*") || strings.Contains(pattern, "?") {
			continue
		}

		h := host
		h.Name = pattern
		hosts = append(hosts, h)
	}

	return hosts
}

func expandPath(path string) string {
	if path == "" {
		return ""
	}

	path = filepath.Clean(path)

	if strings.Contains(path, "..") {
		return ""
	}

	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		expandedPath := filepath.Join(home, path[2:])

		absHome, err := filepath.Abs(home)
		if err != nil {
			return ""
		}
		absPath, err := filepath.Abs(expandedPath)
		if err != nil {
			return ""
		}

		if !strings.HasPrefix(absPath, absHome) {
			return ""
		}

		return expandedPath
	}

	if filepath.IsAbs(path) {
		home, _ := os.UserHomeDir()
		sshDir := filepath.Join(home, ".ssh")

		absPath, err := filepath.Abs(path)
		if err != nil {
			return ""
		}

		if strings.HasPrefix(absPath, sshDir) || strings.HasPrefix(absPath, "/etc/ssh") {
			return path
		}

		return ""
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh", path)
}

func getHostKeyCallback() (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("unable to get user home directory: %w", err)
	}

	knownHostsPath := filepath.Join(home, ".ssh", "known_hosts")

	if _, err := os.Stat(knownHostsPath); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(knownHostsPath), 0700); err != nil {
			return nil, fmt.Errorf("unable to create .ssh directory: %w", err)
		}
		if _, err := os.Create(knownHostsPath); err != nil {
			return nil, fmt.Errorf("unable to create known_hosts file: %w", err)
		}
	}

	hostKeyCallback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("unable to load known_hosts: %w", err)
	}

	return ssh.HostKeyCallback(func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := hostKeyCallback(hostname, remote, key)
		if err != nil {
			if keyErr, ok := err.(*knownhosts.KeyError); ok && len(keyErr.Want) > 0 {
				return fmt.Errorf("host key verification failed: host key has changed for %s. Remove the old key from %s if you trust this connection", hostname, knownHostsPath)
			} else if keyErr, ok := err.(*knownhosts.KeyError); ok && len(keyErr.Want) == 0 {
				return fmt.Errorf("host key verification failed: %s is not in known_hosts. Add the host key to %s or run 'ssh %s' first to accept the host key", hostname, knownHostsPath, hostname)
			}
			return fmt.Errorf("host key verification failed: %w", err)
		}
		return nil
	}), nil
}

func getValidatedUsername() string {
	user := os.Getenv("USER")
	if user == "" {
		return ""
	}

	if len(user) > 32 {
		return ""
	}

	for _, char := range user {
		if !((char >= 'a' && char <= 'z') ||
			(char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') ||
			char == '_' || char == '-' || char == '.') {
			return ""
		}
	}

	return user
}

func getValidatedSSHAuthSock() string {
	socket := os.Getenv("SSH_AUTH_SOCK")
	if socket == "" {
		return ""
	}

	if !filepath.IsAbs(socket) {
		return ""
	}

	cleanSocket := filepath.Clean(socket)
	if strings.Contains(cleanSocket, "..") {
		return ""
	}

	validPrefixes := []string{
		"/tmp/",
		"/var/run/",
		"/run/",
	}

	if tmpDir := os.Getenv("TMPDIR"); tmpDir != "" {
		validPrefixes = append(validPrefixes, tmpDir)
	}

	for _, prefix := range validPrefixes {
		if strings.HasPrefix(cleanSocket, prefix) {
			return socket
		}
	}

	if info, err := os.Stat(socket); err == nil {
		if info.Mode()&os.ModeSocket != 0 {
			return socket
		}
	}

	return ""
}

func NewSSHClient(host SSHHost) (*SSHClient, error) {
	if host.Local {
		if host.Hostname == "" {
			host.Hostname = "localhost"
		}
		if host.User == "" {
			host.User = getValidatedUsername()
		}
		return &SSHClient{config: &host}, nil
	}

	if host.Hostname == "" {
		host.Hostname = host.Name
	}
	if host.User == "" {
		host.User = getValidatedUsername()
	}
	if host.Port == "" {
		host.Port = "22"
	}

	var authMethods []ssh.AuthMethod

	// Password auth: when a password was supplied for this host it takes priority,
	// so a password-auth host doesn't stall trying keys the server will reject.
	if host.Password != "" {
		authMethods = append(authMethods, passwordAuth(host.Password)...)
	}

	// First, try the specific identity file from SSH config (highest priority)
	if host.IdentityFile != "" {
		if keyAuth, err := publicKeyAuth(host.IdentityFile); err == nil {
			authMethods = append(authMethods, keyAuth)
		}
	}

	// Second, try SSH agent (if available)
	if agentAuth, agentErr := sshAgentAuth(); agentErr == nil {
		authMethods = append(authMethods, agentAuth)
	}

	// Finally, try default key files as fallback
	home, err := os.UserHomeDir()
	if err == nil {
		defaultKeys := []string{
			filepath.Join(home, ".ssh", "id_rsa"),
			filepath.Join(home, ".ssh", "id_ed25519"),
			filepath.Join(home, ".ssh", "id_ecdsa"),
		}
		for _, keyPath := range defaultKeys {
			// Skip if this is the same as the identity file we already tried
			if keyPath != host.IdentityFile {
				if keyAuth, err := publicKeyAuth(keyPath); err == nil {
					authMethods = append(authMethods, keyAuth)
				}
			}
		}
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no authentication methods available")
	}

	hostKeyCallback, err := getHostKeyCallback()
	if err != nil {
		return nil, fmt.Errorf("failed to setup host key verification: %w", err)
	}

	config := &ssh.ClientConfig{
		User:            host.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         10 * time.Second,
	}

	addr := fmt.Sprintf("%s:%s", host.Hostname, host.Port)
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", addr, err)
	}

	return &SSHClient{
		client: client,
		config: &host,
	}, nil
}

// passwordAuth returns the password-based auth methods for a host: the plain
// password method plus a keyboard-interactive responder that answers every
// prompt with the same password (covering servers that only offer the latter).
func passwordAuth(password string) []ssh.AuthMethod {
	ki := ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
		answers := make([]string, len(questions))
		for i := range answers {
			answers[i] = password
		}
		return answers, nil
	})
	return []ssh.AuthMethod{ssh.Password(password), ki}
}

func publicKeyAuth(keyPath string) (ssh.AuthMethod, error) {
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		// if it's an encrypted key error, we can't handle it without a passphrase
		// just return the error - SSH agent should handle these keys
		return nil, err
	}

	return ssh.PublicKeys(signer), nil
}

func sshAgentAuth() (ssh.AuthMethod, error) {
	socket := getValidatedSSHAuthSock()
	if socket == "" {
		return nil, fmt.Errorf("SSH_AUTH_SOCK not set or invalid")
	}

	conn, err := net.Dial("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to SSH agent: %w", err)
	}

	agentClient := agent.NewClient(conn)

	return ssh.PublicKeysCallback(agentClient.Signers), nil
}

func isAllowedCommand(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	allowedExact := []string{
		"cat /proc/net/dev",
		"cat /proc/loadavg",
		"cat /proc/diskstats",
		"cat /proc/uptime",
		"grep -H . /sys/class/thermal/thermal_zone*/type /sys/class/thermal/thermal_zone*/temp",
		"grep -H . /sys/class/hwmon/hwmon*/temp*_label /sys/class/hwmon/hwmon*/temp*_input 2>/dev/null || true",
		gpu.DRMProbeCommand,
		"find -L /sys/class/hwmon -maxdepth 2 \\( -name 'temp*_input' -o -name 'temp*_label' \\) -exec grep -H . {} + 2>/dev/null || true",
		"find -L /sys/class/hwmon -maxdepth 2 \\( -name 'fan*_input' -o -name 'fan*_label' \\) -exec grep -H . {} + 2>/dev/null || true",
		"ps -eo pid=,comm=,pcpu=,pmem= --sort=-pcpu | head -n 25",
		gpu.DRMProbeCommand,
	}
	for _, allowed := range allowedExact {
		if cmd == allowed {
			return true
		}
	}

	allowedPrefixes := []string{
		"lscpu ",
		"top -",
		"which ",
		"nvidia-smi ",
		"amd-smi ",
		"rocm-smi ",
		"free -",
		"df -",
		"cat /sys/class/drm/",
		"lspci ",
	}

	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(cmd, prefix) {
			return true
		}
	}

	return false
}

func (c *SSHClient) ExecuteCommand(cmd string) (string, error) {
	if !isAllowedCommand(cmd) {
		return "", fmt.Errorf("command not in allowed list: %s", cmd)
	}

	if c.config != nil && c.config.Local {
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()

		localCmd := exec.CommandContext(ctx, "sh", "-c", cmd)
		output, err := localCmd.CombinedOutput()
		if ctx.Err() == context.DeadlineExceeded {
			return string(output), fmt.Errorf("command timed out after %s: %s", commandTimeout, cmd)
		}
		if err != nil {
			return string(output), err
		}
		return string(output), nil
	}

	session, err := c.client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	type commandResult struct {
		output []byte
		err    error
	}
	done := make(chan commandResult, 1)
	go func() {
		output, err := session.CombinedOutput(cmd)
		done <- commandResult{output: output, err: err}
	}()

	select {
	case result := <-done:
		if result.err != nil {
			return string(result.output), result.err
		}
		return string(result.output), nil
	case <-time.After(commandTimeout):
		_ = session.Close()
		return "", fmt.Errorf("command timed out after %s: %s", commandTimeout, cmd)
	}
}

func (c *SSHClient) Close() error {
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}
