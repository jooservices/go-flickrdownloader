package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/jooservices/flickrdownloader/pkg/api"
	"github.com/jooservices/flickrdownloader/pkg/config"
	"github.com/jooservices/flickrdownloader/pkg/download"
	"github.com/jooservices/flickrdownloader/pkg/ui"
	"golang.org/x/term"
)

// readLine reads one line from r, trimming surrounding whitespace/newline.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// readSecret reads one line without echoing it to the terminal when stdin is
// a TTY, falling back to a plain read (e.g. piped input in tests/scripts).
func readSecret(r *bufio.Reader) (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return readLine(r)
}

var (
	urlFlag string
	outDir  string
	workers int
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "flickrdownloader",
		Short: "Download all photos from a Flickr user or photoset",
	}

	authCmd := &cobra.Command{
		Use:   "auth",
		Short: "Authenticate with Flickr (one-time setup)",
		RunE:  runAuth,
	}

	downloadCmd := &cobra.Command{
		Use:   "download -u <flickr-url>",
		Short: "Download photos from a Flickr URL",
		RunE:  runDownload,
	}
	downloadCmd.Flags().StringVarP(&urlFlag, "url", "u", "", "Flickr URL (user / album / photo)")
	downloadCmd.Flags().StringVarP(&outDir, "out", "o", "", "Output directory (default: ./photos)")
	downloadCmd.Flags().IntVarP(&workers, "workers", "w", 0, "Number of concurrent download workers (default: 20)")

	rootCmd.AddCommand(authCmd)
	rootCmd.AddCommand(downloadCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

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
		APIKey:      apiKey,
		APISecret:   apiSecret,
		OAuthToken:  accessToken,
		OAuthSecret: accessSecret,
		NSID:        userNSID,
		WorkerCount: 20,
		OutputDir:   "./photos",
	}

	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	fmt.Printf("\n  %s%s Authenticated as %s (NSID: %s)%s\n", ui.ColorGreen, ui.IconOk, username, userNSID, ui.ColorReset)
	fmt.Printf("  %sConfig saved. You can now use '%s'%s\n", ui.ColorDim, "flickrdownloader download -u <url>", ui.ColorReset)
	return nil
}

func runDownload(cmd *cobra.Command, args []string) error {
	if urlFlag == "" {
		return fmt.Errorf("provide -u <flickr-url>")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if workers > 0 {
		cfg.WorkerCount = workers
	}
	if outDir != "" {
		cfg.OutputDir = outDir
	}

	client := api.NewClient(cfg.APIKey, cfg.APISecret, cfg.OAuthToken, cfg.OAuthSecret)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Printf("\n  %s%s Cancelling...%s\n", ui.ColorYellow, ui.IconErr, ui.ColorReset)
		cancel()
	}()

	fmt.Printf("  %sResolving URL...%s\n", ui.ColorDim, ui.ColorReset)
	parsed, err := api.ResolveURL(ctx, urlFlag, client)
	if err != nil {
		return fmt.Errorf("resolve URL '%s': %w", urlFlag, err)
	}

	fmt.Printf("\n%s\n\n", ui.Bordered("Flickr Downloader", ui.ColorCyan))
	fmt.Printf("  %sURL:%s    %s\n", ui.ColorDim, ui.ColorReset, urlFlag)

	dl := download.New(client, cfg.OutputDir, cfg.WorkerCount)

	switch parsed.Type {
	case api.TargetPhoto:
		info, err := client.GetPhotoInfo(ctx, parsed.ID)
		if err != nil {
			return fmt.Errorf("get photo info: %w", err)
		}
		fmt.Printf("  %sType:%s   Photo\n", ui.ColorDim, ui.ColorReset)
		fmt.Printf("  %sTitle:%s  %s\n", ui.ColorDim, ui.ColorReset, info.Photo.Title.Content)
		fmt.Printf("  %sOwner:%s  %s (%s)\n", ui.ColorDim, ui.ColorReset, info.Photo.Owner.Username, info.Photo.Owner.NSID)
		fmt.Printf("  %sMedia:%s  %s\n", ui.ColorDim, ui.ColorReset, info.Photo.Media)
		fmt.Printf("  %sOutput:%s %s/%s/\n\n", ui.ColorDim, ui.ColorReset, cfg.OutputDir, info.Photo.Owner.NSID)

		return dl.DownloadPhoto(ctx, parsed.ID)

	case api.TargetPhotoset:
		psInfo, err := client.GetPhotosetInfo(ctx, parsed.ID)
		if err != nil {
			return fmt.Errorf("get photoset info: %w", err)
		}
		fmt.Printf("  %sType:%s   Album\n", ui.ColorDim, ui.ColorReset)
		fmt.Printf("  %sTitle:%s  %s\n", ui.ColorDim, ui.ColorReset, psInfo.Title.Content)
		fmt.Printf("  %sOwner:%s  %s\n", ui.ColorDim, ui.ColorReset, psInfo.Owner)
		fmt.Printf("  %sPhotos:%s %d\n", ui.ColorDim, ui.ColorReset, int(psInfo.Photos))
		fmt.Printf("  %sOutput:%s %s/%s/%s/\n\n", ui.ColorDim, ui.ColorReset, cfg.OutputDir, psInfo.Owner, download.SafeName(psInfo.Title.Content))
		fmt.Printf("  %sDownloading...%s\n\n", ui.ColorDim, ui.ColorReset)
		_, err = dl.DownloadByPhotoset(ctx, parsed.ID)
		return err

	case api.TargetUser:
		nsid := parsed.ID
		if nsid == "" {
			return fmt.Errorf("resolved an empty NSID for '%s' — refusing to query the API", urlFlag)
		}
		fmt.Printf("  %sType:%s   User\n", ui.ColorDim, ui.ColorReset)
		fmt.Printf("  %sNSID:%s   %s\n", ui.ColorDim, ui.ColorReset, nsid)
		fmt.Printf("  %sOutput:%s %s/%s/\n\n", ui.ColorDim, ui.ColorReset, cfg.OutputDir, nsid)

		sets, err := client.GetPhotosets(ctx, nsid)
		if err != nil {
			return fmt.Errorf("discover photosets: %w", err)
		}

		firstPage, err := client.GetPhotosByUser(ctx, nsid, 1)
		if err != nil {
			return fmt.Errorf("get photos: %w", err)
		}
		totalPhotos := int(firstPage.Photos.Total)

		if len(sets) > 0 {
			fmt.Printf("  %sPhotosets:%s %d\n", ui.ColorDim, ui.ColorReset, len(sets))
		}
		fmt.Printf("  %sPhotos:%s %d\n", ui.ColorDim, ui.ColorReset, totalPhotos)
		fmt.Printf("\n  %sDownloading...%s\n\n", ui.ColorDim, ui.ColorReset)
		_, err = dl.DownloadByUser(ctx, nsid)
		return err
	}

	return nil
}
