// SPDX-License-Identifier: AGPL-3.0-or-later
//
// SSH transport for the handful of OPNsense settings that have NO REST API:
// the config.xml <system> general settings (hostname, domain, timezone, DNS
// servers, timeservers/NTP). OPNsense exposes these only through its PHP config
// framework (config_read_array + write_config + the *_configure() functions),
// reachable over SSH — there is no /api endpoint (core/system/getSettings 404s).
// The opnsense_system_config resource drives them through this transport so the
// consuming module no longer needs the generic scottwinkler/shell provider.
//
// Like the sibling tofu-ddwrt/tomato providers, we invoke the system `ssh`
// binary via os/exec rather than an in-process SSH library — this keeps go.mod
// unchanged (no golang.org/x/crypto/ssh) and reuses the lab's existing SSH
// machinery (OpenBao-signed certs / agent / ssh_config). A private key is only
// ever materialized when key_pem is supplied (written to a temp 0600 file per
// call, removed after); key_file and ssh_config paths never touch the material.
package opnsense

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// SSHConfig configures the SSH transport. All fields optional except Host.
type SSHConfig struct {
	Host    string // host or host:port, no scheme
	Port    int    // SSH port (0 → use Host's :port or ssh default)
	User    string // SSH user (default "root")
	KeyFile string // identity file (ssh -i); ssh_config/agent used when empty
	KeyPEM  string // private-key material (e.g. from OpenBao); temp 0600 file per call
	Timeout time.Duration
}

// SSHClient runs remote commands on an OPNsense box over SSH. Safe for
// concurrent use; each call spawns its own ssh process (multiplexed via
// ControlMaster). nil when the provider was configured without SSH.
type SSHClient struct {
	addr    string
	port    string
	user    string
	keyFile string
	keyPEM  string
	timeout time.Duration
}

// NewSSHClient builds an SSHClient. It does not contact the host until first use.
func NewSSHClient(c SSHConfig) *SSHClient {
	if c.Timeout == 0 {
		c.Timeout = 45 * time.Second
	}
	user := c.User
	if user == "" {
		user = "root"
	}
	addr, port := splitHostPort(c.Host)
	if c.Port != 0 {
		port = strconv.Itoa(c.Port)
	}
	return &SSHClient{addr: addr, port: port, user: user, keyFile: c.KeyFile, keyPEM: c.KeyPEM, timeout: c.Timeout}
}

func splitHostPort(h string) (string, string) {
	h = strings.TrimSpace(h)
	h = strings.TrimPrefix(h, "ssh://")
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i+1:], "]") {
		if _, err := strconv.Atoi(h[i+1:]); err == nil {
			return h[:i], h[i+1:]
		}
	}
	return h, ""
}

// SSHError is returned when an ssh invocation exits non-zero.
type SSHError struct {
	Cmd      string
	ExitCode int
	Stderr   string
}

func (e *SSHError) Error() string {
	return fmt.Sprintf("opnsense ssh %q: exit %d: %s", e.Cmd, e.ExitCode, strings.TrimSpace(e.Stderr))
}

const maxSSHAttempts = 4

func transientSSH(err error) bool {
	var se *SSHError
	if !errors.As(err, &se) {
		return false
	}
	return strings.Contains(se.Stderr, "kex_exchange_identification") ||
		strings.Contains(se.Stderr, "Connection reset by") ||
		strings.Contains(se.Stderr, "Connection closed by")
}

// Run executes a remote command (handed to the box's shell), retrying transient
// connection resets, and returns stdout. stdin (may be nil) is piped to the
// command — used to feed a PHP script to `php` without shell-quoting it.
func (c *SSHClient) Run(remote string, stdin []byte) ([]byte, error) {
	var (
		out []byte
		err error
	)
	for attempt := 1; attempt <= maxSSHAttempts; attempt++ {
		out, err = c.runOnce(remote, stdin)
		if err == nil || attempt == maxSSHAttempts || !transientSSH(err) {
			return out, err
		}
		time.Sleep(time.Duration(attempt) * 250 * time.Millisecond)
	}
	return out, err
}

func (c *SSHClient) runOnce(remote string, stdin []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	args := []string{
		"-o", "BatchMode=yes",
		// Relay-forwarded lab boxes present varying host keys and the runner's
		// known_hosts may be unwritable — discard host-key state (matches the
		// shell tier this replaced) so a key change never blocks the connection.
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", fmt.Sprintf("ConnectTimeout=%d", connectTimeoutSeconds(c.timeout)),
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=/tmp/opnsense-cm-%C",
		"-o", "ControlPersist=20s",
	}
	if c.port != "" {
		args = append(args, "-p", c.port)
	}
	keyPath, cleanup, kerr := c.identityFile()
	if kerr != nil {
		return nil, fmt.Errorf("opnsense: materialize ssh identity: %w", kerr)
	}
	defer cleanup()
	if keyPath != "" {
		args = append(args, "-i", keyPath, "-o", "IdentitiesOnly=yes")
	}
	target := c.addr
	if c.user != "" {
		target = c.user + "@" + c.addr
	}
	args = append(args, target, remote)

	cmd := exec.CommandContext(ctx, "ssh", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("opnsense ssh %q: timed out after %s", remote, c.timeout)
		}
		return nil, &SSHError{Cmd: remote, ExitCode: code, Stderr: stderr.String()}
	}
	return stdout.Bytes(), nil
}

// identityFile resolves the SSH identity. An explicit keyFile wins (no cleanup).
// Else keyPEM is written to a temp 0600 file (cleaned up after). Neither → ssh
// falls back to ssh_config/agent.
func (c *SSHClient) identityFile() (path string, cleanup func(), err error) {
	noop := func() {}
	if c.keyFile != "" {
		return c.keyFile, noop, nil
	}
	if strings.TrimSpace(c.keyPEM) == "" {
		return "", noop, nil
	}
	f, err := os.CreateTemp("", "opnsense-key-*")
	if err != nil {
		return "", noop, err
	}
	name := f.Name()
	fail := func(e error) (string, func(), error) { _ = f.Close(); _ = os.Remove(name); return "", noop, e }
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := f.WriteString(strings.TrimRight(c.keyPEM, "\n") + "\n"); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", noop, err
	}
	return name, func() { _ = os.Remove(name) }, nil
}

func connectTimeoutSeconds(d time.Duration) int {
	s := int(d.Seconds()) / 2
	if s < 1 {
		s = 1
	}
	return s
}
