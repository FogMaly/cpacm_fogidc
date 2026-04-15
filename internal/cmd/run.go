// Package cmd provides command-line interface functionality for the CLI Proxy API server.
// It includes authentication flows for various AI service providers, service startup,
// and other command-line operations.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy"
	log "github.com/sirupsen/logrus"
)

// StartService builds and runs the proxy service using the exported SDK.
// It creates a new proxy service instance, sets up signal handling for graceful shutdown,
// and starts the service with the provided configuration.
//
// Parameters:
//   - cfg: The application configuration
//   - configPath: The path to the configuration file
//   - localPassword: Optional password accepted for local management requests
func StartService(cfg *config.Config, configPath string, localPassword string) error {
	builder := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath).
		WithLocalManagementPassword(localPassword)

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signalCh)

	stopReasonCh := make(chan string, 1)
	recordStopReason := func(reason string) {
		select {
		case stopReasonCh <- reason:
		default:
		}
	}

	go func() {
		sig := <-signalCh
		log.Warnf("shutdown signal received signal=%s pid=%d", sig.String(), os.Getpid())
		recordStopReason(fmt.Sprintf("signal:%s", sig.String()))
		cancel()
	}()

	if localPassword != "" {
		builder = builder.WithServerOptions(api.WithKeepAliveEndpoint(10*time.Second, func() {
			log.Warn("keep-alive endpoint idle for 10s, shutting down")
			recordStopReason("keep-alive-timeout")
			cancel()
		}))
	}

	service, err := builder.Build()
	if err != nil {
		return fmt.Errorf("failed to build proxy service: %w", err)
	}

	log.Infof(
		"proxy service starting pid=%d ppid=%d config=%s port=%d host=%s",
		os.Getpid(),
		os.Getppid(),
		configPath,
		cfg.Port,
		cfg.Host,
	)

	err = service.Run(runCtx)

	stopReason := ""
	select {
	case stopReason = <-stopReasonCh:
	default:
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if errShutdown := service.Shutdown(shutdownCtx); errShutdown != nil {
		log.Errorf("proxy service shutdown error: %v", errShutdown)
		if err == nil || errors.Is(err, context.Canceled) {
			err = errShutdown
		}
	}

	switch {
	case err == nil:
		log.Infof("proxy service exited cleanly pid=%d exit_code=0", os.Getpid())
		return nil
	case errors.Is(err, context.Canceled):
		if stopReason == "" {
			stopReason = "context-canceled"
		}
		log.Infof("proxy service stopped pid=%d reason=%s exit_code=0", os.Getpid(), stopReason)
		return nil
	default:
		log.Errorf("proxy service exited with error pid=%d reason=%s err=%v", os.Getpid(), stopReason, err)
		return err
	}
}

// WaitForCloudDeploy waits indefinitely for shutdown signals in cloud deploy mode
// when no configuration file is available.
func WaitForCloudDeploy() {
	// Clarify that we are intentionally idle for configuration and not running the API server.
	log.Info("Cloud deploy mode: No config found; standing by for configuration. API server is not started. Press Ctrl+C to exit.")

	ctxSignal, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Block until shutdown signal is received
	<-ctxSignal.Done()
	log.Info("Cloud deploy mode: Shutdown signal received; exiting")
}
