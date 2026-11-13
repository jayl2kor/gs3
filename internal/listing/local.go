package listing

import (
	"context"
	"io/fs"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"gs3/internal/core"
	"gs3/internal/s3path"
)

const sep = string(os.PathSeparator)

// Local lists the files of a local source, as awscli's
// FileGenerator.list_files does.
//
// Ordering: S3 lists keys in byte order of the full key. To produce local
// files in the same order, each directory's entries are sorted after
// appending "/" to the names of subdirectories, and a subdirectory is walked
// at its place in that order. ("a.txt" sorts before the contents of "a/"
// because "." < "/", exactly as the keys "a.txt" and "a/x" do.)
func Local(ctx context.Context, files s3path.Files, op core.Op, followSymlinks bool, warn Warner) *Stream {
	return start(ctx, 2, func(emit func([]core.FileStat) bool) error {
		w := &walker{
			files: files, op: op, follow: followSymlinks, warn: warn, emit: emit,
			uid: uint32(os.Geteuid()),
		}
		if groups, err := os.Getgroups(); err == nil {
			w.groups = groups
		}
		w.gid = os.Getegid()

		root := files.Src.Path
		if !files.DirOp {
			if !w.ignored(root, nil) {
				w.addPath(root)
			}
		} else if !w.ignored(root, nil) {
			w.walk(root, nil)
		}
		w.flush()
		return nil
	})
}

type walker struct {
	files  s3path.Files
	op     core.Op
	follow bool
	warn   Warner
	emit   func([]core.FileStat) bool

	batch   []core.FileStat
	stopped bool

	uid    uint32
	gid    int
	groups []int
}

type dirID struct {
	dev uint64
	ino uint64
}

type item struct {
	name  string // with a trailing separator for directories
	isDir bool
	info  fs.FileInfo // target info (symlinks resolved)
}

// walk lists one directory. dir ends with a separator. ancestors holds the
// identities of the directories on the path down to dir, to catch symlink
// cycles.
func (w *walker) walk(dir string, ancestors []dirID) {
	if w.stopped {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		w.warn.Warn(dir, "File/Directory is not readable.")
		return
	}
	if id, ok := identity(dir); ok {
		ancestors = append(ancestors, id)
	}

	items := make([]item, 0, len(entries))
	for _, entry := range entries {
		path := dir + entry.Name()
		info, skip := w.inspect(path, entry)
		if skip {
			continue
		}
		it := item{name: entry.Name(), isDir: info.IsDir(), info: info}
		if it.isDir {
			it.name += sep
		}
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })

	for _, it := range items {
		if w.stopped {
			return
		}
		path := dir + it.name
		if !it.isDir {
			w.add(path, it.info)
			continue
		}
		if id, ok := identity(path); ok && contains(ancestors, id) {
			w.warn.Warn(path, "File is a symbolic link that loops back to a parent directory.")
			continue
		}
		w.walk(path, ancestors)
	}
}

func contains(ids []dirID, id dirID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func identity(path string) (dirID, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return dirID{}, false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return dirID{}, false
	}
	return dirID{dev: uint64(sys.Dev), ino: uint64(sys.Ino)}, true
}

// inspect decides whether a directory entry is listed, emitting the AWS
// CLI's warnings for the ones that are not. It returns the entry's info with
// symlinks resolved.
func (w *walker) inspect(path string, entry fs.DirEntry) (fs.FileInfo, bool) {
	isLink := entry.Type()&fs.ModeSymlink != 0
	if isLink && !w.follow {
		return nil, true // skipped silently
	}
	var info fs.FileInfo
	var err error
	if isLink {
		info, err = os.Stat(path)
	} else {
		info, err = entry.Info()
	}
	if err != nil {
		// Includes dangling symlinks.
		w.warn.Warn(path, "File does not exist.")
		return nil, true
	}
	if w.unusable(path, info) {
		return nil, true
	}
	return info, false
}

// ignored applies the same checks to the root path of the listing. The path
// is examined exactly as given: a regular file named with a trailing
// separator does not exist, which is how `sync file s3://...` is reported.
func (w *walker) ignored(path string, _ fs.DirEntry) bool {
	if !w.follow {
		link := path
		if st, err := os.Stat(path); err == nil && st.IsDir() && len(path) > 1 {
			// A symlink can only be recognised without the trailing separator.
			link = strings.TrimSuffix(path, sep)
		}
		if li, err := os.Lstat(link); err == nil && li.Mode()&fs.ModeSymlink != 0 {
			return true
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		w.warn.Warn(path, "File does not exist.")
		return true
	}
	return w.unusable(path, info)
}

// unusable warns about special files and unreadable files.
func (w *walker) unusable(path string, info fs.FileInfo) bool {
	if info.Mode()&(fs.ModeDevice|fs.ModeCharDevice|fs.ModeNamedPipe|fs.ModeSocket) != 0 {
		w.warn.Warn(path, "File is character special device, block special device, FIFO, or socket.")
		return true
	}
	if !info.IsDir() && !w.readable(info) {
		w.warn.Warn(path, "File/Directory is not readable.")
		return true
	}
	return false
}

// readable reports whether the process may read the file, judged from its
// permission bits. The AWS CLI opens every file to find out; deciding from
// the stat already in hand avoids two system calls per file.
func (w *walker) readable(info fs.FileInfo) bool {
	if w.uid == 0 {
		return true
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	mode := info.Mode().Perm()
	if sys.Uid == w.uid {
		return mode&0o400 != 0
	}
	if int(sys.Gid) == w.gid {
		return mode&0o040 != 0
	}
	for _, g := range w.groups {
		if int(sys.Gid) == g {
			return mode&0o040 != 0
		}
	}
	return mode&0o004 != 0
}

// addPath lists a single file given by path (non-recursive source).
func (w *walker) addPath(path string) {
	info, err := os.Stat(path)
	if err != nil {
		w.warn.Warn(path, "File does not exist.")
		return
	}
	w.add(path, info)
}

func (w *walker) add(path string, info fs.FileInfo) {
	dest, key := w.files.DestAndCompareKey(path)
	w.batch = append(w.batch, core.FileStat{
		Src: path, Dest: dest, CompareKey: key,
		Size: info.Size(), SizeKnown: true,
		// Python keeps microseconds.
		ModTime: info.ModTime().Round(time.Microsecond),
		SrcType: core.Local, DestType: w.files.Dest.Type, Op: w.op,
	})
	if len(w.batch) >= batchSize {
		w.flush()
	}
}

func (w *walker) flush() {
	if len(w.batch) == 0 {
		return
	}
	if !w.emit(w.batch) {
		w.stopped = true
	}
	w.batch = nil
}
