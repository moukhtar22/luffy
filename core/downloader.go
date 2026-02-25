package core

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Download downloads a stream to disk using a native Go implementation.
// HLS streams (*.m3u8) are downloaded by fetching and concatenating segments
// concurrently. Other URLs are downloaded directly, using parallel byte-range
// requests when the server supports them and the file is large enough.
//
// Subtitle files (VTT/SRT) are downloaded alongside the video when provided.
func Download(basePath, dlPath, name, url, referer, userAgent string, subtitles []string, debug bool) error {
	if dlPath == "" {
		dlPath = filepath.Join(basePath, "Downloads", "luffy")
	} else {
		dlPath = filepath.Join(dlPath, "luffy")
	}
	if err := os.MkdirAll(dlPath, 0755); err != nil {
		return fmt.Errorf("failed to create download directory: %w", err)
	}

	cleanName := sanitizeFilename(name)

	// Choose extension: HLS produces a raw MPEG-TS stream that most players
	// handle fine as .ts; direct files are saved as .mp4.
	ext := ".mp4"
	if strings.Contains(url, ".m3u8") {
		ext = ".ts"
	}

	outputPath := filepath.Join(dlPath, cleanName+ext)
	outputPath = ensureUnique(outputPath)

	fmt.Printf("[download] Saving to: %s\n", outputPath)

	ctx := context.Background()

	headers := map[string]string{
		"User-Agent": userAgent,
	}
	if referer != "" {
		headers["Referer"] = referer
	}

	var err error
	if strings.Contains(url, ".m3u8") {
		err = downloadHLSWithProgress(ctx, url, outputPath, headers, debug)
	} else {
		err = downloadDirect(ctx, url, outputPath, headers, debug)
	}
	if err != nil {
		_ = os.Remove(outputPath)
		return fmt.Errorf("download failed: %w", err)
	}

	// Download subtitle files.
	if len(subtitles) > 0 {
		for i, subURL := range subtitles {
			if subURL == "" {
				continue
			}
			subExt := ".vtt"
			if strings.HasSuffix(subURL, ".srt") {
				subExt = ".srt"
			}
			subPath := filepath.Join(dlPath, cleanName)
			if i > 0 {
				subPath += fmt.Sprintf(".eng%d%s", i, subExt)
			} else {
				subPath += ".eng" + subExt
			}
			if debug {
				fmt.Printf("[download] Downloading subtitle to %s...\n", subPath)
			}
			if subErr := downloadFileWithRetry(subURL, subPath, 3); subErr != nil {
				fmt.Printf("[warning] Failed to download subtitle: %v\n", subErr)
			}
		}
	}

	fmt.Println("[download] Complete!")
	return nil
}

// sanitizeFilename replaces characters that are problematic in filenames.
func sanitizeFilename(name string) string {
	r := strings.NewReplacer(
		" ", "-",
		"\"", "",
		"/", "-",
		"\\", "-",
		":", "-",
		"*", "",
		"?", "",
		"<", "",
		">", "",
		"|", "-",
	)
	return r.Replace(name)
}

// ensureUnique appends a counter to the path stem until it finds a name that
// does not exist on disk.
func ensureUnique(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s.%d%s", stem, i, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

// getTerminalWidth returns the terminal column width, defaulting to 80.
func getTerminalWidth() int {
	if runtime.GOOS == "windows" {
		return 80
	}
	// Use os.Stdout fd to query terminal size via ioctl would require cgo;
	// fall back to a simple stty call on Unix-likes.
	return 80
}

// downloadHLSWithProgress downloads an HLS stream and prints segment progress.
func downloadHLSWithProgress(ctx context.Context, url, output string, headers map[string]string, debug bool) error {
	referer := headers["Referer"]

	printProgress := func(downloaded, total int) {
		if total == 0 {
			return
		}
		pct := float64(downloaded) / float64(total) * 100.0
		bar := progressBar(downloaded, total, 30)
		fmt.Printf("\r[download] %s %5.1f%% (%d/%d segments)", bar, pct, downloaded, total)
	}

	err := DownloadHLS(ctx, url, output, referer, printProgress)
	fmt.Println() // newline after progress bar
	return err
}

// downloadDirect downloads a regular (non-HLS) URL.
// It attempts concurrent byte-range downloads for large files; falls back to
// a single-connection stream otherwise.
func downloadDirect(ctx context.Context, url, output string, headers map[string]string, debug bool) error {
	// HEAD request to check range support and content length.
	headReq, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return downloadDirectSingle(ctx, url, output, headers, debug)
	}
	for k, v := range headers {
		headReq.Header.Set(k, v)
	}

	headClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := headClient.Do(headReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return downloadDirectSingle(ctx, url, output, headers, debug)
	}
	_ = resp.Body.Close()

	contentLength := resp.ContentLength
	acceptRanges := resp.Header.Get("Accept-Ranges")

	// Use parallel download when ranges are supported and file is >10 MB.
	if contentLength > 10*1024*1024 && acceptRanges == "bytes" {
		if debug {
			fmt.Printf("[download] Using parallel download (%d bytes)\n", contentLength)
		}
		return downloadDirectConcurrent(ctx, url, output, headers, contentLength, debug)
	}

	return downloadDirectSingle(ctx, url, output, headers, debug)
}

// downloadDirectConcurrent downloads url in numParts parallel byte-range chunks.
func downloadDirectConcurrent(ctx context.Context, url, output string, headers map[string]string, totalBytes int64, debug bool) error {
	f, err := os.Create(output)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Pre-allocate.
	if err := f.Truncate(totalBytes); err != nil {
		return fmt.Errorf("failed to allocate file: %w", err)
	}

	const numParts = 8
	partSize := totalBytes / numParts

	var wg sync.WaitGroup
	errChan := make(chan error, numParts)
	var downloadedBytes int64

	partCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Progress monitor goroutine.
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-partCtx.Done():
				return
			case <-ticker.C:
				current := atomic.LoadInt64(&downloadedBytes)
				pct := float64(current) / float64(totalBytes) * 100.0
				bar := progressBar(int(current), int(totalBytes), 30)
				fmt.Printf("\r[download] %s %5.1f%%", bar, pct)
			}
		}
	}()

	for i := 0; i < numParts; i++ {
		start := int64(i) * partSize
		end := start + partSize - 1
		if i == numParts-1 {
			end = totalBytes - 1
		}

		wg.Add(1)
		go func(partIdx int, start, end int64) {
			defer wg.Done()

			req, err := http.NewRequestWithContext(partCtx, "GET", url, nil)
			if err != nil {
				errChan <- err
				cancel()
				return
			}
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			if req.Header.Get("User-Agent") == "" {
				req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")
			}

			client := &http.Client{Timeout: 0}
			resp, err := client.Do(req)
			if err != nil {
				errChan <- err
				cancel()
				return
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
				errChan <- fmt.Errorf("unexpected status %d", resp.StatusCode)
				cancel()
				return
			}

			buf := make([]byte, 32*1024)
			offset := start
			for {
				n, err := resp.Body.Read(buf)
				if n > 0 {
					if _, wErr := f.WriteAt(buf[:n], offset); wErr != nil {
						errChan <- wErr
						cancel()
						return
					}
					offset += int64(n)
					atomic.AddInt64(&downloadedBytes, int64(n))
				}
				if err != nil {
					if err == io.EOF {
						break
					}
					errChan <- err
					cancel()
					return
				}
			}
		}(i, start, end)
	}

	wg.Wait()
	cancel() // stop monitor
	<-monitorDone
	fmt.Println() // newline after progress bar

	close(errChan)
	for err := range errChan {
		if err != nil {
			return err
		}
	}
	return nil
}

// downloadDirectSingle downloads url using a single HTTP connection, streaming
// directly to the output file.
func downloadDirectSingle(ctx context.Context, url, output string, headers map[string]string, debug bool) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")
	}

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	totalBytes := resp.ContentLength

	out, err := os.Create(output)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer func() { _ = out.Close() }()

	buf := make([]byte, 32*1024)
	var downloaded int64
	lastReport := time.Now()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := out.Write(buf[:n]); wErr != nil {
				return fmt.Errorf("failed to write to file: %w", wErr)
			}
			downloaded += int64(n)

			if time.Since(lastReport) >= 500*time.Millisecond {
				if totalBytes > 0 {
					bar := progressBar(int(downloaded), int(totalBytes), 30)
					pct := float64(downloaded) / float64(totalBytes) * 100.0
					fmt.Printf("\r[download] %s %5.1f%% (%s / %s)",
						bar, pct, formatSize(downloaded), formatSize(totalBytes))
				} else {
					fmt.Printf("\r[download] %s downloaded", formatSize(downloaded))
				}
				lastReport = time.Now()
			}
		}

		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("error reading response: %w", err)
		}
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("error closing output file: %w", err)
	}

	if totalBytes > 0 {
		bar := progressBar(int(downloaded), int(totalBytes), 30)
		fmt.Printf("\r[download] %s 100.0%% (%s)", bar, formatSize(downloaded))
	}
	fmt.Println()
	return nil
}

// progressBar returns a simple ASCII progress bar string.
func progressBar(done, total, width int) string {
	if total == 0 {
		return "[" + strings.Repeat(" ", width) + "]"
	}
	filled := done * width / total
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("=", filled) + strings.Repeat(" ", width-filled) + "]"
}

// formatSize returns a human-readable byte count.
func formatSize(n int64) string {
	const (
		MB = 1024 * 1024
		GB = 1024 * 1024 * 1024
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.2f GB", float64(n)/float64(GB))
	case n >= MB:
		return fmt.Sprintf("%.2f MB", float64(n)/float64(MB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// downloadFileWithRetry downloads a single file with up to maxRetries attempts.
func downloadFileWithRetry(url, path string, maxRetries int) error {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		if err := downloadFile(url, path); err == nil {
			return nil
		} else {
			lastErr = err
		}
		_ = os.Remove(path)
		time.Sleep(time.Duration(i+1) * time.Second)
	}
	return lastErr
}

// downloadFile downloads a single URL to path using a plain GET request.
func downloadFile(url, path string) error {
	client := NewClient()
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	_, err = io.Copy(out, resp.Body)
	return err
}
