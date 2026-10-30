package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Nomadcxx/sysc-clipboard/internal/daemon"
	"github.com/Nomadcxx/sysc-clipboard/internal/history"
	"github.com/Nomadcxx/sysc-clipboard/internal/store"
	"github.com/Nomadcxx/sysc-clipboard/internal/wayland"
	"github.com/Nomadcxx/sysc-clipboard/protocol"
)

const maxAutoClearDays = int64((1<<63 - 1) / int64(24*time.Hour))

type options struct {
	keyFile        string
	stateDir       string
	check          bool
	disableCapture bool
	autoClearDays  int
	clearAtStartup bool
	limits         history.Limits
}

type runtimeState struct {
	root        string
	history     *history.History
	store       *store.Store
	persistence protocol.PersistenceState
	limits      history.Limits
	warnings    []error
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(parent context.Context, args []string, output, errorsOutput io.Writer) int {
	parsed, err := parseOptions(args, errorsOutput)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(errorsOutput, err)
		return 2
	}

	state, err := loadState(parsed.stateDir, parsed.keyFile, parsed.limits)
	if err != nil {
		fmt.Fprintln(errorsOutput, err)
		return 1
	}
	if parsed.check {
		if err := checkState(state, parsed, output); err != nil {
			fmt.Fprintln(errorsOutput, err)
			return 1
		}
		return 0
	}
	if err := applyStartupCleanup(&state, parsed.autoClearDays, parsed.clearAtStartup, time.Now()); err != nil {
		fmt.Fprintln(errorsOutput, err)
		return 1
	}

	logger := log.New(errorsOutput, "sysc-clipboard: ", log.LstdFlags)
	for _, warning := range state.warnings {
		logger.Printf("persistence unavailable: %v", warning)
	}
	if err := runDaemon(parent, state, logger, parsed.disableCapture, autoClearTTL(parsed.autoClearDays)); err != nil {
		logger.Print(err)
		return 1
	}
	return 0
}

func parseOptions(args []string, output io.Writer) (options, error) {
	defaults := history.DefaultLimits()
	parsed := options{limits: defaults}
	flags := flag.NewFlagSet("sysc-clipboard", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&parsed.keyFile, "key-file", "", help("use a private 32-byte key file instead of Secret Service"))
	flags.StringVar(&parsed.stateDir, "state-dir", "", help("clipboard state directory"))
	flags.BoolVar(&parsed.check, "check", false, help("check persistence and runtime configuration, then exit"))
	flags.BoolVar(&parsed.disableCapture, "disable-capture", false, help("disable clipboard history capture; copy and existing history remain available"))
	flags.IntVar(&parsed.autoClearDays, "auto-clear-days", 0, help("maximum age in days before unpinned entries are removed; 0 disables auto-clear"))
	flags.BoolVar(&parsed.clearAtStartup, "clear-at-startup", false, help("clear all clipboard history when the daemon starts"))
	flags.IntVar(&parsed.limits.MaxEntries, "max-entries", defaults.MaxEntries, help("maximum retained history entries"))
	flags.Uint64Var(&parsed.limits.MaxTotalBytes, "max-total-bytes", defaults.MaxTotalBytes, help("maximum combined history payload bytes"))
	flags.Uint64Var(&parsed.limits.MaxTextBytes, "max-text-bytes", defaults.MaxTextBytes, help("maximum bytes in a captured text entry"))
	flags.Uint64Var(&parsed.limits.MaxImageBytes, "max-image-bytes", defaults.MaxImageBytes, help("maximum bytes in a captured image entry"))
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 {
		return options{}, fmt.Errorf(help("unexpected argument %q"), flags.Arg(0))
	}
	if err := validateAutoClearDays(parsed.autoClearDays); err != nil {
		return options{}, err
	}
	if err := validateLimits(parsed.limits); err != nil {
		return options{}, err
	}
	return parsed, nil
}

func validateAutoClearDays(days int) error {
	if days < 0 || int64(days) > maxAutoClearDays {
		return errors.New(help("auto-clear days must be non-negative and within the duration limit"))
	}
	return nil
}

func autoClearTTL(days int) time.Duration {
	return time.Duration(days) * 24 * time.Hour
}

func validateLimits(limits history.Limits) error {
	defaults := history.DefaultLimits()
	if limits.MaxEntries < 1 || limits.MaxEntries > defaults.MaxEntries ||
		limits.MaxTotalBytes < 1 || limits.MaxTotalBytes > defaults.MaxTotalBytes ||
		limits.MaxTextBytes < 1 || limits.MaxTextBytes > defaults.MaxTextBytes ||
		limits.MaxImageBytes < 1 || limits.MaxImageBytes > defaults.MaxImageBytes {
		return errors.New(help("history caps must be positive and within the protocol limits"))
	}
	return nil
}

func resolveStateDir(override string) (string, error) {
	if override != "" {
		if !filepath.IsAbs(override) {
			return "", errors.New(help("clipboard state directory must be absolute"))
		}
		return filepath.Clean(override), nil
	}
	return store.DefaultStateDir()
}

func loadState(stateDir, keyFile string, limits history.Limits) (runtimeState, error) {
	state := runtimeState{
		history:     history.NewWithLimits(0, limits),
		persistence: protocol.PersistenceUnavailable,
		limits:      limits,
	}
	root, err := resolveStateDir(stateDir)
	if err != nil {
		return state, err
	}
	state.root = root

	key, err := store.LoadKey(keyFile)
	if err != nil {
		if keyFile != "" {
			return state, fmt.Errorf("load clipboard key: %w", err)
		}
		state.warnings = append(state.warnings, err)
		return state, nil
	}

	state.store, err = store.New(root, key)
	if err != nil {
		state.warnings = append(state.warnings, err)
		state.store = nil
		return state, nil
	}
	loaded, loadErr := state.store.Load()
	if len(loaded.Items) > 0 {
		if err := state.history.Replace(loaded.Items); err != nil {
			state.warnings = append(state.warnings, fmt.Errorf("load clipboard history: %w", err))
		}
	}
	state.persistence = protocol.PersistenceDurable
	if loadErr != nil {
		state.warnings = append(state.warnings, loadErr)
		state.persistence = protocol.PersistenceVolatile
	}
	if loaded.ManifestCorrupt {
		state.persistence = protocol.PersistenceVolatile
	}
	if len(loaded.Dropped) > 0 {
		state.warnings = append(state.warnings, fmt.Errorf(help("dropped %d unreadable clipboard entries"), len(loaded.Dropped)))
		state.persistence = protocol.PersistenceVolatile
	}
	return state, nil
}

func checkState(state runtimeState, parsed options, output io.Writer) error {
	socketPath, err := daemon.DefaultSocketPath()
	if err != nil {
		return fmt.Errorf("resolve clipboard socket: %w", err)
	}
	fmt.Fprintf(output, "state-dir: %s\n", state.root)
	fmt.Fprintf(output, "socket: %s\n", socketPath)
	fmt.Fprintf(output, "entries: %d\n", state.history.Count())
	fmt.Fprintf(output, "persistence: %s\n", state.persistence)
	fmt.Fprintf(output, "max-entries: %d\n", state.limits.MaxEntries)
	fmt.Fprintf(output, "max-total-bytes: %d\n", state.limits.MaxTotalBytes)
	fmt.Fprintf(output, "max-text-bytes: %d\n", state.limits.MaxTextBytes)
	fmt.Fprintf(output, "max-image-bytes: %d\n", state.limits.MaxImageBytes)
	captureState := "enabled"
	if parsed.disableCapture {
		captureState = "disabled"
	}
	fmt.Fprintf(output, "capture: %s\n", captureState)
	fmt.Fprintf(output, "auto-clear-days: %d\n", parsed.autoClearDays)
	clearAtStartup := "disabled"
	if parsed.clearAtStartup {
		clearAtStartup = "enabled"
	}
	fmt.Fprintf(output, "clear-at-startup: %s\n", clearAtStartup)
	for _, warning := range state.warnings {
		fmt.Fprintf(output, "warning: %v\n", warning)
	}
	return nil
}

func applyStartupCleanup(state *runtimeState, autoClearDays int, clearAtStartup bool, now time.Time) error {
	if clearAtStartup {
		if _, err := state.history.Clear(protocol.ClearAll); err != nil {
			return fmt.Errorf("clear clipboard history at startup: %w", err)
		}
		if state.store == nil {
			return nil
		}
		if err := state.store.Save(state.history.Items()); err != nil {
			return fmt.Errorf("persist clipboard history clear: %w", err)
		}
		state.persistence = protocol.PersistenceDurable
		return nil
	}
	if autoClearDays == 0 {
		return nil
	}
	changes := state.history.ExpireBefore(now.Add(-autoClearTTL(autoClearDays)))
	if len(changes) == 0 || state.store == nil {
		return nil
	}
	if err := state.store.Save(state.history.Items()); err != nil {
		return fmt.Errorf("persist expired clipboard history: %w", err)
	}
	state.persistence = protocol.PersistenceDurable
	return nil
}

func runDaemon(parent context.Context, state runtimeState, logger *log.Logger, disableCapture bool, historyTTL time.Duration) error {
	var owner *wayland.Owner
	service := daemon.NewService(daemon.Options{
		History:        state.history,
		Store:          state.store,
		Persistence:    state.persistence,
		Wayland:        protocol.WaylandUnavailable,
		DisableCapture: disableCapture,
		HistoryTTL:     historyTTL,
		Restore: func(ctx context.Context, item history.Item) error {
			if owner == nil {
				return daemon.ErrRestoreUnavailable
			}
			return owner.Restore(ctx, item)
		},
	})
	defer service.Close()

	serviceOptions := wayland.OwnerOptions{
		MaxTextBytes:  state.limits.MaxTextBytes,
		MaxImageBytes: state.limits.MaxImageBytes,
		MaxTotalBytes: state.limits.MaxTotalBytes,
		Capture: func(ctx context.Context, capture history.Capture) error {
			return service.Capture(ctx, capture)
		},
		CaptureError: func(err error) {
			logger.Printf("capture degraded: %v", err)
		},
		SetGeneration: func(ctx context.Context, generation uint64) error {
			return service.SetGeneration(ctx, generation)
		},
		SetWaylandState: func(state protocol.WaylandState) error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			return service.SetWaylandState(ctx, state)
		},
	}
	var err error
	owner, err = wayland.NewOwner(serviceOptions)
	if err != nil {
		logger.Printf("Wayland unavailable: %v", err)
	}

	serverPath, err := daemon.DefaultSocketPath()
	if err != nil {
		return fmt.Errorf("resolve clipboard socket: %w", err)
	}
	server, err := daemon.NewServer(service, serverPath)
	if err != nil {
		return err
	}
	defer server.Close()

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(ctx) }()

	ownerDone := make(chan error, 1)
	ownerFinished := true
	if owner != nil {
		ownerFinished = false
		go func() { ownerDone <- owner.Run(ctx) }()
	}

	serverFinished := false
	var runErr error
	ctxDone := ctx.Done()
	for !serverFinished || !ownerFinished {
		select {
		case err := <-serverDone:
			serverFinished = true
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, daemon.ErrServerClosed) {
				runErr = err
				cancel()
				_ = server.Close()
			}
		case err := <-ownerDone:
			ownerFinished = true
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, wayland.ErrOwnerUnavailable) {
				logger.Printf("Wayland owner stopped: %v", err)
			}
		case <-ctxDone:
			cancel()
			_ = server.Close()
			ctxDone = nil
		}
	}
	return runErr
}
