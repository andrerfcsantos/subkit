// Package naming composes output file paths derived from a media file's name.
// It is the single home for the base+suffix+ext arithmetic that the CLI and
// the pipeline both need.
package naming

import (
	"path/filepath"
	"strings"
)

// ReplaceExt swaps path's extension for ext, which may be given with or
// without a leading dot.
func ReplaceExt(path string, ext string) string {
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return strings.TrimSuffix(path, filepath.Ext(path)) + ext
}

// Sibling returns a path next to mediaPath named base[.suffix].ext.
func Sibling(mediaPath string, suffix string, ext string) string {
	return InDir(mediaPath, filepath.Dir(mediaPath), suffix, ext)
}

// InDir returns dir/base[.suffix].ext for mediaPath's base name. An empty
// suffix is omitted along with its dot.
func InDir(mediaPath string, dir string, suffix string, ext string) string {
	base := strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath))
	if suffix != "" {
		base += "." + suffix
	}
	return filepath.Join(dir, base+"."+ext)
}
