package source

import (
	"bufio"
	"context"
	"io"
	"os/exec"
	"syscall"
	"time"

	"github.com/jwarykowski/drover/loop"
)

// ExecSource is the local transport: it spawns a command and reads NDJSON
// events from its stdout. The process is the plugin — any language, no linking,
// no ABI. It reconnects when the stream drops (the plugin crashed, or exited on
// purpose) and applies backpressure through an unbuffered channel, so a slow
// loop pauses the reader, which pauses the plugin's writes.
//
// The plugin's stderr is inherited, so its own diagnostics reach the operator
// without being mistaken for events.
type ExecSource struct {
	Name    string        // config source name; the default Event.Source
	Cmd     []string      // argv, from the [[source]] row
	Backoff time.Duration // wait before respawning; 0 defaults to 1s
	Stderr  io.Writer     // plugin diagnostics sink; nil discards
	Logf    func(string, ...any)
}

func (s ExecSource) backoff() time.Duration {
	if s.Backoff <= 0 {
		return time.Second
	}
	return s.Backoff
}

func (s ExecSource) logf(format string, a ...any) {
	if s.Logf != nil {
		s.Logf(format, a...)
	}
}

// Events runs the plugin until ctx is cancelled, respawning on exit.
func (s ExecSource) Events(ctx context.Context) <-chan loop.Event {
	out := make(chan loop.Event) // unbuffered: the consumer paces the producer
	go func() {
		defer close(out)
		if len(s.Cmd) == 0 {
			s.logf("source %q has no cmd", s.Name)
			return
		}
		for ctx.Err() == nil {
			err := s.stream(ctx, out)
			if ctx.Err() != nil {
				return
			}
			s.logf("source %q ended (%v); restarting in %s", s.Name, err, s.backoff())
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.backoff()):
			}
		}
	}()
	return out
}

// stream runs one plugin process and pumps its output until it exits or ctx is
// cancelled.
func (s ExecSource) stream(ctx context.Context, out chan<- loop.Event) error {
	cmd := exec.CommandContext(ctx, s.Cmd[0], s.Cmd[1:]...)
	cmd.Stderr = s.Stderr
	// Own process group so cancel reaps the plugin AND anything it spawned
	// (e.g. `drover source shepherd` runs its own `shepherd watch`). Default
	// CommandContext SIGKILLs only the direct child, orphaning grandchildren to
	// init where they pile up and leak memory.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// SIGTERM (not the default SIGKILL) so a plugin under signal.NotifyContext
	// shuts its own children down cleanly; negative pid signals the whole group.
	// WaitDelay then SIGKILLs the group if it ignores the term.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	scanErr := Scan(ctx, stdout, out, s.Name, s.logf)
	// CommandContext terminates the group on ctx cancel; reap it either way.
	waitErr := cmd.Wait()
	// The exit status is what explains a shim that dies immediately — a binary
	// too old for the subcommand, a port already bound, jq missing. Discarding it
	// left "ended (<nil>); restarting in 1s" looping every second with the reason
	// nowhere, so a clean scan reports how the process actually exited.
	if scanErr != nil {
		return scanErr
	}
	return waitErr
}

// Scan reads NDJSON from r and emits one event per line. Split out from the
// subprocess so it is unit-tested with a plain reader, and exported so a shim
// can reuse it. A line that will not decode is logged and skipped — one
// malformed event must not take the whole source down.
func Scan(ctx context.Context, r io.Reader, out chan<- loop.Event, name string, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // events carrying notes can be long
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		e, err := decode(line, name)
		if err != nil {
			logf("source %q: %v", name, err)
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- e:
		}
	}
	return sc.Err()
}
