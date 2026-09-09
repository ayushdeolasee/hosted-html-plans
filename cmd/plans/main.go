// Command plans is the hosted-html-plans server and its CLI.
//
// Usage:
//
//	plans run                                # foreground server (LAN listener)
//	plans service install|uninstall|status   # launchd (macOS) / systemd (Linux)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ayushdeolasee/hosted-html-plans/internal/buildinfo"
	"github.com/ayushdeolasee/hosted-html-plans/internal/selfupdate"
	"github.com/ayushdeolasee/hosted-html-plans/server"
)

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "--version":
		fmt.Println(buildinfo.Version)
	case "run":
		os.Exit(cmdRun(os.Args[2:]))
	case "service":
		os.Exit(cmdService(os.Args[2:]))
	case "__apply-update":
		// This internal command is launched only as a separate one-shot
		// launchd/systemd job, so it survives restarting the server and can
		// restore the original executable if restart verification fails.
		os.Exit(selfupdate.RunApplyHelper(os.Args[2:], log.Default()))
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "plans: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `plans — hosted HTML plans server

Commands:
  version                    Print the installed build version.
  run                        Run the server in the foreground (LAN listener).
  service install            Install as a background service (launchd on macOS,
                              systemd on Linux) and start it.
  service uninstall          Stop and remove the background service registration.
  service status             Report registration/running state, healthz reachability,
                              config/data paths, and tailnet auth state.

Flags for "run":
  -listen ADDR              Override lan_listen from config (e.g. 127.0.0.1:8080, off).

Flags for "service install|uninstall|status":
  -user                      Linux only: use a user-level systemd unit
                              (~/.config/systemd/user/plans.service, systemctl --user)
                              instead of the system unit. Ignored on macOS.
  -print                     Dry run: render and print the plist/unit and the
                              commands that would run, without touching the system.
                              Same effect as setting PLANS_SERVICE_DRYRUN=1.
`)
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	listen := fs.String("listen", "", "override lan_listen from config")
	_ = fs.Parse(args)

	// plan.html §7: cap the macOS launchd log file so it doesn't grow
	// unbounded — rotate to plans.log.1 on startup if it's over ~10MB.
	// No-op on Linux (systemd/journald handles its own rotation) and when
	// no log file exists yet (e.g. a foreground dev run).
	rotateLogIfNeeded()

	cfg, err := server.LoadConfig()
	if err != nil {
		log.Printf("config: %v", err)
		return 1
	}
	if *listen != "" {
		cfg.LANListen = *listen
	}

	dataDir, err := server.DataDir()
	if err != nil {
		log.Printf("data dir: %v", err)
		return 1
	}
	store, err := server.NewStore(dataDir)
	if err != nil {
		log.Printf("store: %v", err)
		return 1
	}

	srv := server.NewServer(store, cfg, nil, log.Default())
	srv.SetUpdateManager(selfupdate.NewManager(buildinfo.Version, log.Default()))
	log.Printf("data dir: %s", dataDir)

	// Build the routers once and reuse them across every listener: the full
	// app for the LAN + tailnet listeners, the shares-only router for the
	// funnel. Handlers read live state (Funnel, tailnet identity) per request,
	// so building them before those are wired is fine.
	fullHandler := srv.FullHandler()
	sharesHandler := srv.SharesHandler()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Tailnet (access modes 2 + 3 of plan.html §3). The manager owns the tsnet
	// node's runtime lifecycle: main.go starts it here when enabled, the
	// settings API can start/stop it live, and shutdown stops it — all through
	// one code path. When disabled, no tsnet code runs and the box stays
	// LAN-only.
	tn := server.NewTailnetManager(ctx, server.TailnetManagerDeps{
		DataDir:       dataDir,
		Logger:        log.Default(),
		Server:        srv,
		FullHandler:   fullHandler,
		SharesHandler: sharesHandler,
		ActiveShares:  store.ActiveShareCount,
	})
	srv.SetTailnetManager(tn)
	if cfg.TailscaleEnabled {
		if terr := tn.Start(); terr != nil {
			log.Printf("tailscale: %v (continuing LAN-only)", terr)
		} else {
			log.Printf("tailscale: enabled; bringing up tsnet node %q (first run needs a one-time auth — watch for the URL)", cfg.Hostname)
		}
	} else {
		log.Printf("tailscale: disabled in config (LAN-only mode)")
	}

	// LAN listener (plain TCP :8080). Optional: "off"/"" disables it.
	var httpSrv *http.Server
	lanOff := cfg.LANListen == "" || cfg.LANListen == "off"
	errCh := make(chan error, 1)
	if !lanOff {
		httpSrv = &http.Server{
			Addr:              cfg.LANListen,
			Handler:           fullHandler,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			log.Printf("listening on http://%s (LAN)", cfg.LANListen)
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	} else {
		log.Printf("lan_listen is off; LAN listener disabled")
		if !tn.Running() {
			log.Printf("no listeners configured (LAN off and tailscale disabled). Exiting.")
			return 0
		}
	}

	select {
	case err := <-errCh:
		log.Printf("server error: %v", err)
		return 1
	case <-ctx.Done():
		log.Printf("shutting down…")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		code := 0
		if httpSrv != nil {
			if err := httpSrv.Shutdown(shutCtx); err != nil {
				log.Printf("graceful LAN shutdown failed: %v", err)
				code = 1
			}
		}
		if err := tn.Stop(); err != nil {
			log.Printf("tailnet shutdown: %v", err)
		}
		return code
	}
}
