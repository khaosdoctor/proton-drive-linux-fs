// Command gen regenerates internal/about/licenses from the module's dependency list: one LICENSE
// file per module, copied out of the local module cache, plus an index.json listing them all.
// Run it with `go generate ./internal/about`.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// goModule is one entry in `go list -m -json all`'s output.
type goModule struct {
	Path    string
	Version string
	Dir     string // local module cache checkout; empty for the main module or a replaced one
	Main    bool
}

// licenseEntry is one recorded license, written to licenses/index.json.
type licenseEntry struct {
	Module  string `json:"module"`
	Version string `json:"version"`
	File    string `json:"file"`
}

// licenseFilenames are tried in order against each module's checkout.
var licenseFilenames = []string{"LICENSE", "LICENSE.md", "LICENSE.txt", "COPYING", "LICENSE-MIT", "LICENSE-APACHE"}

const outDir = "licenses"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	mods, err := listModules()
	if err != nil {
		return err
	}

	if err := os.RemoveAll(outDir); err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	var entries []licenseEntry
	for _, m := range mods {
		if m.Main || m.Dir == "" {
			continue
		}

		src := findLicense(m.Dir)
		if src == "" {
			continue
		}

		safeName := strings.ReplaceAll(m.Path, "/", "_")
		dstDir := filepath.Join(outDir, safeName)
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			return err
		}
		dst := filepath.Join(dstDir, "LICENSE")
		if err := copyFile(src, dst); err != nil {
			return err
		}

		entries = append(entries, licenseEntry{Module: m.Path, Version: m.Version, File: filepath.ToSlash(dst)})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Module < entries[j].Module })

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	fmt.Fprintf(os.Stderr, "wrote %d licenses\n", len(entries))
	return os.WriteFile(filepath.Join(outDir, "index.json"), data, 0o644)
}

// listModules runs `go list -m -json all` and decodes its stream of concatenated JSON objects,
// one per module in the build list.
func listModules() ([]goModule, error) {
	cmd := exec.Command("go", "list", "-m", "-json", "all")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var mods []goModule
	dec := json.NewDecoder(bufio.NewReader(stdout))
	for dec.More() {
		var m goModule
		if err := dec.Decode(&m); err != nil {
			return nil, err
		}
		mods = append(mods, m)
	}

	if err := cmd.Wait(); err != nil {
		return nil, err
	}
	return mods, nil
}

func findLicense(dir string) string {
	for _, name := range licenseFilenames {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	_, err = io.Copy(out, in)
	return err
}
