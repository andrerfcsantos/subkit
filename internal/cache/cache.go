package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// pathLocks serializes work on a cache path across every store in the
// process. Entries are reference counted and removed once the last holder
// releases them, so the table doesn't grow with each distinct path touched
// over a long batch.
var (
	pathLocksMu sync.Mutex
	pathLocks   = map[string]*pathLock{}
)

type pathLock struct {
	mu   sync.Mutex
	refs int
}

type Store struct {
	Root    string
	Read    bool
	Write   bool
	Refresh bool
	Rerun   map[string]bool
	tempDir string
}

func NewStore(root string, read bool, write bool, refresh bool, rerun []string) (*Store, error) {
	if root == "" {
		defaultRoot, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("resolving user cache dir: %w", err)
		}
		root = filepath.Join(defaultRoot, "subkit")
	}

	store := &Store{
		Root:    root,
		Read:    read,
		Write:   write,
		Refresh: refresh,
		Rerun:   map[string]bool{},
	}
	if !read || !write {
		// Path falls back to scratch space when the persistent cache is
		// disabled, so it has to exist up front.
		if _, err := store.Scratch(); err != nil {
			return nil, err
		}
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

// Scratch lazily creates and returns the store's temporary work directory. It
// backs both cache paths when persistence is disabled and run-local artifacts
// like temporary audio; Close removes it.
func (s *Store) Scratch() (string, error) {
	if s.tempDir != "" {
		return s.tempDir, nil
	}
	dir, err := os.MkdirTemp("", "subkit-*")
	if err != nil {
		return "", fmt.Errorf("creating temp work dir: %w", err)
	}
	s.tempDir = dir
	return dir, nil
}

// Close removes the store's scratch directory, if one was created.
func (s *Store) Close() error {
	if s.tempDir == "" {
		return nil
	}
	dir := s.tempDir
	s.tempDir = ""
	return os.RemoveAll(dir)
}

func (s *Store) Path(kind string, key string, ext string) string {
	base := s.Root
	if !s.Write && !s.Read && s.tempDir != "" {
		base = s.tempDir
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

func (s *Store) LockPath(path string) func() {
	key := lockKey(path)

	pathLocksMu.Lock()
	lock := pathLocks[key]
	if lock == nil {
		lock = &pathLock{}
		pathLocks[key] = lock
	}
	lock.refs++
	pathLocksMu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		pathLocksMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(pathLocks, key)
		}
		pathLocksMu.Unlock()
	}
}

func (s *Store) EnsureDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o755)
}

func (s *Store) WriteJSON(path string, value any) error {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	return s.WriteFile(path, buf.Bytes(), 0o644)
}

func (s *Store) WriteFile(path string, data []byte, perm fs.FileMode) error {
	if err := s.EnsureDir(path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	temp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = os.Remove(tempPath)
	}()

	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Chmod(perm); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return s.CommitFile(tempPath, path)
}

func (s *Store) CommitFile(tempPath string, path string) error {
	if err := s.EnsureDir(path); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return err
		}
		return os.Rename(tempPath, path)
	}
	return nil
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

	if dstInfo, err := os.Stat(dst); err == nil {
		srcInfo, err := os.Stat(src)
		if err != nil {
			return false, err
		}
		// Different sizes cannot be identical content, so skip the hashing and
		// its two full-file reads.
		if srcInfo.Size() == dstInfo.Size() {
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

func lockKey(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return strings.ToLower(filepath.Clean(abs))
}
