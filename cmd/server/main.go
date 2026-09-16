package main

import (
	"context"
	"errors"
	"io"
	"log"
	"m365-copilot2api/internal/outbound"
	"m365-copilot2api/internal/web"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Dir(exe); dir != "" {
			os.Chdir(dir)
		}
	}
	// Self-update handoff (Windows only): when spawned by an older instance
	// during an online update, copy this (new) executable over the canonical
	// path now that the old process has exited and freed the file lock.
	if target := os.Getenv("M365_SELF_UPDATE_REPLACE"); target != "" {
		os.Unsetenv("M365_SELF_UPDATE_REPLACE")
		if err := replaceRunningExecutable(target); err != nil {
			log.Printf("[self-update] replace canonical exe failed: %v", err)
		}
	}
	web.ApplyStartupSettingsEnv()
	if err := outbound.ConfigureFromEnv(); err != nil {
		log.Fatalf("configure outbound proxy: %v", err)
	}
	s, e := web.New()
	if e != nil {
		log.Fatal(e)
	}
	s.InitM365CloudClient()
	s.StartAutoCleanup()
	s.StartConvCacheGC()
	s.StartChatJanitor()
	s.RefreshExpiredTokens()
	s.StartQuotaRefresh()
	s.StartAutoModelsRefresh()
	s.StartTokenRefresh()
	s.StartAlertMonitor()
	s.StartUpdateChecker()
	web.OpenAuditStore()
	listen := "127.0.0.1:4141"
	if v := os.Getenv("M365_LISTEN"); v != "" {
		listen = v
	}
	if !isLoopbackListen(listen) {
		// Binding beyond loopback is the normal deployment for this gateway -
		// serving LAN clients is the point - but it also exposes the admin
		// console over plain HTTP, which puts the login password and the
		// session cookie on the wire. Say so once, loudly, at startup.
		log.Printf("[security] listening on %s over plain HTTP: the administrator "+
			"password and session cookie are sent in the clear on this network. "+
			"Put a TLS-terminating reverse proxy in front (see SECURITY.md), or set "+
			"M365_LISTEN=127.0.0.1:4141 to keep the console on this host.", listen)
	}
	log.Printf("m365-copilot2api listening on http://%s\\n", listen)
	server := &http.Server{
		Addr:              listen,
		Handler:           s.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		WriteTimeout:      0, // streaming endpoints need an open-ended write window.
	}
	s.SetHTTPServer(server)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown: %v", err)
		}
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	web.StopPersistLoop()
	log.Println("shutdown complete")
}

// isLoopbackListen reports whether addr restricts the listener to this host.
// An empty host means every interface, which is not loopback.
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// replaceRunningExecutable copies the currently running executable over
// target (backing up the previous target to target+".bak"). Used on Windows
// self-update, where the OS locks the running .exe and the new copy therefore
// takes over the canonical path after the old process has exited.
func replaceRunningExecutable(target string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if filepath.Clean(self) == filepath.Clean(target) {
		return nil // already in place
	}
	bak := target + ".bak"
	_ = os.Remove(bak)
	_ = copyFile(target, bak) // best-effort backup of the replaced binary
	return copyFile(self, target)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
