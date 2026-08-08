//go:build darwin || linux

package secretstore

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

type fileMetadata struct {
	exists  bool
	size    int64
	modTime int64
	device  uint64
	inode   uint64
}

type fileObservation struct {
	exists   bool
	digest   [sha256.Size]byte
	byName   map[string][sha256.Size]byte
	metadata fileMetadata
}

func metadataFromFileInfo(info os.FileInfo) fileMetadata {
	metadata := fileMetadata{exists: true, size: info.Size(), modTime: info.ModTime().UnixNano()}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		metadata.device = uint64(stat.Dev)
		metadata.inode = uint64(stat.Ino)
	}
	return metadata
}

func observeFileSnapshot(data []byte, values map[string]string, metadata fileMetadata) fileObservation {
	byName := make(map[string][sha256.Size]byte, len(values))
	for name, value := range values {
		byName[name] = sha256.Sum256([]byte(value))
	}
	return fileObservation{
		exists:   metadata.exists,
		digest:   sha256.Sum256(data),
		byName:   byName,
		metadata: metadata,
	}
}

func (s *FileStore) startWatching(ctx context.Context, debounce, pollInterval time.Duration, changed func(context.Context, []string)) {
	var watcher *fsnotify.Watcher
	if !s.disableWatchEvents {
		var err error
		watcher, err = fsnotify.NewWatcher()
		if err == nil {
			err = watcher.Add(s.dir)
		}
		if err != nil {
			if watcher != nil {
				_ = watcher.Close()
				watcher = nil
			}
			slog.Warn("watch managed secret file with metadata fallback", "error", err)
		}
	}
	go s.watchLoop(ctx, watcher, debounce, pollInterval, changed)
}

func (s *FileStore) watchLoop(ctx context.Context, watcher *fsnotify.Watcher, debounce, pollInterval time.Duration, changed func(context.Context, []string)) {
	if watcher != nil {
		defer watcher.Close()
	}
	var events <-chan fsnotify.Event
	var watcherErrors <-chan error
	if watcher != nil {
		events = watcher.Events
		watcherErrors = watcher.Errors
	}
	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	maxDelay := 5 * debounce
	if maxDelay < debounce {
		maxDelay = debounce
	}
	var quietTimer, maxTimer *time.Timer
	var quietC, maxC <-chan time.Time
	stopTimer := func(timer *time.Timer) {
		if timer == nil || timer.Stop() {
			return
		}
		select {
		case <-timer.C:
		default:
		}
	}
	clearTimers := func() {
		stopTimer(quietTimer)
		stopTimer(maxTimer)
		quietTimer, maxTimer = nil, nil
		quietC, maxC = nil, nil
	}
	schedule := func() {
		stopTimer(quietTimer)
		if quietTimer == nil {
			quietTimer = time.NewTimer(debounce)
		} else {
			quietTimer.Reset(debounce)
		}
		quietC = quietTimer.C
		if maxTimer == nil {
			maxTimer = time.NewTimer(maxDelay)
			maxC = maxTimer.C
		}
	}
	flush := func() {
		clearTimers()
		names, err := s.observeExternalChange()
		if err != nil {
			slog.Warn("reload externally changed managed secret file", "error", err)
			return
		}
		if len(names) > 0 {
			changed(ctx, names)
		}
	}
	defer clearTimers()
	dataPath := filepath.Join(s.dir, fileStoreDataName)

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				events = nil
				watcherErrors = nil
				continue
			}
			if filepath.Clean(event.Name) == dataPath && event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) != 0 {
				schedule()
			}
		case err, ok := <-watcherErrors:
			if !ok {
				watcherErrors = nil
				continue
			}
			slog.Warn("watch managed secret file", "error", err)
		case <-poll.C:
			if s.metadataChanged() {
				flush()
			}
		case <-quietC:
			flush()
		case <-maxC:
			flush()
		}
	}
}

func (s *FileStore) metadataChanged() bool {
	info, err := os.Lstat(filepath.Join(s.dir, fileStoreDataName))
	metadata := fileMetadata{}
	if err == nil {
		metadata = metadataFromFileInfo(info)
	} else if !os.IsNotExist(err) {
		return true
	}
	s.observeMu.Lock()
	defer s.observeMu.Unlock()
	return metadata != s.observed.metadata
}

func (s *FileStore) observeExternalChange() ([]string, error) {
	if err := ValidateStateDir(s.dir); err != nil {
		return nil, err
	}
	s.observeMu.Lock()
	defer s.observeMu.Unlock()
	values, data, metadata, err := s.loadSnapshot(OperationGet, "")
	if err != nil {
		return nil, err
	}
	defer clear(values)
	next := observeFileSnapshot(data, values, metadata)
	if next.exists == s.observed.exists && next.digest == s.observed.digest {
		s.observed.metadata = next.metadata
		return nil, nil
	}
	names := changedSecretNames(s.observed.byName, next.byName)
	s.observed = next
	return names, nil
}

func changedSecretNames(before, after map[string][sha256.Size]byte) []string {
	changed := make([]string, 0)
	for name, digest := range before {
		if next, ok := after[name]; !ok || next != digest {
			changed = append(changed, name)
		}
	}
	for name, digest := range after {
		if previous, ok := before[name]; !ok || previous != digest {
			if _, existed := before[name]; !existed {
				changed = append(changed, name)
			}
		}
	}
	sort.Strings(changed)
	return changed
}
