package patch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

type FileResult struct {
	Path               string `json:"path"`
	MoveTo             string `json:"move_to,omitempty"`
	Kind               string `json:"kind"`
	Status             string `json:"status"`
	BeforeSHA256       string `json:"before_sha256,omitempty"`
	AfterSHA256        string `json:"after_sha256,omitempty"`
	Spans              []Span `json:"spans,omitempty"`
	DestinationWritten bool   `json:"destination_written"`
	SourceRemoved      bool   `json:"source_removed"`
	Durable            bool   `json:"durable"`
	Error              *Error `json:"error,omitempty"`
}

type Report struct {
	Status             string       `json:"status"`
	Files              []FileResult `json:"files"`
	CreatedDirectories []string     `json:"created_directories,omitempty"`
	TemporaryFiles     []string     `json:"temporary_files,omitempty"`
}

type snapshot struct {
	data []byte
	info os.FileInfo
}
type entry struct {
	op     operation
	path   string
	move   string
	before *snapshot
	after  []byte
}
type plan struct {
	cwd     string
	entries []entry
	report  Report
}

// Engine's filesystem mutations are small injection points for I/O fault tests.
// All Engine instances share an in-process sequencer. External programs remain
// free to write; snapshots are rechecked immediately before each mutation.
type Engine struct {
	createTemp func(string, string) (*os.File, error)
	publish    func(string, string, bool) error
	remove     func(string) error
	syncDir    func(string) error
}

var editToken = make(chan struct{}, 1)

func New() *Engine {
	return &Engine{createTemp: os.CreateTemp, remove: os.Remove, syncDir: syncDirectory,
		publish: func(temp, target string, replace bool) error {
			if replace {
				return os.Rename(temp, target)
			}
			// Link publishes a fully written file without overwriting a destination
			// created since preflight. The private temporary name is removed later.
			return os.Link(temp, target)
		},
	}
}

func (e *Engine) Apply(ctx context.Context, cwd, text string) (Report, error) {
	select {
	case editToken <- struct{}{}:
		defer func() { <-editToken }()
	case <-ctx.Done():
		return Report{Status: "rejected", Files: []FileResult{}}, ctx.Err()
	}
	p, err := prepare(ctx, cwd, text)
	if err != nil {
		return p.report, err
	}
	return e.apply(ctx, p)
}

func prepare(ctx context.Context, cwd, text string) (plan, error) {
	p := plan{cwd: cwd, report: Report{Status: "rejected", Files: []FileResult{}}}
	ops, err := parse(text)
	if err != nil {
		return p, err
	}
	if !filepath.IsAbs(cwd) {
		return p, problem("invalid_patch", "", 0, "working directory must be absolute")
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return p, problem("patch_io", cwd, 0, "working directory is unavailable")
	}
	rawPathBytes := 0
	for _, op := range ops {
		rawPathBytes += len(op.path) + len(op.move)
	}
	if rawPathBytes > 32768 {
		return p, problem("patch_limit", "", 0, "combined input paths exceed 32 KiB")
	}
	for _, op := range ops {
		p.report.Files = append(p.report.Files, FileResult{Path: op.path, MoveTo: op.move, Kind: op.kind, Status: "not_applied"})
	}
	fail := func(i int, err error) (plan, error) {
		p.report.Files[i].Status = "failed"
		p.report.Files[i].Error = asError(err, ops[i].path)
		return p, err
	}
	paths := map[string]bool{}
	reportPaths := map[string]bool{}
	pathBytes := 0
	total := 0
	for i, op := range ops {
		if err := ctx.Err(); err != nil {
			return fail(i, err)
		}
		item := entry{op: op}
		item.path, err = resolve(cwd, op.path)
		if err != nil {
			return fail(i, err)
		}
		r := &p.report.Files[i]
		r.Path = item.path
		if op.move != "" {
			item.move, err = resolve(cwd, op.move)
			if err != nil {
				return fail(i, err)
			}
			r.MoveTo = item.move
		}
		for _, path := range []string{item.path, item.move} {
			if path == "" {
				continue
			}
			for existing := range paths {
				if existing == path || strings.HasPrefix(path, existing+string(os.PathSeparator)) || strings.HasPrefix(existing, path+string(os.PathSeparator)) {
					return fail(i, problem("patch_conflict", path, op.line, "duplicate, aliased or parent/child patch targets"))
				}
			}
			paths[path] = true
			// Bound file/directory result paths before creating anything. A deep
			// missing directory tree must not produce an unretainable tool result.
			for current := path; ; current = filepath.Dir(current) {
				if !reportPaths[current] {
					pathBytes += len(current)
					reportPaths[current] = true
				}
				if _, err := os.Lstat(filepath.Dir(current)); err == nil || filepath.Dir(current) == current {
					break
				}
			}
			if pathBytes > 32768 {
				return fail(i, problem("patch_limit", path, op.line, "combined file and new directory paths exceed 32 KiB"))
			}
		}
		if op.kind == "add" {
			if err = absent(item.path); err != nil {
				return fail(i, err)
			}
			if len(op.added) > 0 {
				item.after = []byte(strings.Join(op.added, "\n") + "\n")
			}
			r.Spans = []Span{{OldStart: 1, NewStart: 1, NewCount: len(op.added)}}
		} else {
			item.before, err = readSnapshot(item.path)
			if err != nil {
				return fail(i, err)
			}
			r.BeforeSHA256 = digest(item.before.data)
			total += len(item.before.data)
			if op.kind == "delete" {
				r.Spans = []Span{{OldStart: 1, OldCount: len(splitText(string(item.before.data))), NewStart: 1}}
			} else {
				var updated string
				updated, r.Spans, err = updateText(string(item.before.data), item.path, op.chunks)
				if err != nil {
					return fail(i, err)
				}
				item.after = []byte(updated)
			}
		}
		if item.move != "" {
			if err = absent(item.move); err != nil {
				return fail(i, err)
			}
		}
		total += len(item.after)
		if len(item.after) > MaxFileBytes || total > MaxTotalBytes || bytes.Count(item.after, []byte{'\n'}) > maxLines {
			return fail(i, problem("patch_limit", item.path, op.line, "text exceeds 8 MiB/200000 lines per file or 32 MiB combined before/after"))
		}
		if op.kind != "delete" {
			r.AfterSHA256 = digest(item.after)
		}
		p.entries = append(p.entries, item)
	}
	return p, nil
}

func (e *Engine) apply(ctx context.Context, p plan) (Report, error) {
	// Revalidate the entire prepared set before the first directory or file write.
	for i, item := range p.entries {
		err := ctx.Err()
		if err == nil {
			err = recheck(p.cwd, item)
		}
		if err != nil {
			p.report.Files[i].Status, p.report.Files[i].Error = "failed", asError(err, item.path)
			return p.report, err
		}
	}
	p.report.Status = "applying"
	for i, item := range p.entries {
		r := &p.report.Files[i]
		err := ctx.Err()
		if err == nil {
			err = e.applyFile(ctx, p.cwd, item, r, &p.report)
		}
		if err != nil {
			r.Status, r.Error = "failed", asError(err, item.path)
			if r.DestinationWritten || r.SourceRemoved {
				r.Status = "partial"
			}
			p.report.Status = "failed"
			if len(p.report.CreatedDirectories) > 0 || len(p.report.TemporaryFiles) > 0 {
				p.report.Status = "partial"
			}
			for _, done := range p.report.Files {
				if done.DestinationWritten || done.SourceRemoved {
					p.report.Status = "partial"
				}
			}
			return p.report, err
		}
		r.Status, r.Durable = "applied", true
	}
	p.report.Status = "applied"
	return p.report, nil
}

func (e *Engine) applyFile(ctx context.Context, cwd string, item entry, r *FileResult, report *Report) (retErr error) {
	if err := recheck(cwd, item); err != nil {
		return err
	}
	if item.op.kind != "delete" {
		target := item.path
		if item.move != "" {
			target = item.move
		}
		parent := filepath.Dir(target)
		if err := e.makeParents(parent, report); err != nil {
			return err
		}
		f, err := e.createTemp(parent, ".zende-patch-*")
		if err != nil {
			return asError(err, target)
		}
		temp := f.Name()
		defer func() {
			_ = f.Close()
			if err := e.remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
				report.TemporaryFiles = append(report.TemporaryFiles, temp)
				if retErr == nil {
					retErr = problem("patch_io", temp, 0, "edit is visible but temporary file cleanup failed")
				}
			}
		}()
		mode := os.FileMode(0644)
		if item.before != nil {
			mode = item.before.info.Mode().Perm()
		}
		if _, err = f.Write(item.after); err == nil {
			err = f.Chmod(mode)
		}
		if err == nil {
			err = f.Sync()
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = recheck(cwd, item)
		}
		if err != nil {
			return asError(err, target)
		}
		if err = e.publish(temp, target, item.op.kind == "update" && item.move == ""); err != nil {
			return asError(err, target)
		}
		r.DestinationWritten = true
		if err := e.syncDir(parent); err != nil {
			return problem("operation_unknown", target, 0, "destination written; directory sync failed, verify durability before retrying")
		}
	}
	if item.op.kind == "delete" || item.move != "" {
		// A move publishes the destination before removing the original. If this
		// step fails, both remain and the report explicitly records that range.
		if err := checkSnapshot(item.path, item.before); err != nil {
			return err
		}
		if err := e.remove(item.path); err != nil {
			return asError(err, item.path)
		}
		r.SourceRemoved = true
		if err := e.syncDir(filepath.Dir(item.path)); err != nil {
			return problem("operation_unknown", item.path, 0, "source removed; directory sync failed, verify durability before retrying")
		}
	}
	return nil
}

func recheck(cwd string, item entry) error {
	for raw, expected := range map[string]string{item.op.path: item.path, item.op.move: item.move} {
		if raw == "" {
			continue
		}
		current, err := resolve(cwd, raw)
		if err != nil || current != expected {
			return problem("patch_conflict", expected, 0, "path resolution changed since preflight")
		}
	}
	if err := checkSnapshot(item.path, item.before); err != nil {
		return err
	}
	if item.move != "" {
		return absent(item.move)
	}
	return nil
}

func checkSnapshot(path string, expected *snapshot) error {
	if expected == nil {
		return absent(path)
	}
	current, err := readSnapshot(path)
	if err != nil {
		return err
	}
	if !os.SameFile(expected.info, current.info) || expected.info.Mode() != current.info.Mode() || !bytes.Equal(expected.data, current.data) {
		return problem("patch_conflict", path, 0, "file changed since preflight")
	}
	return nil
}

func readSnapshot(path string) (*snapshot, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, asError(err, path)
	}
	if !info.Mode().IsRegular() {
		return nil, problem("patch_conflict", path, 0, "expected a regular text file; symlinks and special files need an explicit shell operation")
	}
	if info.Size() > MaxFileBytes {
		return nil, problem("patch_limit", path, 0, "file exceeds 8 MiB")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, asError(err, path)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, problem("patch_conflict", path, 0, "file identity changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, asError(err, path)
	}
	after, err := f.Stat()
	named, nameErr := os.Lstat(path)
	if err != nil || nameErr != nil || !os.SameFile(opened, named) || opened.Size() != after.Size() || opened.Size() != int64(len(data)) || !opened.ModTime().Equal(after.ModTime()) {
		return nil, problem("patch_conflict", path, 0, "file changed while reading")
	}
	if len(data) > MaxFileBytes || bytes.Count(data, []byte{'\n'}) > maxLines {
		return nil, problem("patch_limit", path, 0, "file exceeds 8 MiB or 200000 lines")
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, problem("patch_conflict", path, 0, "patch requires UTF-8 text without NUL")
	}
	return &snapshot{data: data, info: after}, nil
}

func absent(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return asError(err, path)
	}
	return problem("patch_conflict", path, 0, "destination already exists")
}

// Resolve directory symlinks, including an existing prefix of a new directory.
// Do not follow the final file symlink. Absolute and parent-relative paths are
// permitted: this is a text-edit contract, not a workspace sandbox.
func resolve(cwd, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	parent := filepath.Dir(path)
	var missing []string
	for {
		real, err := filepath.EvalSymlinks(parent)
		if err == nil {
			info, statErr := os.Stat(real)
			if statErr != nil || !info.IsDir() {
				return "", problem("patch_conflict", path, 0, "parent is not a directory")
			}
			for i := len(missing) - 1; i >= 0; i-- {
				real = filepath.Join(real, missing[i])
			}
			return filepath.Join(real, filepath.Base(path)), nil
		}
		if !errors.Is(err, os.ErrNotExist) || parent == filepath.Dir(parent) {
			return "", asError(err, path)
		}
		// A dangling directory symlink is not a missing directory to create.
		if info, statErr := os.Lstat(parent); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", problem("patch_conflict", path, 0, "parent symlink is dangling")
		}
		missing = append(missing, filepath.Base(parent))
		parent = filepath.Dir(parent)
	}
}

func (e *Engine) makeParents(path string, report *Report) error {
	var missing []string
	for {
		info, err := os.Lstat(path)
		if err == nil {
			if !info.IsDir() {
				return problem("patch_conflict", path, 0, "parent changed or is not a directory")
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) || path == filepath.Dir(path) {
			return asError(err, path)
		}
		missing = append(missing, path)
		path = filepath.Dir(path)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		dir := missing[i]
		if err := os.Mkdir(dir, 0755); err != nil {
			return asError(err, dir)
		}
		report.CreatedDirectories = append(report.CreatedDirectories, dir)
		if err := e.syncDir(filepath.Dir(dir)); err != nil {
			return problem("operation_unknown", dir, 0, "directory created; parent sync failed, verify durability before retrying")
		}
	}
	return nil
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func asError(err error, path string) *Error {
	var p *Error
	if errors.As(err, &p) {
		return p
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return problem("patch_cancelled", path, 0, "patch cancelled; inspect file results before retrying")
	}
	return problem("patch_io", path, 0, "filesystem operation failed: "+err.Error())
}
