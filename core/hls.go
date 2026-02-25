// Package core provides HLS (HTTP Live Streaming) download functionality.
// This implementation is ported from github.com/justchokingaround/greg.
package core

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// hlsSegment represents a single HLS segment.
type hlsSegment struct {
	URL      string
	Index    int
	Duration float64
}

// hlsPlaylist represents a parsed HLS media playlist.
type hlsPlaylist struct {
	Segments []hlsSegment
}

// hlsDownloader handles concurrent HLS stream downloads.
type hlsDownloader struct {
	client *http.Client
}

func newHLSDownloader() *hlsDownloader {
	return &hlsDownloader{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// DownloadHLS downloads an HLS stream to outputPath, reporting segment progress
// via progressCallback(downloadedSegments, totalSegments). Pass nil to skip
// progress reporting.
func DownloadHLS(ctx context.Context, streamURL, outputPath, referer string, progressCallback func(int, int)) error {
	d := newHLSDownloader()
	headers := map[string]string{}
	if referer != "" {
		headers["Referer"] = referer
	}
	return d.downloadWithProgress(ctx, streamURL, outputPath, headers, progressCallback)
}

// downloadWithProgress drives the full HLS download pipeline.
func (d *hlsDownloader) downloadWithProgress(ctx context.Context, url, output string, headers map[string]string, progressCallback func(int, int)) error {
	playlist, err := d.parsePlaylist(ctx, url, headers)
	if err != nil {
		return fmt.Errorf("failed to parse playlist: %w", err)
	}
	if len(playlist.Segments) == 0 {
		return fmt.Errorf("playlist has no segments to download")
	}

	if err = os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	outFile, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer func() { _ = outFile.Close() }()

	totalSegments := len(playlist.Segments)
	var downloadedSegments int32

	if progressCallback != nil {
		progressCallback(0, totalSegments)
	}

	const maxWorkers = 8

	type job struct {
		index   int
		segment hlsSegment
	}
	type result struct {
		index int
		data  []byte
		err   error
	}

	jobs := make(chan job, totalSegments)
	results := make(chan result, totalSegments)

	var wg sync.WaitGroup
	for i := 0; i < maxWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				select {
				case <-ctx.Done():
					return
				default:
				}
				data, err := d.downloadSegment(ctx, j.segment.URL, headers)
				results <- result{index: j.index, data: data, err: err}
			}
		}()
	}

	for i, seg := range playlist.Segments {
		jobs <- job{index: i, segment: seg}
	}
	close(jobs)

	// Buffer out-of-order results and write sequentially.
	segmentBuffer := make(map[int][]byte)
	nextIndex := 0
	var firstErr error

	for i := 0; i < totalSegments; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case res := <-results:
			if res.err != nil {
				if firstErr == nil {
					firstErr = res.err
				}
				continue
			}
			segmentBuffer[res.index] = res.data
			atomic.AddInt32(&downloadedSegments, 1)

			// Flush all contiguous segments starting from nextIndex.
			for {
				data, ok := segmentBuffer[nextIndex]
				if !ok {
					break
				}
				if _, wErr := outFile.Write(data); wErr != nil && firstErr == nil {
					firstErr = fmt.Errorf("failed to write segment %d: %w", nextIndex, wErr)
				}
				delete(segmentBuffer, nextIndex)
				nextIndex++
			}

			if progressCallback != nil {
				progressCallback(int(atomic.LoadInt32(&downloadedSegments)), totalSegments)
			}
		}
	}

	wg.Wait()
	return firstErr
}

// parsePlaylist fetches and parses a master or media playlist, returning the
// resolved media playlist for the highest-bandwidth variant.
func (d *hlsDownloader) parsePlaylist(ctx context.Context, url string, headers map[string]string) (*hlsPlaylist, error) {
	lines, err := d.fetchLines(ctx, url, headers)
	if err != nil {
		return nil, err
	}

	// Detect master playlist by presence of #EXT-X-STREAM-INF.
	isMaster := false
	for _, line := range lines {
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			isMaster = true
			break
		}
	}

	if isMaster {
		bestURL := selectBestVariant(lines, url)
		if bestURL == "" {
			return nil, fmt.Errorf("no suitable stream found in master playlist")
		}
		return d.parseMediaPlaylist(ctx, bestURL, headers)
	}

	return parseMediaPlaylistLines(lines, url), nil
}

// parseMediaPlaylist fetches and parses a media (non-master) playlist.
func (d *hlsDownloader) parseMediaPlaylist(ctx context.Context, url string, headers map[string]string) (*hlsPlaylist, error) {
	lines, err := d.fetchLines(ctx, url, headers)
	if err != nil {
		return nil, err
	}
	return parseMediaPlaylistLines(lines, url), nil
}

// fetchLines fetches a URL and returns its body as trimmed lines.
func (d *hlsDownloader) fetchLines(ctx context.Context, url string, headers map[string]string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	var lines []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		lines = append(lines, strings.TrimSpace(scanner.Text()))
	}
	return lines, scanner.Err()
}

// selectBestVariant picks the highest-bandwidth variant URL from a master
// playlist's lines.
func selectBestVariant(lines []string, baseURL string) string {
	type streamInfo struct {
		URL       string
		Bandwidth int
	}

	bwRe := regexp.MustCompile(`BANDWIDTH=(\d+)`)
	var streams []streamInfo

	for i, line := range lines {
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			continue
		}
		bandwidth := 0
		if m := bwRe.FindStringSubmatch(line); len(m) > 1 {
			if bw, err := strconv.Atoi(m[1]); err == nil {
				bandwidth = bw
			}
		}
		if i+1 >= len(lines) {
			continue
		}
		variantURL := strings.TrimSpace(lines[i+1])
		if !strings.HasPrefix(variantURL, "http") {
			if idx := strings.LastIndex(baseURL, "/"); idx != -1 {
				variantURL = baseURL[:idx+1] + variantURL
			}
		}
		streams = append(streams, streamInfo{URL: variantURL, Bandwidth: bandwidth})
	}

	if len(streams) == 0 {
		return ""
	}
	best := streams[0]
	for _, s := range streams[1:] {
		if s.Bandwidth > best.Bandwidth {
			best = s
		}
	}
	return best.URL
}

// parseMediaPlaylistLines extracts segments from a media playlist's lines,
// resolving relative URLs against baseURL.
func parseMediaPlaylistLines(lines []string, baseURL string) *hlsPlaylist {
	playlist := &hlsPlaylist{}
	idx := 0

	for i, line := range lines {
		if !strings.HasPrefix(line, "#EXTINF:") {
			continue
		}
		if i+1 >= len(lines) {
			break
		}
		infLine := strings.TrimPrefix(line, "#EXTINF:")
		parts := strings.SplitN(infLine, ",", 2)
		var duration float64
		if len(parts) > 0 {
			duration, _ = strconv.ParseFloat(strings.TrimRight(parts[0], ", "), 64)
		}

		segURL := strings.TrimSpace(lines[i+1])
		if segURL == "" || strings.HasPrefix(segURL, "#") {
			continue
		}
		if !strings.HasPrefix(segURL, "http") {
			base := baseURL
			if j := strings.LastIndex(base, "/"); j != -1 {
				base = base[:j+1]
			} else {
				base += "/"
			}
			segURL = base + segURL
		}

		playlist.Segments = append(playlist.Segments, hlsSegment{
			URL:      segURL,
			Index:    idx,
			Duration: duration,
		})
		idx++
	}

	return playlist
}

// downloadSegment fetches a single .ts segment with up to 2 retries.
func (d *hlsDownloader) downloadSegment(ctx context.Context, url string, headers map[string]string) ([]byte, error) {
	const maxRetries = 2
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		}

		resp, err := d.client.Do(req)
		if err != nil {
			if attempt < maxRetries {
				time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
				continue
			}
			return nil, err
		}

		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if readErr != nil {
			if attempt < maxRetries {
				time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
				continue
			}
			return nil, readErr
		}

		if resp.StatusCode != http.StatusOK {
			if attempt < maxRetries {
				time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
				continue
			}
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
		}

		return body, nil
	}

	return nil, fmt.Errorf("failed to download segment after %d attempts", maxRetries+1)
}
