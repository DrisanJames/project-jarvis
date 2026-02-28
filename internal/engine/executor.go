package engine

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Executor translates agent decisions into PMTA actions via SSH.
// Batches config changes to avoid excessive reloads
// (max 1 reload per 30 seconds).
type Executor struct {
	pmtaHost     string
	pmtaPort     int
	sshUser      string
	sshKeyPath   string
	sshClient    *ssh.Client

	mu            sync.Mutex
	pendingReload bool
	lastReload    time.Time
	reloadMinGap  time.Duration
}

// NewExecutor creates a new PMTA command executor.
// Pass an empty host to operate in dry-run mode.
func NewExecutor(host string, port int, user, sshKeyPath string) *Executor {
	return &Executor{
		pmtaHost:     host,
		pmtaPort:     port,
		sshUser:      user,
		sshKeyPath:   sshKeyPath,
		reloadMinGap: 30 * time.Second,
	}
}

// Execute processes a decision and sends the appropriate PMTA command.
func (e *Executor) Execute(ctx context.Context, d Decision) error {
	switch d.ActionTaken {
	case "disable_source_ip":
		return e.disableSource(ctx, d.TargetValue, string(d.ISP))
	case "quarantine_ip":
		return e.disableSource(ctx, d.TargetValue, string(d.ISP))
	case "pause_isp_queues":
		return e.pauseQueues(ctx, d.ISP)
	case "emergency_halt":
		return e.emergencyHalt(ctx, d.ISP)
	case "reduce_rate", "backoff_mode":
		return e.setBackoffMode(ctx, d.ISP)
	case "increase_rate":
		return e.setNormalMode(ctx, d.ISP)
	case "snap_to_stable_rate":
		return e.triggerReload(ctx)
	case "pause_warmup":
		return e.pauseQueues(ctx, d.ISP)
	case "advance_warmup_day":
		return e.triggerReload(ctx)
	default:
		log.Printf("[executor] unknown action: %s", d.ActionTaken)
		return nil
	}
}

func (e *Executor) ensureSSH() (*ssh.Client, error) {
	if e.sshClient != nil {
		// Quick liveness check
		_, _, err := e.sshClient.SendRequest("keepalive@openssh.com", true, nil)
		if err == nil {
			return e.sshClient, nil
		}
		e.sshClient.Close()
		e.sshClient = nil
	}

	keyBytes, err := os.ReadFile(e.sshKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read SSH key %s: %w", e.sshKeyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse SSH key: %w", err)
	}

	config := &ssh.ClientConfig{
		User:            e.sshUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", e.pmtaHost, e.pmtaPort)
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, fmt.Errorf("SSH dial %s: %w", addr, err)
	}
	e.sshClient = client
	return client, nil
}

func (e *Executor) sendCommand(ctx context.Context, command string) error {
	if e.pmtaHost == "" {
		log.Printf("[executor] dry-run: %s", command)
		return nil
	}

	client, err := e.ensureSSH()
	if err != nil {
		return fmt.Errorf("SSH connect: %w", err)
	}

	session, err := client.NewSession()
	if err != nil {
		// Connection may have broken; reset and retry once
		e.sshClient = nil
		client, err = e.ensureSSH()
		if err != nil {
			return fmt.Errorf("SSH reconnect: %w", err)
		}
		session, err = client.NewSession()
		if err != nil {
			return fmt.Errorf("SSH session: %w", err)
		}
	}
	defer session.Close()

	// Wrap with sudo since pmta CLI needs root
	fullCmd := fmt.Sprintf("sudo /usr/sbin/%s", command)
	output, err := session.CombinedOutput(fullCmd)
	if err != nil {
		return fmt.Errorf("pmta command '%s': %s (output: %s)", command, err, string(output))
	}

	log.Printf("[executor] command OK: %s → %s", command, string(output))
	return nil
}

func (e *Executor) disableSource(ctx context.Context, ip string, domain string) error {
	cmd := fmt.Sprintf("pmta disable source %s %s/*", ip, domain)
	return e.sendCommand(ctx, cmd)
}

func (e *Executor) pauseQueues(ctx context.Context, isp ISP) error {
	cmd := fmt.Sprintf("pmta pause queue */%s-pool", isp)
	return e.sendCommand(ctx, cmd)
}

func (e *Executor) emergencyHalt(ctx context.Context, isp ISP) error {
	if err := e.pauseQueues(ctx, isp); err != nil {
		return err
	}
	cmd := fmt.Sprintf("pmta disable source * */%s-pool", isp)
	return e.sendCommand(ctx, cmd)
}

func (e *Executor) setBackoffMode(ctx context.Context, isp ISP) error {
	cmd := fmt.Sprintf("pmta set queue --mode=backoff */%s-pool", isp)
	return e.sendCommand(ctx, cmd)
}

func (e *Executor) setNormalMode(ctx context.Context, isp ISP) error {
	cmd := fmt.Sprintf("pmta set queue --mode=normal */%s-pool", isp)
	return e.sendCommand(ctx, cmd)
}

func (e *Executor) triggerReload(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if time.Since(e.lastReload) < e.reloadMinGap {
		e.pendingReload = true
		return nil
	}

	if err := e.sendCommand(ctx, "pmta reload"); err != nil {
		return err
	}
	e.lastReload = time.Now()
	e.pendingReload = false
	return nil
}

// StartReloadLoop processes deferred reloads.
func (e *Executor) StartReloadLoop(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.mu.Lock()
				shouldReload := e.pendingReload && time.Since(e.lastReload) >= e.reloadMinGap
				e.mu.Unlock()
				if shouldReload {
					e.triggerReload(ctx)
				}
			}
		}
	}()
}

// ResumeAll resumes all queues and re-enables all sources (manual override).
func (e *Executor) ResumeAll(ctx context.Context) error {
	if err := e.sendCommand(ctx, "pmta resume queue */*"); err != nil {
		return err
	}
	return e.sendCommand(ctx, "pmta enable source * */*")
}

// ResumeISP resumes queues for a specific ISP.
func (e *Executor) ResumeISP(ctx context.Context, isp ISP) error {
	cmd := fmt.Sprintf("pmta resume queue */%s-pool", isp)
	if err := e.sendCommand(ctx, cmd); err != nil {
		return err
	}
	cmd = fmt.Sprintf("pmta enable source * */%s-pool", isp)
	return e.sendCommand(ctx, cmd)
}

// SCPFile copies a local file to the PMTA server via SSH/SFTP.
func (e *Executor) SCPFile(localPath, remotePath string) error {
	if e.pmtaHost == "" {
		log.Printf("[executor] dry-run: scp %s -> %s", localPath, remotePath)
		return nil
	}

	client, err := e.ensureSSH()
	if err != nil {
		return err
	}

	data, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("read local file: %w", err)
	}

	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	// Write via stdin to a temp file, then move atomically
	tmpPath := remotePath + ".tmp"
	cmd := fmt.Sprintf("sudo tee %s > /dev/null && sudo mv %s %s", tmpPath, tmpPath, remotePath)
	session.Stdin = bytes.NewReader(data)
	if err := session.Run(cmd); err != nil {
		return fmt.Errorf("scp %s: %w", remotePath, err)
	}
	return nil
}

// Close shuts down the SSH connection.
func (e *Executor) Close() {
	if e.sshClient != nil {
		e.sshClient.Close()
		e.sshClient = nil
	}
}
