// registry.go discovers freedesktop thumbnailers installed on the system and runs them on
// local files, so the FUSE daemon can pre-generate thumbnails without the file manager's own
// thumbnailer process ever reading through the mount.
package thumbs

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// thumbExec is one parsed [Thumbnailer Entry] from a .thumbnailer file.
type thumbExec struct {
	exec string // Exec= line with %i/%o/%s/%u placeholders
}

// Registry maps MIME types to their freedesktop thumbnailer commands.
type Registry struct {
	byMIME map[string]thumbExec
}

var defaultThumbnailerDirs = []string{
	"/usr/share/thumbnailers",
	"/usr/local/share/thumbnailers",
}

// LoadRegistry parses *.thumbnailer files from the standard directories.
func LoadRegistry() *Registry {
	r := &Registry{byMIME: make(map[string]thumbExec)}

	dirs := append([]string(nil), defaultThumbnailerDirs...)
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "share", "thumbnailers"))
	}
	for _, dir := range dirs {
		r.loadDir(dir)
	}

	slog.Debug("thumbnailer registry loaded", "mime_types", len(r.byMIME))
	return r
}

func (r *Registry) loadDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".thumbnailer") {
			r.loadFile(filepath.Join(dir, e.Name()))
		}
	}
}

func (r *Registry) loadFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	var execLine, mimeTypes, tryExec string
	inSection := false

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "[Thumbnailer Entry]" {
			inSection = true
			continue
		}
		if strings.HasPrefix(line, "[") {
			inSection = false
			continue
		}
		if !inSection {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			switch k {
			case "Exec":
				execLine = v
			case "MimeType":
				mimeTypes = v
			case "TryExec":
				tryExec = v
			}
		}
	}

	if execLine == "" || mimeTypes == "" {
		return
	}
	if tryExec != "" {
		if _, err := exec.LookPath(tryExec); err != nil {
			return
		}
	}

	te := thumbExec{exec: execLine}
	for _, mt := range strings.Split(mimeTypes, ";") {
		if mt = strings.TrimSpace(mt); mt != "" {
			r.byMIME[mt] = te
		}
	}
}

// ForExt returns the thumbnailer Exec template for the given file extension (with dot), or "".
func (r *Registry) ForExt(ext string) string {
	if r == nil || len(r.byMIME) == 0 || ext == "" {
		return ""
	}

	mt := mime.TypeByExtension(ext)
	if mt == "" {
		return ""
	}
	// mime.TypeByExtension may return "type; charset=utf-8"; strip params.
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	if te, ok := r.byMIME[mt]; ok {
		return te.exec
	}
	return ""
}

// Generate runs the thumbnailer for ext on a local input file, returning output PNG bytes.
// outputSize is the thumbnail edge length in pixels (passed as %s to the thumbnailer).
func (r *Registry) Generate(ctx context.Context, ext, input string, outputSize int) ([]byte, error) {
	execTpl := r.ForExt(ext)
	if execTpl == "" {
		return nil, fmt.Errorf("no thumbnailer for %q", ext)
	}

	tmp, err := os.CreateTemp("", "proton-thumb-out-*.png")
	if err != nil {
		return nil, err
	}
	out := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(out)

	line := execTpl
	line = strings.ReplaceAll(line, "%i", input)
	line = strings.ReplaceAll(line, "%o", out)
	line = strings.ReplaceAll(line, "%s", fmt.Sprintf("%d", outputSize))
	line = strings.ReplaceAll(line, "%u", "file://"+input)
	// %% is a literal %, per the spec.
	line = strings.ReplaceAll(line, "%%", "%")

	args := strings.Fields(line)
	if len(args) == 0 {
		return nil, fmt.Errorf("empty exec line for %q", ext)
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("thumbnailer %s: %w", filepath.Base(args[0]), err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("thumbnailer produced empty output for %q", ext)
	}
	return data, nil
}

// ProcessNames returns the basenames of all registered thumbnailer executables, for the FUSE
// layer to block them from reading through the mount.
func (r *Registry) ProcessNames() []string {
	if r == nil {
		return nil
	}
	seen := make(map[string]bool)
	var names []string
	for _, te := range r.byMIME {
		if name := execBasename(te.exec); name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

func execBasename(execLine string) string {
	if f := strings.Fields(execLine); len(f) > 0 {
		return filepath.Base(f[0])
	}
	return ""
}
