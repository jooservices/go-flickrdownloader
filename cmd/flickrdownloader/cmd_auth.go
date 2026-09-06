package main

import (
	"bufio"
	"fmt"
	"os"
	"time"

	"github.com/jooservices/flickrdownloader/pkg/api"
	"github.com/jooservices/flickrdownloader/pkg/config"
	"github.com/jooservices/flickrdownloader/pkg/quota"
	"github.com/jooservices/flickrdownloader/pkg/ui"
	"github.com/spf13/cobra"
)

func runAuth(cmd *cobra.Command, args []string) error {
	fmt.Printf("\n%s\n\n", ui.Bordered("Flickr Downloader", ui.ColorCyan))
	fmt.Printf("  %sStep 1%s  Get an API key from:\n", ui.ColorBold, ui.ColorReset)
	fmt.Println("          https://www.flickr.com/services/apps/create/apply/")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)

	fmt.Printf("  %sAPI Key:%s    ", ui.ColorCyan, ui.ColorReset)
	apiKey, err := readLine(reader)
	if err != nil {
		return fmt.Errorf("read API key: %w", err)
	}

	fmt.Printf("  %sAPI Secret:%s ", ui.ColorCyan, ui.ColorReset)
	apiSecret, err := readSecret(reader)
	if err != nil {
		return fmt.Errorf("read API secret: %w", err)
	}

	if apiKey == "" || apiSecret == "" {
		return fmt.Errorf("API key and secret are required")
	}

	fmt.Printf("\n  %sStep 2%s  %sRequesting OAuth token...%s\n", ui.ColorBold, ui.ColorReset, ui.ColorDim, ui.ColorReset)
	reqToken, reqSecret, err := api.GetRequestToken(apiKey, apiSecret)
	if err != nil {
		return fmt.Errorf("get request token: %w", err)
	}

	authURL := api.GetAuthorizeURL(reqToken)
	fmt.Printf("\n  %sStep 3%s  Open this URL in your browser:\n", ui.ColorBold, ui.ColorReset)
	fmt.Printf("          %s%s%s\n", ui.ColorCyan, authURL, ui.ColorReset)
	fmt.Printf("\n  %sStep 4%s  After authorizing, paste the 9-digit code here: ", ui.ColorBold, ui.ColorReset)

	verifier, err := readLine(reader)
	if err != nil {
		return fmt.Errorf("read verifier: %w", err)
	}

	if verifier == "" {
		return fmt.Errorf("verifier is required")
	}

	fmt.Printf("\n  %sStep 5%s  %sExchanging for access token...%s\n", ui.ColorBold, ui.ColorReset, ui.ColorDim, ui.ColorReset)
	accessToken, accessSecret, userNSID, username, err := api.GetAccessToken(apiKey, apiSecret, reqToken, reqSecret, verifier)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	cfg := &config.Config{
		APIKey:               apiKey,
		APISecret:            apiSecret,
		OAuthToken:           accessToken,
		OAuthSecret:          accessSecret,
		NSID:                 userNSID,
		WorkerCount:          20,
		OutputDir:            "./photos",
		APIHourlyLimit:       quota.DefaultHourlyLimit,
		APIRateMS:            int(quota.DefaultInterval / time.Millisecond),
		CacheListingTTLHours: config.DefaultCacheListingTTLHours,
	}

	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	fmt.Printf("\n  %s%s Authenticated as %s (NSID: %s)%s\n", ui.ColorGreen, ui.IconOk, username, userNSID, ui.ColorReset)
	fmt.Printf("  %sConfig saved.%s\n\n", ui.ColorDim, ui.ColorReset)
	fmt.Printf("  Next steps:\n")
	fmt.Printf("    %-42s %sone-time download%s\n", "flickrdownloader download -u <url>", ui.ColorDim, ui.ColorReset)
	fmt.Printf("    %-42s %sauto-download forever%s\n", "flickrdownloader watch add <url>", ui.ColorDim, ui.ColorReset)
	return nil
}
