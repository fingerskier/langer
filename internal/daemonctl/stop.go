package daemonctl

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fingerskier/langer/config"
	"github.com/fingerskier/langer/protocol"
)

// StopOptions configures Stop / StopAll.
type StopOptions struct {
	Logger *slog.Logger
	// Hard force-kills the daemon process tree when graceful drain fails or is
	// skipped after a failed dial (lock still held).
	Hard bool
	// Wait is how long to wait for graceful exit after drain. Zero = default.
	Wait time.Duration
	// Session id for the drain handshake.
	Session protocol.SessionID
}

const (
	defaultStopWait = 30 * time.Second
	stopPoll        = 50 * time.Millisecond
)

// StopResult is a terse outcome for one workspace daemon.
type StopResult struct {
	Root    string // may be empty when discovered only by socket
	Socket  string
	Lock    string
	PID     int
	Action  string // "drained", "already_stopped", "killed", "kill_failed", "drain_failed"
	Message string
}

// DialOnly connects to an existing daemon for root without auto-starting one.
func DialOnly(ctx context.Context, cfg *config.Config, root string, opts Options) (*Client, error) {
	if cfg == nil {
		return nil, protocol.NewError(protocol.ErrInternal, "daemonctl: a config is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	session := opts.Session
	if session == "" {
		session = "stop"
	}
	canonical, err := canonicalRoot(root)
	if err != nil {
		return nil, err
	}
	socket, err := cfg.WorkspaceSocketPath(canonical)
	if err != nil {
		return nil, protocol.NewErrorf(protocol.ErrInternal, "%v", err)
	}
	return dialAndHandshake(ctx, socket, canonical, session, log)
}

// Stop asks the daemon for root to drain (graceful). With Hard, force-kills
// the PID from the liveness lock if the daemon does not exit in time.
func Stop(ctx context.Context, cfg *config.Config, root string, opts StopOptions) (StopResult, error) {
	if cfg == nil {
		return StopResult{}, protocol.NewError(protocol.ErrInternal, "daemonctl: a config is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	wait := opts.Wait
	if wait <= 0 {
		wait = defaultStopWait
	}
	session := opts.Session
	if session == "" {
		session = "stop"
	}

	canonical, err := canonicalRoot(root)
	if err != nil {
		return StopResult{}, err
	}
	socket, err := cfg.WorkspaceSocketPath(canonical)
	if err != nil {
		return StopResult{}, protocol.NewErrorf(protocol.ErrInternal, "%v", err)
	}
	lockPath := cfg.WorkspaceLivenessLockPath(canonical)
	res := StopResult{Root: canonical, Socket: socket, Lock: lockPath, PID: readPID(lockPath)}

	client, err := dialAndHandshake(ctx, socket, canonical, session, log)
	if err != nil {
		// No listener: already stopped, unless a lock PID is still alive.
		if !opts.Hard {
			res.Action = "already_stopped"
			res.Message = "no daemon listening"
			return res, nil
		}
		if res.PID > 0 && processAlive(res.PID) {
			if err := killProcessTree(res.PID); err != nil {
				res.Action = "kill_failed"
				res.Message = err.Error()
				return res, err
			}
			res.Action = "killed"
			res.Message = fmt.Sprintf("force-killed pid %d (no socket)", res.PID)
			return res, nil
		}
		res.Action = "already_stopped"
		res.Message = "no daemon listening"
		return res, nil
	}
	defer client.Close()

	if err := client.requestDrain(ctx, session, "langer stop"); err != nil {
		if !opts.Hard {
			res.Action = "drain_failed"
			res.Message = err.Error()
			return res, err
		}
		// fall through to hard kill
	} else {
		if waitDaemonGone(ctx, socket, lockPath, wait) {
			res.Action = "drained"
			res.Message = "daemon drained"
			return res, nil
		}
		if !opts.Hard {
			res.Action = "drain_failed"
			res.Message = "daemon did not exit after drain"
			return res, fmt.Errorf("daemon for %s did not exit within %s", canonical, wait)
		}
	}

	pid := readPID(lockPath)
	if pid <= 0 {
		pid = res.PID
	}
	res.PID = pid
	if pid <= 0 {
		res.Action = "kill_failed"
		res.Message = "no pid in liveness lock"
		return res, fmt.Errorf("cannot force-kill daemon for %s: no pid", canonical)
	}
	if err := killProcessTree(pid); err != nil {
		res.Action = "kill_failed"
		res.Message = err.Error()
		return res, err
	}
	res.Action = "killed"
	res.Message = fmt.Sprintf("force-killed pid %d", pid)
	return res, nil
}

// StopAll stops every daemon with a socket or liveness lock under the runtime dir.
func StopAll(ctx context.Context, cfg *config.Config, opts StopOptions) ([]StopResult, error) {
	if cfg == nil {
		return nil, protocol.NewError(protocol.ErrInternal, "daemonctl: a config is required")
	}
	if _, err := cfg.EnsureRuntimeDir(); err != nil {
		return nil, err
	}
	runtimeDir := cfg.RuntimeDir()
	keys, err := listDaemonKeys(runtimeDir)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}

	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	session := opts.Session
	if session == "" {
		session = "stop"
	}
	wait := opts.Wait
	if wait <= 0 {
		wait = defaultStopWait
	}

	var out []StopResult
	var firstErr error
	for _, key := range keys {
		socket := filepath.Join(runtimeDir, "daemon-"+key+".sock")
		lockPath := filepath.Join(runtimeDir, "daemon-"+key+".lock")
		res := StopResult{Socket: socket, Lock: lockPath, PID: readPID(lockPath)}

		client, err := dialAndHandshake(ctx, socket, "", session, log)
		if err != nil {
			if opts.Hard && res.PID > 0 && processAlive(res.PID) {
				if kErr := killProcessTree(res.PID); kErr != nil {
					res.Action = "kill_failed"
					res.Message = kErr.Error()
					if firstErr == nil {
						firstErr = kErr
					}
				} else {
					res.Action = "killed"
					res.Message = fmt.Sprintf("force-killed pid %d", res.PID)
				}
			} else {
				res.Action = "already_stopped"
				res.Message = "no daemon listening"
			}
			out = append(out, res)
			continue
		}
		root := client.Root()
		res.Root = root
		_ = client.requestDrain(ctx, session, "langer stop --all")
		_ = client.Close()

		if waitDaemonGone(ctx, socket, lockPath, wait) {
			res.Action = "drained"
			res.Message = "daemon drained"
			out = append(out, res)
			continue
		}
		if opts.Hard {
			pid := readPID(lockPath)
			if pid <= 0 {
				pid = res.PID
			}
			res.PID = pid
			if pid > 0 {
				if kErr := killProcessTree(pid); kErr != nil {
					res.Action = "kill_failed"
					res.Message = kErr.Error()
					if firstErr == nil {
						firstErr = kErr
					}
				} else {
					res.Action = "killed"
					res.Message = fmt.Sprintf("force-killed pid %d", pid)
				}
			} else {
				res.Action = "kill_failed"
				res.Message = "no pid"
				if firstErr == nil {
					firstErr = fmt.Errorf("no pid for %s", socket)
				}
			}
		} else {
			res.Action = "drain_failed"
			res.Message = "daemon did not exit after drain"
			if firstErr == nil {
				firstErr = fmt.Errorf("daemon on %s did not exit", socket)
			}
		}
		out = append(out, res)
	}
	return out, firstErr
}

// NukeForce kills every langer process image it can find (daemons and mcp).
// Use only as last resort; active MCP sessions die.
func NukeForce() (killed int, err error) {
	return killAllLangerProcesses()
}

func listDaemonKeys(runtimeDir string) ([]string, error) {
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	seen := map[string]struct{}{}
	var keys []string
	for _, e := range entries {
		name := e.Name()
		var key string
		switch {
		case strings.HasPrefix(name, "daemon-") && strings.HasSuffix(name, ".sock"):
			key = strings.TrimSuffix(strings.TrimPrefix(name, "daemon-"), ".sock")
		case strings.HasPrefix(name, "daemon-") && strings.HasSuffix(name, ".lock"):
			key = strings.TrimSuffix(strings.TrimPrefix(name, "daemon-"), ".lock")
		default:
			continue
		}
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys, nil
}

func readPID(lockPath string) int {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return 0
	}
	line := strings.TrimSpace(string(data))
	if line == "" {
		return 0
	}
	// first token only
	if i := strings.IndexAny(line, "\r\n \t"); i >= 0 {
		line = line[:i]
	}
	pid, err := strconv.Atoi(line)
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func waitDaemonGone(ctx context.Context, socket, lockPath string, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if !socketAlive(socket) {
			pid := readPID(lockPath)
			if pid <= 0 || !processAlive(pid) {
				return true
			}
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(stopPoll):
		}
	}
}

func socketAlive(socket string) bool {
	if _, err := os.Stat(socket); err != nil {
		return false
	}
	// Best-effort: if the file exists but nothing listens, still treat as gone
	// after dial fails — Stat alone is enough for our wait loop together with PID.
	return true
}
