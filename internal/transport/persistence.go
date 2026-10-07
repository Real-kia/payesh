package transport

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// persistTransportFile returns success only after both the replacement content
// and directory entry have been synced. A failed post-rename sync attempts to
// restore the prior file; persistent storage faults still require operator repair.
func persistTransportFile(path string, data []byte, syncFile func(*os.File) error) error {
	previous, err := os.ReadFile(path)
	absent := errors.Is(err, os.ErrNotExist)
	if err != nil && !absent {
		return err
	}
	published, err := replaceTransportFile(path, data, syncFile)
	if err == nil || !published {
		return err
	}
	var restoreErr error
	if absent {
		restoreErr = os.Remove(path)
		if restoreErr == nil {
			restoreErr = syncTransportDirectory(filepath.Dir(path), syncFile)
		}
	} else {
		_, restoreErr = replaceTransportFile(path, previous, syncFile)
	}
	if restoreErr != nil {
		return fmt.Errorf("transport persistence failed: %w; restoring prior state: %v", err, restoreErr)
	}
	return err
}

func syncTransport(file *os.File, syncFile func(*os.File) error) error {
	if syncFile != nil {
		return syncFile(file)
	}
	return file.Sync()
}
func syncTransportDirectory(path string, syncFile func(*os.File) error) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return syncTransport(dir, syncFile)
}
func replaceTransportFile(path string, data []byte, syncFile func(*os.File) error) (bool, error) {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".transport-*")
	if err != nil {
		return false, err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return false, err
	}
	if _, err = file.Write(data); err != nil {
		file.Close()
		return false, err
	}
	if err = syncTransport(file, syncFile); err != nil {
		file.Close()
		return false, err
	}
	if err = file.Close(); err != nil {
		return false, err
	}
	if err = os.Rename(name, path); err != nil {
		return false, err
	}
	if err = syncTransportDirectory(directory, syncFile); err != nil {
		return true, err
	}
	return true, nil
}

// Create each missing parent separately and sync its containing directory before
// publishing any state underneath it. Failed creation is retriable without
// treating an unsynced leftover empty directory as an established boundary.
func makeTransportDirectory(path string, syncFile func(*os.File) error) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("transport parent is not a directory")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if err = makeTransportDirectory(parent, syncFile); err != nil {
		return err
	}
	if err = os.Mkdir(path, 0700); err != nil {
		return err
	}
	if err = syncTransportDirectory(parent, syncFile); err != nil {
		if removeErr := os.Remove(path); removeErr != nil {
			return fmt.Errorf("create transport parent: %w; remove unsynced directory: %v", err, removeErr)
		}
		_ = syncTransportDirectory(parent, syncFile)
		return err
	}
	return nil
}
