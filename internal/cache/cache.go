package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Store struct {
	Root    string
	Read    bool
	Write   bool
	Refresh bool
	Rerun   map[string]bool
	TempDir string
}

func NewStore(root string, read bool, write bool, refresh bool, rerun []string) (*Store, error) {
	if root == "" {
		defaultRoot, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("resolving user cache dir: %w", err)
		}
		root = filepath.Join(defaultRoot, "subkit")
	}

	tempDir := ""
	if !read || !write {
		dir, err := os.MkdirTemp("", "subkit-*")
		if err != nil {
			return nil, fmt.Errorf("creating temp work dir: %w", err)
		}
		tempDir = dir
	}

	store := &Store{
		Root:    root,
		Read:    read,
		Write:   write,
		Refresh: refresh,
		Rerun:   map[string]bool{},
		TempDir: tempDir,
	}
	for _, item := range rerun {
		for _, part := range strings.Split(item, ",") {
			part = strings.TrimSpace(strings.ToLower(part))
			if part != "" {
				store.Rerun[part] = true
			}
		}
	}

	if write {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, fmt.Errorf("creating cache dir: %w", err)
		}
	}

	return store, nil
}

func (s *Store) CanRead(step string) bool {
	if !s.Read || s.Refresh {
		return false
	}
	step = strings.ToLower(step)
	return !s.Rerun["all"] && !s.Rerun[step]
}

func (s *Store) CanWrite() bool {
	return s.Write
}

func (s *Store) Path(kind string, key string, ext string) string {
	base := s.Root
	if !s.Write && !s.Read && s.TempDir != "" {
		base = s.TempDir
	}
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return filepath.Join(base, kind, key+ext)
}

func (s *Store) Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (s *Store) EnsureDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o755)
}

func (s *Store) WriteJSON(path string, value any) error {
	if err := s.EnsureDir(path); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func (s *Store) ReadJSON(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewDecoder(file).Decode(value)
}

func HashJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func FileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func CopyFile(src string, dst string) error {
	if samePath(src, dst) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		_ = out.Close()
	}()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func CopyFileIfDifferent(src string, dst string) (bool, error) {
	if samePath(src, dst) {
		return false, nil
	}

	if _, err := os.Stat(dst); err == nil {
		srcHash, err := FileSHA256(src)
		if err != nil {
			return false, err
		}
		dstHash, err := FileSHA256(dst)
		if err != nil {
			return false, err
		}
		if srcHash == dstHash {
			return false, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	return true, CopyFile(src, dst)
}

func RemoveAll(root string) error {
	if root == "" {
		return errors.New("cache root is empty")
	}
	return os.RemoveAll(root)
}

func DirSize(root string) (int64, error) {
	var size int64
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		size += info.Size()
		return nil
	})
	return size, err
}

func samePath(a string, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA == nil && errB == nil {
		return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
