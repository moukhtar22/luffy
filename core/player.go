package core

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

var mpv_executable string = "mpv"
var vlc_executable string = "vlc"

func checkAndroid() bool {
	cmd := exec.Command("uname", "-o")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(output)) == "Android"
}

// watchLaterDir returns a temporary directory used exclusively for this
// luffy process's mpv watch-later files. It is created on first call.
func watchLaterDir() string {
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("luffy-watchlater-%d", os.Getpid()))
	os.MkdirAll(dir, 0o700) //nolint:errcheck
	return dir
}

// readWatchLaterSecs scans all files in dir for a "start=<seconds>" line and
// returns the first value found. Returns 0 if nothing is found.
func readWatchLaterSecs(dir string) float64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "start=") {
				val := strings.TrimPrefix(line, "start=")
				if secs, err := strconv.ParseFloat(val, 64); err == nil {
					f.Close()
					return secs
				}
			}
		}
		f.Close()
	}
	return 0
}

// buildPlayerCmd constructs the player command for the current platform/config.
// wlDir, if non-empty, is the watch-later directory passed to mpv so it writes
// position on quit.
// startSecs, if > 0, tells mpv to seek to that position before playing.
// It does NOT start the process.
func buildPlayerCmd(url, title, referer, userAgent string, subtitles []string, debug bool, wlDir string, startSecs float64) (*exec.Cmd, error) {
	if runtime.GOOS == "windows" {
		mpv_executable = "mpv.exe"
		vlc_executable = "vlc.exe"
	} else {
		mpv_executable = "mpv"
		vlc_executable = "vlc"
	}

	var cmd *exec.Cmd

	if checkAndroid() {
		if debug {
			fmt.Printf("Starting VLC on Android for %s...\n", title)
		}
		args := []string{
			"start",
			"--user", "0",
			"-a", "android.intent.action.VIEW",
			"-d", url,
			"-n", "org.videolan.vlc/org.videolan.vlc.gui.video.VideoPlayerActivity",
			"-e", "title", fmt.Sprintf("Playing %s", title),
		}
		if len(subtitles) > 0 {
			args = append(args, "--es", "subtitles_location", subtitles[0])
		}
		cmd = exec.Command("am", args...)
		return cmd, nil
	}

	switch runtime.GOOS {
	case "darwin":
		args := []string{
			"--no-stdin",
			"--keep-running",
			fmt.Sprintf("--mpv-referrer=%s", referer),
			fmt.Sprintf("--mpv-user-agent=%s", userAgent),
			url,
			fmt.Sprintf("--mpv-force-media-title=Playing %s", title),
		}
		for _, sub := range subtitles {
			args = append(args, fmt.Sprintf("--mpv-sub-files=%s", sub))
		}
		cmd = exec.Command("iina", args...)

	default:
		cfg := LoadConfig()
		if cfg.Player == "vlc" {
			args := []string{
				url,
				fmt.Sprintf("--http-referrer=%s", referer),
				fmt.Sprintf("--http-user-agent=%s", userAgent),
				fmt.Sprintf("--meta-title=Playing %s", title),
			}
			for _, sub := range subtitles {
				if sub != "" {
					if strings.HasPrefix(sub, "http://") || strings.HasPrefix(sub, "https://") {
						args = append(args, fmt.Sprintf("--input-slave=%s", sub))
					} else {
						args = append(args, fmt.Sprintf("--sub-file=%s", sub))
					}
				}
			}
			cmd = exec.Command(vlc_executable, args...)
		} else {
			// Default to mpv.
			args := []string{
				url,
				fmt.Sprintf("--referrer=%s", referer),
				fmt.Sprintf("--user-agent=%s", userAgent),
				fmt.Sprintf("--force-media-title=Playing %s", title),
			}
			for _, sub := range subtitles {
				if sub != "" {
					args = append(args, fmt.Sprintf("--sub-file=%s", sub))
				}
			}
			// Watch-later: mpv writes position to a file on quit.
			if wlDir != "" {
				args = append(args,
					fmt.Sprintf("--watch-later-directory=%s", wlDir),
					"--write-filename-in-watch-later-config",
					"--save-position-on-quit",
				)
			}
			// Resume from saved position (seconds).
			if startSecs > 0 {
				args = append(args, fmt.Sprintf("--start=%f", startSecs))
			}
			cmd = exec.Command(mpv_executable, args...)
		}
	}

	return cmd, nil
}

// isMPV returns true when the current platform/config uses mpv for playback.
func isMPV() bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" || checkAndroid() {
		return false
	}
	cfg := LoadConfig()
	return cfg.Player != "vlc"
}

// Play starts the player and blocks until it exits.
// Returns the final playback position in seconds; 0 if not tracked.
func Play(url, title, referer, userAgent string, subtitles []string, debug bool, startSecs float64) (float64, error) {
	wlDir := ""
	if isMPV() {
		wlDir = watchLaterDir()
		defer os.RemoveAll(wlDir)
	}

	cmd, err := buildPlayerCmd(url, title, referer, userAgent, subtitles, debug, wlDir, startSecs)
	if err != nil {
		return 0, err
	}
	if cmd == nil {
		return 0, fmt.Errorf("could not build player command")
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if debug && len(subtitles) > 0 {
		fmt.Printf("Subtitles found: %d\n", len(subtitles))
	}

	fmt.Printf("Starting player for %s...\n", title)

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("failed to start player: %w", err)
	}

	cmd.Wait() //nolint:errcheck

	if wlDir == "" {
		return 0, nil
	}
	return readWatchLaterSecs(wlDir), nil
}

// StartPlayer launches the player in the background with its output suppressed
// so the terminal stays available for the fzf control menu.
// wlDir is passed to mpv's --watch-later-directory if non-empty.
// startSecs, if > 0, tells mpv to seek to that position before playing.
// Returns the running *exec.Cmd so the caller can wait on or kill it.
func StartPlayer(url, title, referer, userAgent string, subtitles []string, debug bool, wlDir string, startSecs float64) (*exec.Cmd, error) {
	cmd, err := buildPlayerCmd(url, title, referer, userAgent, subtitles, debug, wlDir, startSecs)
	if err != nil {
		return nil, err
	}
	if cmd == nil {
		return nil, fmt.Errorf("could not build player command")
	}

	// Suppress player output so fzf can own the terminal.
	cmd.Stdout = nil
	cmd.Stderr = nil

	if debug && len(subtitles) > 0 {
		fmt.Printf("Subtitles found: %d\n", len(subtitles))
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start player: %w", err)
	}
	return cmd, nil
}

// PlaybackAction represents what the user chose from the playback control menu.
type PlaybackAction string

const (
	PlaybackReplay   PlaybackAction = "Replay"
	PlaybackNext     PlaybackAction = "Next"
	PlaybackPrevious PlaybackAction = "Previous"
	PlaybackQuit     PlaybackAction = "Quit"
)

// PlayResult bundles the chosen action and the final playback position.
type PlayResult struct {
	Action       PlaybackAction
	PositionSecs float64 // seconds; 0 if not tracked
}

// PlayWithControls starts the player in the background and shows an fzf menu
// with Replay / Next / Previous / Quit.  It kills the running player process
// before returning so the caller can act on the chosen action immediately.
// Returns a PlayResult containing the chosen action and the last tracked position.
func PlayWithControls(url, title, referer, userAgent string, subtitles []string, debug bool, startSecs float64) (PlayResult, error) {
	for {
		fmt.Printf("Starting player for %s...\n", title)

		wlDir := ""
		if isMPV() {
			wlDir = watchLaterDir()
		}

		cmd, err := StartPlayer(url, title, referer, userAgent, subtitles, debug, wlDir, startSecs)
		if err != nil {
			if wlDir != "" {
				os.RemoveAll(wlDir)
			}
			return PlayResult{Action: PlaybackQuit}, err
		}

		// Signal when the player exits on its own.
		done := make(chan struct{})
		go func() {
			cmd.Wait() //nolint:errcheck
			close(done)
		}()

		chosen := SelectActionCtx("Playback:", []string{
			string(PlaybackNext),
			string(PlaybackPrevious),
			string(PlaybackReplay),
			string(PlaybackQuit),
		}, done)

		// Kill the player if it is still running.
		select {
		case <-done:
			// already exited naturally (e.g. user pressed q in mpv)
		default:
			if cmd.Process != nil {
				cmd.Process.Kill() //nolint:errcheck
			}
			<-done
		}

		// Read position from watch-later file — mpv writes it on any quit (q, kill, etc.)
		var finalSecs float64
		if wlDir != "" {
			finalSecs = readWatchLaterSecs(wlDir)
			os.RemoveAll(wlDir)
		}

		if chosen == "" || chosen == string(PlaybackQuit) {
			return PlayResult{Action: PlaybackQuit, PositionSecs: finalSecs}, nil
		}
		if chosen == string(PlaybackReplay) {
			startSecs = 0
			continue
		}
		return PlayResult{Action: PlaybackAction(chosen), PositionSecs: finalSecs}, nil
	}
}
