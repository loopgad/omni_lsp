package persistent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var errWriterLockBusy = errors.New("persistent: writer lock busy")

type writerLock struct {
	file   *os.File
	unlock func() error
	once   sync.Once
}

func (l *writerLock) release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		_ = l.unlock()
		_ = l.file.Close()
	})
}

func acquireWriterLock(ctx context.Context, root string) (*writerLock, error) {
	return acquireFileLock(ctx, filepath.Join(root, "writer.lock"), "writer")
}

func acquireFileLock(ctx context.Context, path, purpose string) (*writerLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("persistent: open %s lock: %w", purpose, err)
	}
	for {
		unlock, lockErr := tryWriterLock(f)
		if lockErr == nil {
			return &writerLock{file: f, unlock: unlock}, nil
		}
		if !errors.Is(lockErr, errWriterLockBusy) {
			_ = f.Close()
			return nil, fmt.Errorf("persistent: acquire %s lock: %w", purpose, lockErr)
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func tryAcquireWriterLock(root string) (*writerLock, error) {
	path := filepath.Join(root, "writer.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("persistent: open writer lock: %w", err)
	}
	unlock, err := tryWriterLock(f)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, errWriterLockBusy) {
			return nil, errWriterLockBusy
		}
		return nil, fmt.Errorf("persistent: acquire writer lock: %w", err)
	}
	return &writerLock{file: f, unlock: unlock}, nil
}
