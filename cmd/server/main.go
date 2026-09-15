package main

import (
	"context"
	"errors"
	"io"
	"log"
	"m365-copilot2api/internal/outbound"
	"m365-copilot2api/internal/web"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
