package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"m365-copilot2api/internal/auth"
)

var scopes = []string{
	"https://m365.cloud.microsoft/v2/.default",
	"https://api.spaces.skype.com/.default",
	"https://substrate.office.com/sydney/.default",
	"https://teams.microsoft.com/.default",
	"https://graph.microsoft.com/.default",
	"https://substrate.office.com/.default",
	"https://outlook.office.com/.default",
	"00000003-0000-0ff1-ce00-000000000000/.default",
}

func main() {
	url := os.Args[1]
	dataDir := os.Getenv("M365_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
	store, err := auth.OpenStore(dataDir + "/accounts.json")
	if err != nil {
		fmt.Println("open store:", err)
		os.Exit(1)
	}
	accs := store.List()
	client := &http.Client{Timeout: 40 * time.Second}

	var acc *auth.AccountToken
	for i := range accs {
		if accs[i].Status == "online" && accs[i].RefreshToken != "" {
			acc = &accs[i]
			break
		}
	}
	fmt.Println("account:", acc.Email)

	try := func(tag string, headers map[string]string) {
		req, _ := http.NewRequest("GET", url, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/131.0 Safari/537.36")
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("  [%s] ERR %v\n", tag, err)
			return
		}
		defer resp.Body.Close()
		head := make([]byte, 8)
		n, _ := io.ReadFull(resp.Body, head)
		fmt.Printf("  [%s] HTTP %d ct=%q magic=%q www-auth=%q loc=%q\n", tag, resp.StatusCode,
			resp.Header.Get("Content-Type"), strings.TrimSpace(string(head[:n])),
			resp.Header.Get("WWW-Authenticate"), resp.Header.Get("Location"))
	}

	try("anon", nil)
	for _, sc := range scopes {
		set, err := auth.RefreshWithScope(acc.RefreshToken, acc.ClientID, sc)
		if err != nil {
			fmt.Printf("  [refresh %s] FAIL %v\n", sc, err)
			continue
		}
		try("bearer:"+sc, map[string]string{"Authorization": "Bearer " + set.AccessToken})
	}
}
