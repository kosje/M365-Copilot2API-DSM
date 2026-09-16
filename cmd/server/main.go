package main

import (
	"context"
	"errors"
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
