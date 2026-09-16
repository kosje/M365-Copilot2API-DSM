package web

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Public, unguessable capability URLs remain valid across process restarts.
// Retention is bounded by age, file count and total bytes; no account tokens
// or client-provided filenames are stored alongside these image bytes.
const generatedImageRetention = 72 * time.Hour
const generatedImageDiskLimit int64 = 512 << 20

func generatedImageDir() string { return filepath.Join(chatDataDir(), "generated-images") }

func (s *Server) storeGeneratedImage(data []byte, contentType string) (string, error) {
	if len(data) == 0 || len(data) > maxGeneratedImageBytes || !strings.HasPrefix(http.DetectContentType(data), "image/") {
		return "", fmt.Errorf("invalid image bytes")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := generatedImageDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var retained []os.FileInfo
	var total int64
	now := time.Now()
	for _, entry := range entries {
		if _, err := uuid.Parse(entry.Name()); err != nil {
			continue
		}
		fi, err := entry.Info()
		if err != nil {
			return "", err
		}
		if !fi.Mode().IsRegular() {
			continue
		}
		if now.Sub(fi.ModTime()) >= generatedImageRetention {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return "", err
			}
			continue
		}
		retained = append(retained, fi)
		total += fi.Size()
	}
	sort.Slice(retained, func(i, j int) bool { return retained[i].ModTime().Before(retained[j].ModTime()) })
	for len(retained) > 0 && (len(retained) >= maxGeneratedImages || total+int64(len(data)) > generatedImageDiskLimit) {
		fi := retained[0]
		if err := os.Remove(filepath.Join(dir, fi.Name())); err != nil {
			return "", err
		}
		total -= fi.Size()
		retained = retained[1:]
	}
	id := uuid.NewString()
	if err := writeFileAtomic(filepath.Join(dir, id), data, 0600); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Server) loadGeneratedImage(id string) (generatedImage, error) {
	var item generatedImage
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id {
		return item, os.ErrNotExist
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(generatedImageDir(), id)
	fi, err := os.Lstat(path)
	if err != nil {
		return item, err
	}
	if !fi.Mode().IsRegular() {
		return item, os.ErrNotExist
	}
	if time.Since(fi.ModTime()) >= generatedImageRetention {
		_ = os.Remove(path)
		return item, os.ErrNotExist
	}
	f, err := os.Open(path)
	if err != nil {
		return item, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxGeneratedImageBytes+1))
	if err != nil {
		return item, err
	}
	ct := http.DetectContentType(data)
	if len(data) == 0 || len(data) > maxGeneratedImageBytes || !strings.HasPrefix(ct, "image/") {
		return item, fmt.Errorf("invalid stored image")
	}
	return generatedImage{Data: data, ContentType: ct, ExpiresAt: fi.ModTime().Add(generatedImageRetention)}, nil
}
