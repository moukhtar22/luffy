package core

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/demonkingswarn/fzf.go"
)

func Prompt(label string) string {
	fmt.Print(label + ": ")
	reader := bufio.NewReader(os.Stdin)
	text, _ := reader.ReadString('\n')
	return strings.TrimSpace(text)
}

func Select(label string, items []string) int {
	components := make([]interface{}, len(items))
	for i := range items {
		components[i] = i
	}

	cfg := LoadConfig()
	prompt := label + "> "
	height := "40"
	layout := fzf.LayoutReverse
	res, _, err := fzf.FzfPrompt(
		components,
		func(i interface{}) string {
			return items[i.(int)]
		},
		cfg.FzfPath,
		&fzf.Options{
			PromptString: &prompt,
			Layout:       &layout,
			Height:       &height,
		},
	)

	if err != nil {
		fmt.Println("Selection cancelled or failed:", err)
		os.Exit(1)
	}

	if res == nil {
		fmt.Println("No selection made")
		os.Exit(1)
	}

	fmt.Print("\033[H\033[2J") // Clear screen
	return res.(int)
}

// SelectAction shows an fzf menu with the given action labels and returns
// the label of the chosen action. Returns "" if the selection is cancelled.
func SelectAction(label string, actions []string) string {
	return SelectActionCtx(label, actions, nil)
}

// SelectActionCtx is like SelectAction but takes an optional done channel.
// If done is closed before the user makes a selection, fzf is killed and ""
// is returned — this lets the caller detect that the player exited on its own.
func SelectActionCtx(label string, actions []string, done <-chan struct{}) string {
	components := make([]interface{}, len(actions))
	for i := range actions {
		components[i] = i
	}

	cfg := LoadConfig()
	prompt := label + "> "
	height := "40"
	layout := fzf.LayoutReverse
	f, err := fzf.LoadWithOptions(cfg.FzfPath, &fzf.Options{
		PromptString: &prompt,
		Layout:       &layout,
		Height:       &height,
	})
	if err != nil {
		return ""
	}

	processedToOriginal := make(map[string]int)
	lines := make([]string, len(actions))
	for i, a := range actions {
		lines[i] = a
		processedToOriginal[a] = i
	}
	f.AddLines(lines, true)

	// If done fires, kill fzf so GetOutput unblocks.
	if done != nil {
		go func() {
			select {
			case <-done:
				if p := f.Process(); p != nil && p.Process != nil {
					p.Process.Kill() //nolint:errcheck
				}
			}
		}()
	}

	query, _, err := f.GetOutput()
	if err != nil || query == "" {
		return ""
	}

	fmt.Print("\033[H\033[2J") // Clear screen
	return query
}

func SelectWithPreview(label string, items []string, previewCmd string) int {
	components := make([]interface{}, len(items))
	for i := range items {
		components[i] = i
	}

	cfg := LoadConfig()
	prompt := label + "> "
	layout := fzf.LayoutReverse

	opts := &fzf.Options{
		PromptString: &prompt,
		Layout:       &layout,
	}

	if previewCmd != "" {
		opts.Preview = &previewCmd
	}

	res, _, err := fzf.FzfPrompt(
		components,
		func(i interface{}) string {
			return items[i.(int)]
		},
		cfg.FzfPath,
		opts,
	)

	if err != nil {
		fmt.Println("Selection cancelled or failed:", err)
		os.Exit(1)
	}

	if res == nil {
		fmt.Println("No selection made")
		os.Exit(1)
	}

	fmt.Print("\033[H\033[2J") // Clear screen
	return res.(int)
}
