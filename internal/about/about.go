// Package about builds and displays the About dialog: project name, version, commit, links, and
// the third-party licenses gathered by go:generate (see gen/main.go).
package about

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

//go:generate go run ./gen

//go:embed licenses
var licenseFS embed.FS

// LicenseEntry is one third-party module's license, as recorded in licenses/index.json.
type LicenseEntry struct {
	Module  string `json:"module"`
	Version string `json:"version"`
	File    string `json:"file"` // path within licenseFS
}

// Licenses returns every recorded third-party license, sorted by module name (gen/main.go
// already sorts them; this stays correct even if index.json is ever hand-edited).
func Licenses() ([]LicenseEntry, error) {
	data, err := licenseFS.ReadFile("licenses/index.json")
	if err != nil {
		return nil, err
	}

	var entries []LicenseEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// LicenseText returns one entry's license file content.
func LicenseText(e LicenseEntry) (string, error) {
	data, err := licenseFS.ReadFile(e.File)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Text builds the About dialog's full plain-text content: project info followed by every
// third-party license.
func Text(version, commit string) string {
	var b strings.Builder

	fmt.Fprintln(&b, "proton-drive-fs")
	fmt.Fprintln(&b, "by Lucas Santos")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "Version: %s (%s)\n", version, commit)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "GitHub: https://github.com/khaosdoctor/proton-drive-linux-fs")
	fmt.Fprintln(&b, "Docs:   https://oss.lsantos.dev/proton-drive-linux-fs/")
	fmt.Fprintln(&b, "Issues: https://github.com/khaosdoctor/proton-drive-linux-fs/issues")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Third-party licenses")
	fmt.Fprintln(&b, "---------------------")

	entries, err := Licenses()
	if err != nil {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "(license list unavailable:", err, ")")
		return b.String()
	}

	for _, e := range entries {
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "%s %s\n", e.Module, e.Version)

		text, err := LicenseText(e)
		if err != nil {
			fmt.Fprintln(&b, "(license text unavailable)")
			continue
		}
		fmt.Fprintln(&b, text)
	}

	return b.String()
}

// Show displays the About dialog: zenity when it's on PATH, otherwise an HTML file opened with
// xdg-open.
func Show(version, commit string) error {
	text := Text(version, commit)

	if _, err := exec.LookPath("zenity"); err == nil {
		return showZenity(text)
	}
	return showHTML(text)
}

// tmpDir returns where the About dialog's scratch files live: $XDG_RUNTIME_DIR/proton-drive-fs,
// falling back to the user cache directory.
func tmpDir() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		var err error
		base, err = os.UserCacheDir()
		if err != nil {
			return "", err
		}
	}

	dir := filepath.Join(base, "proton-drive-fs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// showZenity writes text to a temp file and shows it with zenity's blocking text-info dialog,
// removing the file once the dialog closes.
func showZenity(text string) error {
	dir, err := tmpDir()
	if err != nil {
		return err
	}

	f, err := os.CreateTemp(dir, "about-*.txt")
	if err != nil {
		return err
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()

	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	return exec.Command("zenity", "--text-info", "--title", "About proton-drive-fs", "--filename", path).Run()
}

// showHTML writes an about.html and opens it with xdg-open. xdg-open hands the file to a browser
// and returns immediately, well before the browser has read it, so the file is left in place
// under the runtime directory (tmpfs on most systems) rather than removed right after Run.
// ponytail: leaves one about.html behind per run; fine for a runtime dir that's wiped on logout.
func showHTML(text string) error {
	dir, err := tmpDir()
	if err != nil {
		return err
	}

	path := filepath.Join(dir, "about.html")
	html := "<!doctype html><meta charset=\"utf-8\"><title>About proton-drive-fs</title>" +
		"<pre style=\"font-family:monospace;white-space:pre-wrap\">" + htmlEscape(text) + "</pre>"
	if err := os.WriteFile(path, []byte(html), 0o600); err != nil {
		return err
	}

	return exec.Command("xdg-open", path).Run()
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
