package ffs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/exp/maps"
)

// The respective syscall.* errors listed here: https://unix.stackexchange.com/a/326811

// Time tells the "current" time.
// For testing purposes, you can reassign this variable:
// 	ffs.Time = func() (t time.Time) {
// 		// always return zero value
// 		return
// 	}
var Time = time.Now

type FileSystem interface {
	Create(path string) (File, error)
	Open(path string) (File, error)
	Stat(path string) (os.FileInfo, error)
	OpenFile(path string, flag int, perm fs.FileMode) (File, error)
	// @todo: Mkdir(name string, perm FileMode) error
	// @todo: MkdirAll(path string, perm FileMode) error
	WalkDir(root string, fn fs.WalkDirFunc) error
	Truncate(path string, size int64) error
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm os.FileMode) error
	Remove(path string) error
	RemoveAll(path string) error
}

type File interface {
	Close() error
	Name() string
	Stat() (os.FileInfo, error)
	Read(b []byte) (n int, err error)                     // go doc os.File.Read
	Write(b []byte) (n int, err error)                    // go doc os.File.Write
	Seek(offset int64, whence int) (ret int64, err error) // go doc os.File.Seek
}

type RealFileSystem struct{}

var _ FileSystem = (*RealFileSystem)(nil)

func (*RealFileSystem) Create(path string) (File, error) {
	return os.Create(path)
}

func (*RealFileSystem) Open(path string) (File, error) {
	return os.Open(path)
}

func (*RealFileSystem) Stat(path string) (fs.FileInfo, error) {
	return os.Stat(path)
}

func (*RealFileSystem) OpenFile(path string, flag int, perm os.FileMode) (File, error) {
	return os.OpenFile(path, flag, perm)
}

func (*RealFileSystem) WalkDir(root string, fn fs.WalkDirFunc) error {
	return filepath.WalkDir(root, fn)
}

func (*RealFileSystem) Truncate(path string, size int64) error {
	return os.Truncate(path, size)
}

func (*RealFileSystem) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (*RealFileSystem) WriteFile(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}

func (*RealFileSystem) Remove(path string) error {
	return os.Remove(path)
}

func (*RealFileSystem) RemoveAll(path string) error {
	return os.RemoveAll(path)
}

type FakeFileSystem struct {
	parent, root *FakeFile
	contents     map[string]*FakeFile
}

var _ FileSystem = (*FakeFileSystem)(nil)

// default umask on common Linux systems
const umask = 0022

func (m *FakeFileSystem) createFile(uncleanedPath string, flag int, perm fs.FileMode) (File, error) {
	path := filepath.Clean(uncleanedPath)

	if f, ok := m.contents[path]; ok {
		if f.isDir {
			return nil, &os.PathError{
				Op:   "open",
				Path: uncleanedPath,
				Err:  syscall.EISDIR,
			}
		}
		// @todo(perms): are we allowed to open and truncate the file? (check perms)
		f.bytes = nil
		f.lastMod = Time()
		return &FakeFileDescriptor{
			file:   f,
			cursor: 0,
			flag:   flag,
		}, nil
	}

	if path[len(path)-1] == '/' {
		return nil, &os.PathError{
			Op:   "open",
			Path: uncleanedPath,
			Err:  syscall.EISDIR,
		}
	}

	parentPath := filepath.Dir(path)
	if p, ok := m.contents[parentPath]; ok {
		if !p.isDir {
			return nil, &os.PathError{
				Op:   "open",
				Path: uncleanedPath,
				Err:  syscall.ENOTDIR,
			}
		}

		// @todo(perms): are we allowed to create the file? (check perms of directory)
		f := &FakeFile{
			isDir:   false,
			path:    path,
			name:    filepath.Base(path),
			mode:    perm - umask,
			lastMod: Time(),
			parent:  p,
		}
		p.children[path] = f
		m.contents[path] = f
		return &FakeFileDescriptor{
			file:   f,
			cursor: 0,
			flag:   flag,
		}, nil
	}

	return nil, &os.PathError{
		Op:   "open",
		Path: uncleanedPath,
		Err:  syscall.ENOENT,
	}
}

func (m *FakeFileSystem) Create(path string) (File, error) {
	return m.createFile(path, os.O_RDWR, 0666)
}

func (m *FakeFileSystem) Open(uncleanedPath string) (File, error) {
	path := filepath.Clean(uncleanedPath)
	if f, ok := m.contents[path]; ok {
		// @todo(perms): are we allowed to open the file (check perms)
		return &FakeFileDescriptor{
			file:   f,
			cursor: 0,
			flag:   os.O_RDONLY,
		}, nil
	}
	return nil, &os.PathError{
		Op:   "open",
		Path: uncleanedPath,
		Err:  syscall.ENOENT,
	}
}

func (m *FakeFileSystem) OpenFile(uncleanedPath string, flag int, perm os.FileMode) (File, error) {
	path := filepath.Clean(uncleanedPath)
	if f, ok := m.contents[path]; ok {
		// @todo(perms): are we allowed to open the file? (check perms)
		if f.isDir {
			return nil, &os.PathError{
				Op:   "open",
				Path: uncleanedPath,
				Err:  syscall.EISDIR,
			}
		}
		if (flag&os.O_CREATE) == 1 && (flag&os.O_EXCL) == 1 {
			return nil, &os.PathError{
				Op:   "open",
				Path: uncleanedPath,
				Err:  syscall.EEXIST,
			}
		}
		if (flag & os.O_TRUNC) == 1 {
			f.bytes = nil
		}
		return &FakeFileDescriptor{
			file:   f,
			cursor: 0,
			flag:   flag,
		}, nil
	}
	if (flag & os.O_CREATE) == 1 {
		// @todo(perms): are we allowed to create the file? (check perms of directory)
		if path[len(path)-1] == '/' {
			return nil, &os.PathError{
				Op:   "open",
				Path: uncleanedPath,
				Err:  syscall.EISDIR,
			}
		}
		return m.createFile(uncleanedPath, flag, perm)
	}
	return nil, &os.PathError{
		Op:   "open",
		Path: uncleanedPath,
		Err:  syscall.ENOENT,
	}
}

func (m *FakeFileSystem) Stat(uncleanedPath string) (fs.FileInfo, error) {
	path := filepath.Clean(uncleanedPath)
	if f, ok := m.contents[path]; ok {
		return &FakeFileDescriptor{
			file:   f,
			cursor: 0,
			flag:   os.O_RDONLY,
		}, nil
	}
	return nil, &os.PathError{
		Op:   "stat",
		Path: uncleanedPath,
		Err:  syscall.ENOENT,
	}
}

func readDir(d *FakeFile) []*FakeFile {
	children := maps.Values(d.children)
	// files are visited in lexicographical order
	sort.Slice(children, func(i, j int) bool {
		return children[i].path < children[j].path
	})
	return children
}

func walkDir(d *FakeFile, fn fs.WalkDirFunc) error {
	err := fn(d.path, &FakeFileDescriptor{
		file:   d,
		cursor: 0,
		flag:   os.O_RDONLY,
	}, nil)
	if err == fs.SkipDir {
		return nil // successfully skipped directory
	}
	if err != nil {
		return err
	}

	dirEntries := readDir(d)
	for _, d := range dirEntries {
		if d.isDir {
			// we descend into directories first, before we continue on in the
			// current directory
			err = walkDir(d, fn)
		} else {
			err = fn(d.path, &FakeFileDescriptor{
				file:   d,
				cursor: 0,
				flag:   os.O_RDONLY,
			}, nil)
		}
		if err == fs.SkipDir {
			return nil // successfully skipped rest of directory
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *FakeFileSystem) WalkDir(uncleanedRoot string, fn fs.WalkDirFunc) (err error) {
	// @fixme: the path passed to fn should always have root as prefix
	root := filepath.Clean(uncleanedRoot)
	r, ok := m.contents[root]

	// @todo(perms): maybe we aren't allowed to open some dirs/files?

	if !ok {
		err = &os.PathError{
			Op:   "lstat",
			Path: uncleanedRoot,
			Err:  syscall.ENOENT,
		}
	}

	if err != nil {
		err = fn(root, &FakeFileDescriptor{
			file:   r,
			cursor: 0,
			flag:   os.O_RDONLY,
		}, err)
	} else {
		err = walkDir(r, fn)
	}

	if err == fs.SkipAll || err == fs.SkipDir {
		return nil
	}
	return err
}

func (m *FakeFileSystem) Truncate(uncleanedPath string, size int64) error {
	path := filepath.Clean(uncleanedPath)
	if f, ok := m.contents[path]; ok {
		if f.isDir {
			return &os.PathError{
				Op:   "truncate",
				Path: uncleanedPath,
				Err:  syscall.EISDIR,
			}
		}
		// @todo(perm): check permissions
		f.bytes = f.bytes[:size]
		return nil
	}
	return &os.PathError{
		Op:   "truncate",
		Path: uncleanedPath,
		Err:  syscall.ENOENT,
	}
}

func (m *FakeFileSystem) ReadFile(uncleanedPath string) ([]byte, error) {
	path := filepath.Clean(uncleanedPath)
	if f, ok := m.contents[path]; ok {
		if f.isDir {
			return nil, &os.PathError{
				Op:   "read",
				Path: uncleanedPath,
				Err:  syscall.EISDIR,
			}
		}
		return f.bytes, nil
	}
	return nil, &os.PathError{
		Op:   "open",
		Path: uncleanedPath,
		Err:  syscall.ENOENT,
	}
}

func (m *FakeFileSystem) WriteFile(uncleanedPath string, data []byte, perm os.FileMode) error {
	path := filepath.Clean(uncleanedPath)
	if f, ok := m.contents[path]; ok {
		if f.isDir {
			return &os.PathError{
				Op:   "open",
				Path: uncleanedPath,
				Err:  syscall.EISDIR,
			}
		}
		f.bytes = data
		return nil
	}
	parentPath := filepath.Dir(path)
	if p, ok := m.contents[parentPath]; ok {
		// @todo(perms): check folder perms
		if !p.isDir {
			return &os.PathError{
				Op:   "open",
				Path: uncleanedPath,
				Err:  syscall.ENOTDIR,
			}
		}
		f := &FakeFile{
			isDir:   false,
			path:    path,
			name:    filepath.Base(path),
			bytes:   data,
			mode:    perm - umask,
			lastMod: Time(),
			parent:  p,
		}
		p.children[path] = f
		m.contents[path] = f
		return nil
	}
	return &os.PathError{
		Op:   "open",
		Path: uncleanedPath,
		Err:  syscall.ENOENT,
	}
}

func (m *FakeFileSystem) Remove(uncleanedPath string) error {
	path := filepath.Clean(uncleanedPath)
	if f, ok := m.contents[path]; ok {
		if f == m.root {
			return &os.PathError{
				Op:   "remove",
				Path: uncleanedPath,
				Err:  syscall.EPERM,
			}
		}
		if f.isDir && len(f.children) > 0 {
			return &os.PathError{
				Op:   "remove",
				Path: uncleanedPath,
				Err:  syscall.ENOTEMPTY,
			}
		}
		// @todo(perms): check permissions
		delete(m.contents, path)
		delete(f.parent.children, path) // @todo: write tests to verify that no such references are forgotten about!!!
		return nil
	}
	return &os.PathError{
		Op:   "remove",
		Path: uncleanedPath,
		Err:  syscall.ENOENT,
	}
}

func (m *FakeFileSystem) RemoveAll(path string) error {
	return m.WalkDir(path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fd := d.(*FakeFileDescriptor)
		if fd.file == m.root {
			return &os.PathError{
				Op:   "remove",
				Path: path,
				Err:  syscall.EPERM,
			}
		}
		// @todo(perms): check perms
		delete(m.contents, path)
		// technically only needed for the top-most directory, for all others
		// the parent itself was already deleted, no need to remove the
		// children reference
		delete(fd.file.parent.children, path)
		return nil
	})
}

type FakeFile struct {
	isDir      bool
	path, name string
	bytes      []byte
	mode       fs.FileMode
	lastMod    time.Time

	parent   *FakeFile
	children map[string]*FakeFile // isDir = true only
}

type FakeFileDescriptor struct {
	file   *FakeFile
	cursor int64
	flag   int
	closed bool
}

var _ File = (*FakeFileDescriptor)(nil)
var _ fs.DirEntry = (*FakeFileDescriptor)(nil)
var _ fs.FileInfo = (*FakeFileDescriptor)(nil)

func (m *FakeFileDescriptor) Close() error {
	if m.closed {
		return errors.New("invalid argument")
	}
	m.closed = true
	return nil
}

func (m *FakeFileDescriptor) Name() string {
	return m.file.name
}

func (m *FakeFileDescriptor) Stat() (fs.FileInfo, error) {
	if m.closed {
		return nil, &os.PathError{
			Op:   "stat",
			Path: m.file.path,
			Err:  errors.New("use of closed file"),
		}
	}
	// m implements fs.FileInfo
	return m, nil
}

func (m *FakeFileDescriptor) Read(b []byte) (n int, err error) {
	if m.closed {
		return 0, &os.PathError{
			Op:   "stat",
			Path: m.file.path,
			Err:  errors.New("file already closed"),
		}
	}
	if m.file.isDir || (m.flag&0b11) == os.O_WRONLY {
		return 0, &os.PathError{
			Op:   "read",
			Path: m.file.path,
			Err:  syscall.EISDIR, // @fixme: wronly returns EISDIR? (check if this is correct)
		}
	}
	if m.cursor >= int64(len(m.file.bytes)) {
		return 0, io.EOF
	}
	n = copy(b, m.file.bytes[m.cursor:])
	m.cursor += int64(n)
	return
}

func (m *FakeFileDescriptor) Write(src []byte) (n int, err error) {
	if m.closed {
		return 0, &os.PathError{
			Op:   "stat",
			Path: m.file.path,
			Err:  errors.New("file already closed"),
		}
	}
	if m.file.isDir || (m.flag&0b11) == os.O_RDONLY {
		return 0, &os.PathError{
			Op:   "write",
			Path: m.file.path,
			Err:  syscall.EBADF,
		}
	}
	if (m.flag & os.O_APPEND) == 1 {
		m.cursor = int64(len(m.file.bytes))
	} else {
		for m.cursor > int64(len(m.file.bytes)) {
			m.file.bytes = append(m.file.bytes, 0)
		}
	}
	dst := m.file.bytes[m.cursor:]
	if len(src) <= len(dst) { // enough space in file for new data
		n = copy(dst, src)
	} else { // not enough space, we are appending (and possibly overwriting the end of the file)
		//          current len + amount missing
		nl := len(m.file.bytes) + len(src) - len(dst)
		nbs := make([]byte, nl)
		copy(nbs, m.file.bytes[:m.cursor])
		n = copy(nbs[m.cursor:], src)
		m.file.bytes = nbs
	}
	m.cursor += int64(n)
	return
}

func (m *FakeFileDescriptor) Seek(offset int64, whence int) (int64, error) {
	if m.closed {
		return 0, &os.PathError{
			Op:   "stat",
			Path: m.file.path,
			Err:  errors.New("file already closed"),
		}
	}
	switch whence {
	case io.SeekStart:
		// relative to the origin of the file
		m.cursor = offset
	case io.SeekCurrent:
		// relative to the current offset
		m.cursor += offset
	case io.SeekEnd:
		// relative to the end of the file
		m.cursor = int64(len(m.file.bytes)) + offset
	}
	return m.cursor, nil
}

func (m *FakeFileDescriptor) Info() (fs.FileInfo, error) {
	// "The returned FileInfo may be from the time of the original directory read [...]"
	// -- go doc fs.DirEntry
	// m implements fs.FileInfo
	return m, nil
}

func (m *FakeFileDescriptor) IsDir() bool {
	return m.file.isDir
}

func (m *FakeFileDescriptor) Type() fs.FileMode {
	return m.file.mode
}

func (m *FakeFileDescriptor) ModTime() time.Time {
	return m.file.lastMod
}

func (m *FakeFileDescriptor) Mode() fs.FileMode {
	return m.file.mode
}

func (m *FakeFileDescriptor) Size() int64 {
	return int64(len(m.file.bytes))
}

func (m *FakeFileDescriptor) Sys() any {
	if m.closed {
		return &os.PathError{
			Op:   "stat",
			Path: m.file.path,
			Err:  errors.New("file already closed"),
		}
	}
	return m.file
}

func MockFS(opts ...FSOption) (fs *FakeFileSystem) {
	r := &FakeFile{
		isDir:    true,
		path:     "/",
		name:     "/",
		mode:     0777 - umask,
		lastMod:  Time(),
		parent:   nil,
		children: map[string]*FakeFile{},
	}
	fs = &FakeFileSystem{
		parent: r,
		root:   r,
		contents: map[string]*FakeFile{
			"/": r,
		},
	}
	for _, opt := range opts {
		opt(fs)
	}
	return
}

type FSOption func(*FakeFileSystem)

func WithFile(path string, data []byte) FSOption {
	return func(fs *FakeFileSystem) {
		path := filepath.Clean(path)
		parentPath := filepath.Dir(path)
		parts := strings.Split(parentPath, "/")[1:] // exclude empty ""

		p := fs.root

		for i := range parts {
			pname := "/" + strings.Join(parts[:i+1], "/")
			pn, ok := fs.contents[pname]
			if !ok {
				pn = &FakeFile{
					isDir:    true,
					path:     pname,
					name:     parts[i] + "/",
					mode:     0777 - umask,
					lastMod:  Time(),
					parent:   p,
					children: map[string]*FakeFile{},
				}
				p.children[pname] = pn
				fs.contents[pname] = pn
			}
			p = pn
		}
		// p now points to the file's immediate ancestor

		f := &FakeFile{
			isDir:   false,
			path:    path,
			name:    filepath.Base(path),
			bytes:   data,
			mode:    0666 - umask,
			lastMod: Time(),
			parent:  p,
		}
		p.children[path] = f
		fs.contents[path] = f
	}
}

func WithDirectory(path string) FSOption {
	return func(fs *FakeFileSystem) {
		path = filepath.Clean(path)
		parts := strings.Split(path, "/")[1:] // exclude empty ""

		p := fs.root

		for i := range parts {
			pname := "/" + strings.Join(parts[:i+1], "/")
			pn, ok := fs.contents[pname]
			if !ok {
				pn = &FakeFile{
					isDir:    true,
					path:     pname,
					name:     parts[i] + "/",
					mode:     0777 - umask,
					lastMod:  Time(),
					parent:   p,
					children: map[string]*FakeFile{},
				}
				p.children[pname] = pn
				fs.contents[pname] = pn
			}
			p = pn
		}
		// p now points to the inner-most directory
	}
}

func (m *FakeFileSystem) String() (pp string) {
	ns := []*FakeFile{m.root}
	for len(ns) > 0 {
		n := ns[0]
		ns = ns[1:]

		var content string
		if n.isDir {
			content = "(Directory)"
		} else {
			content = "`" + string(n.bytes) + "'"
		}
		pp = fmt.Sprintf("%s\n%s: %s", pp, n.path, content)

		ns = append(ns, maps.Values(n.children)...)
	}
	return
}
