package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type conversionSnapshot struct {
	RestartOriginal bool
	ActiveServices  []string
	Role            string
	Init            string
	Present         map[string]bool
}
type conversionTransaction struct {
	root, dir string
	snapshot  conversionSnapshot
}

func validateConversionProtected(path, root string) error {
	for current := filepath.Clean(path); current != filepath.Dir(current) && current != filepath.Clean(root); current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode().Perm()&0022 != 0 {
			return errors.New("conversion backup directory is not protected")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() {
			return errors.New("conversion backup directory owner mismatch")
		}
	}
	return rejectConversionSymlinks(path, root)
}
func rejectConversionSymlinks(path, root string) error {
	for current := filepath.Clean(path); current != filepath.Dir(current) && current != filepath.Clean(root); current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeArtifactPath
		}
	}
	return nil
}
func conversionRecoveryPending(root string) bool {
	_, err := os.Lstat(rooted(root, "/var/backups/payesh-conversion"))
	return !errors.Is(err, os.ErrNotExist)
}
func lockConversion(root string) (*os.File, error) {
	dir := rooted(root, "/var/backups")
	if err := rejectConversionSymlinks(dir, root); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := validateConversionProtected(dir, root); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".payesh-conversion.lock")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, ErrUnsafeArtifactPath
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("another conversion is running")
	}
	return file, nil
}
func conversionPaths(init string) []string {
	paths := []string{"/usr/bin/payesh-agent", "/usr/bin/payesh-privd", "/usr/bin/payesh", "/usr/bin/payesh-server", "/usr/bin/payesh-install", "/usr/share/payesh/web-assets", "/etc/payesh/payesh.env", "/etc/payesh-installation.json", "/var/lib/payesh/install-state.json", "/var/lib/payesh/node-identity.json", "/var/lib/payesh/hub-ca.pem"}
	for _, name := range []string{"payesh-agent", "payesh-server", "payesh-privd"} {
		paths = append(paths, servicePath("/", init, name))
	}
	return paths
}
func copyConversionPath(source, destination string, budgets ...*conversionCopyBudget) error {
	parentInfo, err := os.Stat(filepath.Dir(source))
	if err != nil {
		return err
	}
	confined, err := os.OpenRoot(filepath.Dir(source))
	if err != nil {
		return err
	}
	defer confined.Close()
	opened, err := confined.Stat(".")
	if err != nil || !os.SameFile(parentInfo, opened) {
		return errors.New("conversion source parent changed")
	}
	var budget *conversionCopyBudget
	if len(budgets) > 0 {
		budget = budgets[0]
	}
	return copyConversionConfined(confined, filepath.Base(source), destination, budget)
}
func openConversionDirectory(confined *os.Root, name string, expected os.FileInfo) (*os.Root, error) {
	child, err := confined.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	actual, err := child.Stat(".")
	if err != nil || !os.SameFile(expected, actual) {
		child.Close()
		return nil, errors.New("conversion directory changed during snapshot")
	}
	return child, nil
}
func copyConversionConfined(confined *os.Root, source, destination string, budget *conversionCopyBudget) error {
	if budget != nil {
		budget.entries++
		if budget.entries > 10000 {
			return errors.New("conversion snapshot entry budget exceeded")
		}
	}
	info, err := confined.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return ErrUnsafeArtifactPath
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.Mkdir(destination, info.Mode().Perm()); err != nil {
			return err
		}
		childRoot, err := openConversionDirectory(confined, source, info)
		if err != nil {
			return err
		}
		defer childRoot.Close()
		directory, err := childRoot.Open(".")
		if err != nil {
			return err
		}
		entries, err := directory.ReadDir(-1)
		directory.Close()
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyConversionConfined(childRoot, entry.Name(), filepath.Join(destination, entry.Name()), budget); err != nil {
				return err
			}
		}
	} else if info.Mode().IsRegular() {

		in, err := confined.Open(source)
		if err != nil {
			return err
		}
		defer in.Close()
		opened, err := in.Stat()
		if err != nil || !os.SameFile(info, opened) {
			return errors.New("conversion source changed during snapshot")
		}
		out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		limit := int64(512 << 20)
		if budget != nil {
			limit = budget.remaining
		}
		copied, copyErr := io.Copy(out, io.LimitReader(in, limit+1))
		err = copyErr
		if budget != nil {
			budget.remaining -= copied
		}
		if copied > limit {
			err = errors.New("conversion snapshot file grew beyond budget")
		}
		if err == nil {
			err = out.Sync()
		}
		closeErr := out.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	} else {
		return errors.New("unsupported conversion snapshot file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
		if err := os.Chown(destination, int(stat.Uid), int(stat.Gid)); err != nil {
			return err
		}
	}
	return os.Chmod(destination, info.Mode().Perm())
}

type conversionCopyBudget struct {
	remaining int64
	entries   int
}

func beginConversion(root string, state installState, restart ...bool) (*conversionTransaction, error) {
	dir := rooted(root, "/var/backups/payesh-conversion")
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return nil, err
	}
	var snapshotBytes uint64
	for _, path := range conversionPaths(state.Init) {
		err := filepath.Walk(rooted(root, path), func(_ string, info os.FileInfo, walkErr error) error {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return ErrUnsafeArtifactPath
			}
			if info.Mode().IsRegular() {
				snapshotBytes += uint64(info.Size())
				if snapshotBytes > 512<<20 {
					return errors.New("conversion snapshot exceeds 512 MiB budget")
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	var storage syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(dir), &storage); err != nil {
		return nil, err
	}
	if uint64(storage.Bavail)*uint64(storage.Bsize) < snapshotBytes*2+(64<<20) {
		return nil, errors.New("insufficient conversion backup headroom")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, fmt.Errorf("conversion recovery required: %w", err)
	}
	tx := &conversionTransaction{root: root, dir: dir, snapshot: conversionSnapshot{Role: state.Role, Init: state.Init, Present: map[string]bool{}}}
	if len(restart) > 0 {
		tx.snapshot.RestartOriginal = restart[0]
	}
	budget := &conversionCopyBudget{remaining: 512 << 20}
	for _, path := range conversionPaths(state.Init) {
		if err := rejectConversionSymlinks(rooted(root, path), root); err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
		_, err := os.Lstat(rooted(root, path))
		if errors.Is(err, os.ErrNotExist) {
			tx.snapshot.Present[path] = false
			continue
		}
		if err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
		tx.snapshot.Present[path] = true
		if err := copyConversionPath(rooted(root, path), filepath.Join(dir, "files", path), budget); err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
	}
	data, err := json.Marshal(tx.snapshot)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if err := writeAtomic(filepath.Join(dir, "journal.json"), data, 0600); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return tx, nil
}
func (tx *conversionTransaction) rollback(opts InstallOptions) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	remover := opts.ServiceRemover
	if remover == nil {
		remover = commandServiceRemover{runner: chooseRunner(opts.CommandRunner)}
	}
	if err := remover.Remove(ctx, tx.root, tx.snapshot.Init, serviceNames("node"), true); err != nil {
		return fmt.Errorf("stop candidate services: %w", err)
	}
	for _, path := range conversionPaths(tx.snapshot.Init) {
		if err := rejectConversionSymlinks(rooted(tx.root, path), tx.root); err != nil {
			return err
		}
		if err := rejectConversionSymlinks(filepath.Join(tx.dir, "files", path), tx.root); err != nil {
			return err
		}
		if _, err := removeOwnedPath(rooted(tx.root, path), tx.root); err != nil {
			return err
		}
		if tx.snapshot.Present[path] {
			if err := copyConversionPath(filepath.Join(tx.dir, "files", path), rooted(tx.root, path)); err != nil {
				return err
			}
		}
	}
	manager := opts.ServiceManager
	if manager == nil {
		manager = commandServiceManager{runner: chooseRunner(opts.CommandRunner)}
	}
	services := tx.snapshot.ActiveServices
	if services == nil && (opts.Start || tx.snapshot.RestartOriginal) {
		services = serviceNames(tx.snapshot.Role)
	}
	if len(services) > 0 {
		if err := manager.Apply(ctx, tx.root, tx.snapshot.Init, services, true); err != nil {
			return fmt.Errorf("restart original role: %w", err)
		}
	}
	return os.RemoveAll(tx.dir)
}

// RecoverConversion restores only the fixed installation/configuration scope.
// Database, logs, TLS, spool and unrelated files are never reverted.
func RecoverConversion(root string, opts InstallOptions) error {
	dir := rooted(root, "/var/backups/payesh-conversion")
	if err := rejectConversionSymlinks(dir, root); err != nil {
		return err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := validateConversionProtected(dir, root); err != nil {
		return err
	}
	journal := filepath.Join(dir, "journal.json")
	info, statErr := os.Lstat(journal)
	if statErr == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || info.Size() > 65536 || info.Mode().Perm()&0077 != 0 || !ok || int(stat.Uid) != os.Geteuid() {
			return errors.New("untrusted conversion journal")
		}
	}
	data, err := os.ReadFile(journal)
	if errors.Is(err, os.ErrNotExist) {
		if _, e := os.Stat(dir); e == nil {
			return errors.New("incomplete conversion snapshot requires operator recovery")
		}
		return nil
	}
	if err != nil {
		return err
	}
	var snapshot conversionSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return err
	}
	if (snapshot.Role != "hub" && snapshot.Role != "standalone") || (snapshot.Init != "systemd" && snapshot.Init != "openrc") {
		return errors.New("invalid conversion recovery scope")
	}
	allowedServices := map[string]bool{}
	for _, service := range serviceNames(snapshot.Role) {
		allowedServices[service] = true
	}
	seen := map[string]bool{}
	for _, service := range snapshot.ActiveServices {
		if !allowedServices[service] || seen[service] {
			return errors.New("invalid original service recovery scope")
		}
		seen[service] = true
	}
	paths := conversionPaths(snapshot.Init)
	if len(snapshot.Present) != len(paths) {
		return errors.New("incomplete conversion recovery inventory")
	}
	for _, path := range paths {
		present, ok := snapshot.Present[path]
		if !ok {
			return errors.New("missing conversion recovery path")
		}
		if present {
			saved := filepath.Join(dir, "files", path)
			if err := rejectConversionSymlinks(saved, root); err != nil {
				return err
			}
			if _, err := os.Lstat(saved); err != nil {
				return err
			}
		}
	}
	return (&conversionTransaction{root: root, dir: dir, snapshot: snapshot}).rollback(opts)
}
