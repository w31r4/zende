package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// The online backup API copies SQLite pages, including the committed WAL
// state and row identities. It does not rebuild tables or copy a live db file.
func (s *Store) preserveSchema(ctx context.Context, version int) (string, error) {
	directory := filepath.Join(filepath.Dir(s.path), "schema-backups")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("schema backup directory must be a private directory, not a symbolic link")
	}
	parent, err := os.Open(filepath.Dir(directory))
	if err != nil {
		return "", err
	}
	err = parent.Sync() // Retain a newly created backup directory across a crash.
	parentCloseErr := parent.Close()
	if err != nil {
		return "", err
	}
	if parentCloseErr != nil {
		return "", parentCloseErr
	}
	file, err := os.CreateTemp(directory, fmt.Sprintf(".schema-%d-to-%d-*.db", version, SchemaVersion))
	if err != nil {
		return "", err
	}
	temporary := file.Name()
	defer os.Remove(temporary) // Only the new, unpublished backup staging file.
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	uri := url.URL{Scheme: "file", Path: temporary}
	options := uri.Query()
	options.Set("mode", "rw")
	options.Add("_pragma", "journal_mode(DELETE)")
	options.Add("_pragma", "synchronous(FULL)")
	uri.RawQuery = options.Encode()
	connection, err := s.db.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer connection.Close()
	err = connection.Raw(func(raw any) error {
		source, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("SQLite driver does not expose online backup")
		}
		backup, err := source.NewBackup(uri.String())
		if err != nil {
			return err
		}
		finished := false
		defer func() {
			if !finished {
				_ = backup.Finish()
			}
		}()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := backup.Step(128)
			if err != nil {
				var code interface{ Code() int }
				if !errors.As(err, &code) || code.Code()&255 != sqlite3.SQLITE_BUSY && code.Code()&255 != sqlite3.SQLITE_LOCKED {
					return err
				}
				timer := time.NewTimer(25 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
				continue
			}
			if more {
				continue
			}
			destination, err := backup.Commit()
			finished = true // Commit always releases the native backup object.
			if err != nil {
				return err
			}
			return destination.Close()
		}
	})
	closeErr := connection.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := backupSchemaVersion(ctx, temporary, version); err != nil {
		return "", err
	}
	file, err = os.OpenFile(temporary, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	err = file.Sync()
	closeErr = file.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path := filepath.Join(directory, strings.TrimPrefix(filepath.Base(temporary), "."))
	// Link publishes only a finished copy and refuses to replace an existing
	// backup. Both names are within the same private directory/filesystem.
	if err := os.Link(temporary, path); err != nil {
		return "", err
	}
	if err := os.Remove(temporary); err != nil {
		return "", fmt.Errorf("backup retained at %s but staging cleanup failed: %w", path, err)
	}
	folder, err := os.Open(directory)
	if err != nil {
		return "", fmt.Errorf("backup retained at %s but directory could not be synced: %w", path, err)
	}
	err = folder.Sync()
	closeErr = folder.Close()
	if err != nil {
		return "", fmt.Errorf("backup retained at %s but directory sync failed: %w", path, err)
	}
	if closeErr != nil {
		return "", closeErr
	}
	return path, nil
}

func backupSchemaVersion(ctx context.Context, path string, expected int) error {
	uri := url.URL{Scheme: "file", Path: path}
	options := uri.Query()
	options.Set("mode", "ro")
	uri.RawQuery = options.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return err
	}
	defer db.Close()
	var actual int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&actual); err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("schema changed during backup; migration was not started")
	}
	return nil
}
