package cas

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/dennwc/cas/schema"
	"github.com/dennwc/cas/storage"
	"github.com/dennwc/cas/types"
)

var (
	typeDirEnt = schema.MustTypeOf(&schema.DirEntry{})
)

const (
	maxDirEntries = 1024
)

type FileDesc interface {
	Name() string
	Open() (io.ReadCloser, SizedRef, error)
	SetRef(ref types.SizedRef)
}

func LocalFile(path string) FileDesc {
	return &localFile{path: path}
}

func (s *Storage) storeAsFile(ctx context.Context, fd FileDesc, indexOnly bool) (*schema.DirEntry, error) {
	// open the file, snapshot metadata
	rc, xr, err := fd.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	// if we have a reliable metadata - use it without reading the file
	if !xr.Ref.Zero() {
		// we know the ref beforehand
		m := &schema.DirEntry{
			Ref: xr.Ref, Size: xr.Size,
			Name: fd.Name(),
		}
		if indexOnly {
			// if only indexing - return the response directly
			return m, nil
		}
		// if storing, check if blob store has this ref already
		_, err := s.StatBlob(ctx, xr.Ref)
		if err == nil {
			return m, nil
		}
	}

	// we don't have metadata available - need to read the file

	var fw storage.BlobWriter
	if indexOnly {
		// indexing: just hash the file
		fw = storage.Hash()
	} else {
		// storing the file
		if lf, ok := fd.(*localFile); ok {
			if l, ok := s.st.(*storage.LocalStorage); ok {
				// clone file, if possible
				if sr, err := l.ImportFile(ctx, lf.path); err == nil {
					fd.SetRef(sr)
					return &schema.DirEntry{
						Ref: sr.Ref, Size: sr.Size,
						Name: fd.Name(),
					}, nil
				}
			}
		}
		// begin ordinary write procedure
		var err error
		fw, err = s.BeginBlob(ctx)
		if err != nil {
			return nil, err
		}
	}
	defer fw.Close()

	name := filepath.Base(fd.Name())

	n, err := io.Copy(fw, rc)
	if err != nil {
		return nil, err
	} else if uint64(n) != uint64(xr.Size) {
		return nil, fmt.Errorf("file changed while writing it")
	}
	sr, err := fw.Complete()
	if err != nil {
		return nil, err
	} else if sr.Size != uint64(xr.Size) {
		return nil, fmt.Errorf("file changed while writing it")
	}
	fd.SetRef(sr)
	err = fw.Commit()
	if err != nil {
		return nil, err
	}
	return &schema.DirEntry{
		Ref: sr.Ref, Size: sr.Size,
		Name: name,
	}, nil
}

func (s *Storage) storeDirList(ctx context.Context, list []schema.DirEntry) (SizedRef, schema.DirEntry, error) {
	var (
		cnt  uint
		size uint64
	)
	olist := make([]schema.Object, 0, len(list))
	for _, e := range list {
		cnt += e.Count + 1
		size += e.Size
		e := e
		olist = append(olist, &e)
	}
	m := &schema.InlineList{Elem: typeDirEnt, List: olist}
	sr, err := s.StoreSchema(ctx, m)
	if err != nil {
		return SizedRef{}, schema.DirEntry{}, err
	}
	return sr, schema.DirEntry{Ref: sr.Ref, Count: cnt, Size: size}, nil
}

func (s *Storage) storeDirJoin(ctx context.Context, refs []Ref, list []schema.List) (SizedRef, schema.List, error) {
	// TODO: aggregate stats
	//var (
	//	cnt  uint
	//	size uint64
	//)
	//for _, e := range list {
	//	cnt += e.Count
	//	size += e.Size
	//}
	m := schema.List{Elem: typeDirEnt, List: refs}
	sr, err := s.StoreSchema(ctx, &m)
	if err != nil {
		return SizedRef{}, schema.List{}, err
	}
	return sr, m, nil
}

func (s *Storage) storeDir(ctx context.Context, dir string, index bool) (SizedRef, schema.DirEntry, error) {
	d, err := os.Open(dir)
	if err != nil {
		return SizedRef{}, schema.DirEntry{}, err
	}
	defer d.Close()

	var base []schema.DirEntry
	for {
		buf, err := d.Readdir(maxDirEntries)
		if err == io.EOF {
			d.Close()
			break
		} else if err != nil {
			return SizedRef{}, schema.DirEntry{}, err
		}
		for _, fi := range buf {
			if fi.Name() == DefaultDir {
				continue
			}
			fpath := filepath.Join(dir, fi.Name())
			if fi.IsDir() {
				sr, st, err := s.storeDir(ctx, fpath, index)
				if err != nil {
					return SizedRef{}, schema.DirEntry{}, err
				}
				st.Ref = sr.Ref
				st.Name = fi.Name()
				base = append(base, st)
			} else {
				ent, err := s.storeAsFile(ctx, LocalFile(fpath), index)
				if err != nil {
					return SizedRef{}, schema.DirEntry{}, err
				}
				base = append(base, *ent)
			}
		}
	}
	sort.Slice(base, func(i, j int) bool {
		return base[i].Name < base[j].Name
	})
	var (
		level []schema.List
		refs  []Ref
		cur   schema.List
	)
	if len(base) <= maxDirEntries {
		return s.storeDirList(ctx, base)
	}
	for len(base) > 0 {
		page := base
		if len(page) > maxDirEntries {
			page = page[:maxDirEntries]
		}
		base = base[len(page):]

		sr, _, err := s.storeDirList(ctx, page)
		if err != nil {
			return SizedRef{}, schema.DirEntry{}, err
		}
		// TODO: aggregate stats
		//cur.Size += st.Size
		//cur.Count += st.Count
		cur.List = append(cur.List, sr.Ref)
		if len(cur.List) >= maxDirEntries || len(base) == 0 {
			cur.Elem = typeDirEnt
			sr, err = s.StoreSchema(ctx, &cur)
			if err != nil {
				return SizedRef{}, schema.DirEntry{}, err
			}
			level = append(level, cur)
			refs = append(refs, sr.Ref)
			cur = schema.List{}
		}
	}
	for len(level) > 1 {
		var (
			newLevel []schema.List
			newRefs  []Ref
		)
		for len(level) > 0 {
			page := level
			if len(page) > maxDirEntries {
				page = page[:maxDirEntries]
			}
			pref := refs[:len(page)]

			level = level[len(page):]
			refs = refs[len(page):]

			sr, cur, err := s.storeDirJoin(ctx, pref, page)
			if err != nil {
				return SizedRef{}, schema.DirEntry{}, err
			}
			newLevel = append(newLevel, cur)
			newRefs = append(newRefs, sr.Ref)
		}
		level, refs = newLevel, newRefs
	}
	top := level[0]
	sr, err := s.StoreSchema(ctx, &top)
	if err != nil {
		return SizedRef{}, schema.DirEntry{}, err
	}
	return sr, schema.DirEntry{
		Ref: sr.Ref,
		// TODO: aggregate stats
		//Count: top.Count, Size: top.Size,
	}, nil
}

func (s *Storage) IndexAsFile(ctx context.Context, fd FileDesc) (SizedRef, error) {
	m, err := s.storeAsFile(ctx, fd, true)
	if err != nil {
		return SizedRef{}, err
	}
	return s.StoreSchema(ctx, m)
}

func (s *Storage) StoreAsFile(ctx context.Context, fd FileDesc) (SizedRef, error) {
	m, err := s.storeAsFile(ctx, fd, false)
	if err != nil {
		return SizedRef{}, err
	}
	return s.StoreSchema(ctx, m)
}

func (s *Storage) storeFilePath(ctx context.Context, path string, index bool) (SizedRef, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return SizedRef{}, err
	}
	if fi.IsDir() {
		sr, _, err := s.storeDir(ctx, path, index)
		return sr, err
	}
	ent, err := s.storeAsFile(ctx, LocalFile(path), index)
	return SizedRef{Ref: ent.Ref, Size: ent.Size}, err
}

func (s *Storage) IndexFilePath(ctx context.Context, path string) (SizedRef, error) {
	return s.storeFilePath(ctx, path, true)
}

func (s *Storage) StoreFilePath(ctx context.Context, path string) (SizedRef, error) {
	return s.storeFilePath(ctx, path, false)
}

type localFile struct {
	path string
	fi   os.FileInfo
}

func (f *localFile) Name() string {
	return filepath.Base(f.path)
}

func (f *localFile) Open() (io.ReadCloser, SizedRef, error) {
	fd, err := os.Open(f.path)
	if err != nil {
		return nil, types.SizedRef{}, err
	}
	st, err := fd.Stat()
	if err != nil {
		fd.Close()
		return nil, SizedRef{}, err
	}
	f.fi = st
	sr := SizedRef{Size: uint64(st.Size())}
	if xr, err := Stat(context.Background(), f.path); err == nil && xr.Size == sr.Size {
		sr.Ref = xr.Ref
	}
	return fd, sr, nil
}

func (f *localFile) SetRef(ref types.SizedRef) {
	if f.fi == nil || uint64(f.fi.Size()) != ref.Size {
		// this is the only case that we can reject directly
		// all other checks happen at read time
		return
	}
	_ = SaveRef(context.Background(), f.path, f.fi, ref.Ref)
}
