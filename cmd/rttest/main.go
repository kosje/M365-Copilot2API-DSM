package main

import (
	"fmt"
	"os"

	"m365-copilot2api/internal/auth"
)

func main() {
	rt := os.Args[1]
	scope := "https://m365.cloud.microsoft/v2/.default"
	if len(os.Args) > 2 {
		scope = os.Args[2]
	}
	set, err := auth.RefreshWithScope(rt, "", scope)
	if err != nil {
		fmt.Println("REFRESH_FAIL:", err)
		return
	}
	fmt.Println("REFRESH_OK at_len=", len(set.AccessToken))
}
