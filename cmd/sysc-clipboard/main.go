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

type options struct {
	keyFile  string
	stateDir string
	check    bool
}

type runtimeState struct {
	root        string
	history     *history.History
	store       *store.Store
	persistence protocol.PersistenceState
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

	state, err := loadState(parsed.stateDir, parsed.keyFile)
	if err != nil {
		fmt.Fprintln(errorsOutput, err)
		return 1
	}
	if parsed.check {
		if err := checkState(state, output); err != nil {
			fmt.Fprintln(errorsOutput, err)
			return 1
		}
		return 0
	}

	logger := log.New(errorsOutput, "sysc-clipboard: ", log.LstdFlags)
	for _, warning := range state.warnings {
		logger.Printf("persistence unavailable: %v", warning)
	}
	if err := runDaemon(parent, state, logger); err != nil {
		logger.Print(err)
		return 1
	}
	return 0
}

func parseOptions(args []string, output io.Writer) (options, error) {
	var parsed options
	flags := flag.NewFlagSet("sysc-clipboard", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&parsed.keyFile, "key-file", "", "use a private 32-byte key file instead of Secret Service")
	flags.StringVar(&parsed.stateDir, "state-dir", "", "clipboard state directory")
	flags.BoolVar(&parsed.check, "check", false, "check persistence and runtime configuration, then exit")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	return parsed, nil
}

func resolveStateDir(override string) (string, error) {
	if override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("clipboard state directory must be absolute")
		}
		return filepath.Clean(override), nil
	}
	return store.DefaultStateDir()
}

func loadState(stateDir, keyFile string) (runtimeState, error) {
	state := runtimeState{
		history:     history.New(0),
		persistence: protocol.PersistenceUnavailable,
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
		state.warnings = append(state.warnings, fmt.Errorf("dropped %d unreadable clipboard entries", len(loaded.Dropped)))
		state.persistence = protocol.PersistenceVolatile
	}
	return state, nil
}

func checkState(state runtimeState, output io.Writer) error {
	socketPath, err := daemon.DefaultSocketPath()
	if err != nil {
		return fmt.Errorf("resolve clipboard socket: %w", err)
	}
	fmt.Fprintf(output, "state-dir: %s\n", state.root)
	fmt.Fprintf(output, "socket: %s\n", socketPath)
	fmt.Fprintf(output, "entries: %d\n", state.history.Count())
	fmt.Fprintf(output, "persistence: %s\n", state.persistence)
	for _, warning := range state.warnings {
		fmt.Fprintf(output, "warning: %v\n", warning)
	}
	return nil
}

func runDaemon(parent context.Context, state runtimeState, logger *log.Logger) error {
	var owner *wayland.Owner
	service := daemon.NewService(daemon.Options{
		History:     state.history,
		Store:       state.store,
		Persistence: state.persistence,
		Wayland:     protocol.WaylandUnavailable,
		Restore: func(item history.Item) error {
			if owner == nil {
				return daemon.ErrRestoreUnavailable
			}
			return owner.Restore(item)
		},
	})
	defer service.Close()

	serviceOptions := wayland.OwnerOptions{
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
