package web

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ============================================================
// Self-update: detect newer GitHub releases and (on explicit
// admin action) replace the running binary in place.
//
// Detection resolves the latest release tag via the GitHub
// releases/latest redirect, trying a list of mirrors first so
// NAS boxes without direct GitHub access still work. The binary
// asset is downloaded, validated (ELF magic + minimum size),
// swapped with the running file and activated through
// syscall.Exec — the process image is replaced in place, same
// PID, same env (M365_LISTEN etc. preserved), port rebound.
//
// Auto-apply is intentionally NOT implemented: detection can be
// automatic, upgrading always needs an explicit admin action
// (UI button or API call).
// ============================================================

const (
	updateRepo       = "kosje/M365-Copilot2API-DSM"
	updateAssetName  = "m365-copilot2api-linux-amd64"
	alertEventUpdate = "update_available"
)

// selfUpdateApplyEnabled gates in-place binary replacement.
//
// On the Synology DSM build it is off. Two reasons: the package directory is
// owned by root while the service runs as the package user, so the rename
// would fail anyway unless the app directory were handed to the service (which
// would let the service rewrite its own code); and a swapped binary desyncs
// from the version Package Center records in INFO, so a later Package Center
// action could silently roll it back. Detection stays on — the console shows a
// banner and the admin installs the new SPK through Package Center.
const selfUpdateApplyEnabled = false

// Mirrors tried in order when direct GitHub access fails. Each
// entry is prefixed to the full github.com URL (ghproxy style).
var updateMirrors = []string{
	"", // direct
	"https://ghproxy.net",
	"https://gh-proxy.com",
	"https://ghfast.top",
}

var tagFromLocation = regexp.MustCompile(`/tag/(v[0-9][0-9A-Za-z.\-]*)$`)

type updateInfo struct {
	Current         string    `json:"current"`
	Latest          string    `json:"latest,omitempty"`
	UpdateAvailable bool      `json:"updateAvailable"`
	AssetURL        string    `json:"assetUrl,omitempty"`
	Notes           string    `json:"notes,omitempty"`
	CheckedAt       time.Time `json:"checkedAt"`
	Error           string    `json:"error,omitempty"`
}

var (
	updateMu         sync.Mutex
	lastUpdateInfo   updateInfo
	lastCheckAt      time.Time
	lastCheckFailed  bool
	updateHTTPClient = &http.Client{Timeout: 15 * time.Second}
)

// resolveLatestTag returns the newest release tag ("vX.Y.Z") via the
// releases/latest redirect, falling back through mirrors.
func resolveLatestTag() (string, error) {
	target := "https://github.com/" + updateRepo + "/releases/latest"
	var lastErr error
	for _, m := range updateMirrors {
		url := target
		if m != "" {
			url = m + "/" + target
		}
		client := &http.Client{
			Timeout: 12 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse // do not follow; read Location
			},
		}
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			continue
		}
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if resp.StatusCode < 300 || resp.StatusCode >= 400 || loc == "" {
			lastErr = fmt.Errorf("unexpected status %d from %s", resp.StatusCode, m)
			continue
		}
		if match := tagFromLocation.FindStringSubmatch(loc); match != nil {
			return match[1], nil
		}
		lastErr = fmt.Errorf("no tag in redirect %q", loc)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no mirror reachable")
	}
	return "", lastErr
}

// fetchReleaseMeta pulls release notes + asset URL for a tag, trying
// direct API then mirrors (ghproxy style also proxies api.github.com
// on most instances; failures are non-fatal).
func fetchReleaseMeta(tag string) (assetURL, notes string) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", updateRepo, tag)
	bases := append([]string{""}, updateMirrors[1:]...)
	for _, m := range bases {
		url := apiURL
		if m != "" {
			url = m + "/" + apiURL
		}
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := updateHTTPClient.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			continue
		}
		var rel struct {
			Body   string `json:"body"`
			Assets []struct {
				Name               string `json:"name"`
				BrowserDownloadURL string `json:"browser_download_url"`
			} `json:"assets"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel)
		resp.Body.Close()
		if err != nil {
			continue
		}
		for _, a := range rel.Assets {
			if a.Name == updateAssetName && a.BrowserDownloadURL != "" {
				return a.BrowserDownloadURL, rel.Body
			}
		}
		// API unreachable/unshaped: fall back to a deterministic URL.
		return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", updateRepo, tag, updateAssetName), rel.Body
	}
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", updateRepo, tag, updateAssetName), ""
}

func semverAtLeast(latest, current string) bool {
	num := func(v string) []int {
		v = strings.TrimPrefix(strings.TrimSpace(v), "v")
		parts := strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' || r == '+' })
		out := make([]int, 0, 3)
		for _, p := range parts {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				break
			}
			out = append(out, n)
		}
		return out
	}
	a, b := num(latest), num(current)
	for i := 0; i < 3; i++ {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// refreshUpdateInfo performs a fresh check (mirrors included) and caches it.
func refreshUpdateInfo() updateInfo {
	updateMu.Lock()
	defer updateMu.Unlock()
	info := updateInfo{Current: Version, CheckedAt: time.Now()}
	if strings.TrimSpace(Version) == "" || Version == "dev" {
		info.Error = "development build; self-update disabled"
		lastUpdateInfo, lastCheckAt, lastCheckFailed = info, time.Now(), true
		return info
	}
	tag, err := resolveLatestTag()
	if err != nil {
		info.Error = "check failed: " + err.Error()
		lastUpdateInfo, lastCheckAt, lastCheckFailed = info, time.Now(), true
		return info
	}
	info.Latest = tag
	info.UpdateAvailable = semverAtLeast(tag, Version)
	if info.UpdateAvailable {
		info.AssetURL, info.Notes = fetchReleaseMeta(tag)
	}
	lastUpdateInfo, lastCheckAt, lastCheckFailed = info, time.Now(), false
	return info
}

// cachedUpdateInfo returns the cached check result, refreshing when stale
// (30 min for success, 10 min for failures).
func cachedUpdateInfo() updateInfo {
	updateMu.Lock()
	cached, at, failed := lastUpdateInfo, lastCheckAt, lastCheckFailed
	updateMu.Unlock()
	ttl := 30 * time.Minute
	if failed {
		ttl = 10 * time.Minute
	}
	if !at.IsZero() && time.Since(at) < ttl {
		return cached
	}
	return refreshUpdateInfo()
}

// updateHandler serves GET /api/update (public): real update check with
// caching so the admin UI and monitoring can poll it cheaply.
func (s *Server) updateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	info := cachedUpdateInfo()
	if info.Error != "" {
		// Keep the response useful even when GitHub is unreachable.
		jsonOut(w, map[string]any{"current": info.Current, "updateAvailable": false, "error": info.Error, "checkedAt": info.CheckedAt, "applyEnabled": selfUpdateApplyEnabled})
		return
	}
	// applyEnabled lets the console hide the one-click button on builds where
	// upgrading is the package manager's job (see selfUpdateApplyEnabled).
	out := map[string]any{
		"current":         info.Current,
		"latest":          info.Latest,
		"updateAvailable": info.UpdateAvailable,
		"notes":           info.Notes,
		"checkedAt":       info.CheckedAt,
		"applyEnabled":    selfUpdateApplyEnabled,
	}
	if selfUpdateApplyEnabled {
		out["assetUrl"] = info.AssetURL
	} else {
		out["upgradeHint"] = "请在群晖「套件中心」安装新版 SPK 完成升级"
	}
	jsonOut(w, out)
}

// adminUpdateApply serves POST /api/admin/update/apply — downloads the
// release binary, swaps it in and re-execs the process.
func (s *Server) adminUpdateApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	if !selfUpdateApplyEnabled {
		writeOpenAIError(w, 403, "invalid_request_error",
			"此版本为群晖 DSM 套件，应用内更新已停用；请在「套件中心」安装新版 SPK 完成升级")
		return
	}
	var in struct {
		Tag string `json:"tag"` // optional: force a specific "vX.Y.Z"
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in)

	exe, err := os.Executable()
	if err != nil {
		writeOpenAIError(w, 500, "internal_error", "cannot locate executable: "+err.Error())
		return
	}
	if strings.TrimSpace(Version) == "" || Version == "dev" {
		writeOpenAIError(w, 400, "invalid_request_error", "development build; self-update disabled")
		return
	}

	tag := strings.TrimSpace(in.Tag)
	if tag == "" {
		info := refreshUpdateInfo()
		if info.Error != "" {
			writeOpenAIError(w, 502, "upstream_error", info.Error)
			return
		}
		if !info.UpdateAvailable {
			writeOpenAIError(w, 400, "invalid_request_error", "已是最新版本 v"+Version)
			return
		}
		tag = info.Latest
	}
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	if !semverAtLeast(tag, Version) {
		writeOpenAIError(w, 400, "invalid_request_error", fmt.Sprintf("目标版本 %s 不高于当前 v%s", tag, Version))
		return
	}

	asset := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", updateRepo, tag, updateAssetName)
	newPath := exe + ".new"
	if err := downloadBinary(asset, newPath); err != nil {
		writeOpenAIError(w, 502, "upstream_error", "下载失败（直连与镜像均不可达）："+err.Error())
		return
	}

	updateMu.Lock()
	defer updateMu.Unlock()
	bak := exe + ".bak"
	_ = os.Remove(bak)
	if err := os.Rename(exe, bak); err != nil {
		writeOpenAIError(w, 500, "internal_error", "备份旧二进制失败："+err.Error())
		return
	}
	if err := os.Rename(newPath, exe); err != nil {
		_ = os.Rename(bak, exe) // roll back
		writeOpenAIError(w, 500, "internal_error", "启用新二进制失败："+err.Error())
		return
	}

	// Answer the client first, then replace the process image in place:
	// same PID, inherited env (M365_LISTEN / M365_DATA_DIR preserved).
	jsonOut(w, map[string]any{"status": "restarting", "from": Version, "to": strings.TrimPrefix(tag, "v")})
	log.Printf("[self-update] v%s -> %s: binary swapped, restarting process", Version, tag)
	go func() {
		time.Sleep(1200 * time.Millisecond)
		args := append([]string{exe}, os.Args[1:]...)
		if err := syscall.Exec(exe, args, os.Environ()); err != nil {
			log.Printf("[self-update] exec failed: %v; rolling back", err)
			_ = os.Rename(exe, exe+".failed")
			_ = os.Rename(bak, exe)
			_ = syscall.Exec(exe, args, os.Environ())
		}
	}()
}

// downloadBinary fetches the release asset through direct access then
// mirrors, validating ELF magic and a minimum plausible size.
func downloadBinary(assetURL, dest string) error {
	var lastErr error
	for _, m := range updateMirrors {
		url := assetURL
		if m != "" {
			url = m + "/" + assetURL
		}
		if err := downloadToFile(url, dest); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

func downloadToFile(url, dest string) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d from %s", resp.StatusCode, url)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	written, err := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if closeErr != nil {
		os.Remove(tmp)
		return closeErr
	}
	if written < 5<<20 {
		os.Remove(tmp)
		return fmt.Errorf("downloaded file too small (%d bytes)", written)
	}
	// ELF magic check — refuse HTML error pages / truncated payloads.
	head := make([]byte, 4)
	rf, err := os.Open(tmp)
	if err != nil {
		return err
	}
	_, rerr := io.ReadFull(rf, head)
	rf.Close()
	if rerr != nil || string(head) != "\x7fELF" {
		os.Remove(tmp)
		return fmt.Errorf("not a valid ELF binary")
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// StartUpdateChecker polls GitHub every 6 hours and fires a webhook
// alert (event update_available) when a newer release exists. Detection
// only — applying an update always requires an explicit admin action.
func (s *Server) StartUpdateChecker() {
	go func() {
		// initial check shortly after boot so the UI banner is fresh
		time.Sleep(45 * time.Second)
		s.periodicUpdateCheck()
		for {
			time.Sleep(2 * time.Hour)
			s.periodicUpdateCheck()
		}
	}()
}

func (s *Server) periodicUpdateCheck() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[self-update] check panic recovered: %v", r)
		}
	}()
	info := refreshUpdateInfo()
	if info.UpdateAvailable {
		log.Printf("[self-update] update available: v%s -> %s", Version, info.Latest)
		notifyWebhookAlert(alertEventUpdate, fmt.Sprintf("发现新版本 %s（当前 v%s），可在管理台一键更新", info.Latest, Version), "update_available")
	}
}
