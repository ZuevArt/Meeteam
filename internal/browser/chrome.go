package browser

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ResolveChromePath finds a headless-capable local browser on supported systems.
func ResolveChromePath(configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		if isExecutable(configured) {
			return configured, nil
		}
		return "", fmt.Errorf("configured Chrome executable was not found: %s", configured)
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome", "msedge"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	paths := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
	}
	if runtime.GOOS == "windows" {
		for _, root := range []string{os.Getenv("PROGRAMFILES"), os.Getenv("PROGRAMFILES(X86)"), os.Getenv("LOCALAPPDATA")} {
			if root == "" {
				continue
			}
			paths = append(paths,
				filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(root, "Chromium", "Application", "chrome.exe"),
			)
		}
	}
	for _, path := range paths {
		if isExecutable(path) {
			return path, nil
		}
	}
	return "", errors.New("Chrome or Chromium was not found; set CHROME_BIN to the browser executable")
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && (runtime.GOOS == "windows" || info.Mode()&0o111 != 0)
}
