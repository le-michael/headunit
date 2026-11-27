package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"headunit/internal/auth"
	"headunit/internal/config"
	"headunit/internal/scanner"
	"headunit/internal/store"
	"headunit/internal/stream"
	"headunit/internal/subsonic"
)

const (
	defaultReadHeaderTimeout = 10 * time.Second
	defaultIdleTimeout       = 120 * time.Second
	defaultShutdownTimeout   = 10 * time.Second
	dbFilename               = "headunit.db"
)

type server struct {
	cfg             config.Config
	logger          *slog.Logger
	store           *store.Store
	streamer        *stream.DirectStreamer
	scanner         *scanner.Service
	httpSrv         *http.Server
	shutdownTimeout time.Duration
}

func main() {
	if len(os.Args) <= 1 {
		runServerMain()
		return
	}
	if os.Args[1] == "serve" {
		if err := parseServeArgs(os.Args[2:], os.Stderr); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "headunit serve: %v\n", err)
			os.Exit(1)
		}
		runServerMain()
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := runCLI(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "headunit: %v\n", err)
		os.Exit(1)
	}
}

func runServerMain() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(context.Background(), logger); err != nil {
		logger.Error("headunit exited with error", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := newServer(ctx, cfg, logger)
	if err != nil {
		return err
	}

	// Bind the TCP listener before starting background scanner/watcher goroutines
	// so address conflicts fail fast with deterministic resource rollback.
	ln, err := net.Listen("tcp", srv.httpSrv.Addr)
	if err != nil {
		return errors.Join(
			fmt.Errorf("listen %q: %w", srv.httpSrv.Addr, err),
			srv.closeResources(),
		)
	}

	return srv.serve(ctx, ln)
}

func newServer(ctx context.Context, cfg config.Config, logger *slog.Logger) (*server, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	dbPath := filepath.Join(cfg.DataDir, dbFilename)
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	srv := &server{
		cfg:             cfg,
		logger:          logger,
		store:           st,
		shutdownTimeout: defaultShutdownTimeout,
	}

	streamer, err := stream.NewDirectStreamer(cfg.MusicDir)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("init streamer: %w", err), srv.closeResources())
	}
	srv.streamer = streamer

	scanSvc, err := scanner.New(cfg.MusicDir, cfg.ScanInterval, st, logger)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("init scanner: %w", err), srv.closeResources())
	}
	srv.scanner = scanSvc

	handler, err := subsonic.NewHandler(subsonic.Dependencies{
		Auth:     auth.NoopAuthenticator{},
		Scanner:  scanSvc,
		Store:    st,
		Streamer: streamer,
		Logger:   logger,
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("init http handler: %w", err), srv.closeResources())
	}

	srv.httpSrv = &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		IdleTimeout:       defaultIdleTimeout,
		// WriteTimeout is intentionally left at zero so long lossless audio
		// streams are never terminated mid-track. BaseContext is left at the
		// default context.Background so in-flight requests drain cleanly during
		// graceful shutdown.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	return srv, nil
}

func (s *server) serve(ctx context.Context, ln net.Listener) error {
	if ln == nil {
		return errors.Join(errors.New("listener must not be nil"), s.closeResources())
	}

	logger := s.logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	// Derive the scanner lifecycle context from context.Background rather than
	// the signal context so scanner teardown starts strictly after HTTP shutdown
	// drains active handlers.
	scanCtx, scanCancel := context.WithCancel(context.Background())
	defer scanCancel()

	logger.Info(
		"headunit starting",
		"addr", ln.Addr().String(),
		"music_dir", s.cfg.MusicDir,
		"data_dir", s.cfg.DataDir,
		"scan_interval", s.cfg.ScanInterval,
	)

	s.scanner.Start(scanCtx)

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- s.httpSrv.Serve(ln)
	}()

	select {
	case serveErr := <-serveErrCh:
		scanCancel()
		s.scanner.Wait()
		resErr := s.closeResources()
		return errors.Join(unexpectedServeError(serveErr), resErr)

	case <-ctx.Done():
		logger.Info("headunit shutting down")

		shutdownTimeout := s.shutdownTimeout
		if shutdownTimeout <= 0 {
			shutdownTimeout = defaultShutdownTimeout
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer shutdownCancel()

		var shutdownErr, closeErr error
		if err := s.httpSrv.Shutdown(shutdownCtx); err != nil {
			shutdownErr = fmt.Errorf("shutdown http server: %w", err)
			logger.Warn("graceful http shutdown failed; forcing close", "error", err)
			if cErr := s.httpSrv.Close(); cErr != nil {
				closeErr = fmt.Errorf("force close http server: %w", cErr)
			}
		}

		serveErr := <-serveErrCh
		scanCancel()
		s.scanner.Wait()
		resErr := s.closeResources()

		return errors.Join(shutdownErr, closeErr, unexpectedServeError(serveErr), resErr)
	}
}

func (s *server) closeResources() error {
	if s == nil {
		return nil
	}
	// Close streamer before store so the confined music root handle is released
	// before SQLite checkpoints WAL state and closes its pools.
	var streamerErr, storeErr error
	if s.streamer != nil {
		if err := s.streamer.Close(); err != nil {
			streamerErr = fmt.Errorf("close streamer: %w", err)
		}
	}
	if s.store != nil {
		if err := s.store.Close(); err != nil {
			storeErr = fmt.Errorf("close store: %w", err)
		}
	}
	return errors.Join(streamerErr, storeErr)
}

func unexpectedServeError(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("serve http: %w", err)
}
