package cas

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"

	"github.com/dennwc/cas/schema"
	"github.com/dennwc/cas/storage"
	"github.com/dennwc/cas/types"
)

const (
	DefaultDir = ".cas"
	DefaultPin = "root"
)

type OpenOptions struct {
	Dir     string
	Create  bool
	Storage storage.Storage
}

func Open(opt OpenOptions) (*Storage, error) {
	s := opt.Storage
	if s == nil {
		var err error
		s, err = storage.NewLocal(opt.Dir, opt.Create)
		if err != nil {
			return nil, err
		}
	}
	return New(s)
}

func New(st storage.Storage) (*Storage, error) {
	return &Storage{st: st}, nil
}

var _ storage.Storage = (*Storage)(nil)

type Storage struct {
	st storage.Storage
}

func (s *Storage) SetPin(ctx context.Context, name string, ref types.Ref) error {
	if name == "" {
		name = DefaultPin
	}
	return s.st.SetPin(ctx, name, ref)
}

func (s *Storage) DeletePin(ctx context.Context, name string) error {
	if name == "" {
		name = DefaultPin
	}
	return s.st.DeletePin(ctx, name)
}

func (s *Storage) GetPin(ctx context.Context, name string) (types.Ref, error) {
	if name == "" {
		name = DefaultPin
	}
	return s.st.GetPin(ctx, name)
}

func (s *Storage) IteratePins(ctx context.Context) storage.PinIterator {
	return s.st.IteratePins(ctx)
}

func (s *Storage) FetchBlob(ctx context.Context, ref Ref) (io.ReadCloser, uint64, error) {
	if ref.Empty() {
		// generate empty blobs
		return ioutil.NopCloser(bytes.NewReader(nil)), 0, nil
	}
	return s.st.FetchBlob(ctx, ref)
}

func (s *Storage) IterateBlobs(ctx context.Context) storage.Iterator {
	return s.st.IterateBlobs(ctx)
}

func (s *Storage) StatBlob(ctx context.Context, ref Ref) (uint64, error) {
	if ref.Empty() {
		return 0, nil
	}
	return s.st.StatBlob(ctx, ref)
}

func (s *Storage) StoreBlob(ctx context.Context, exp Ref, r io.Reader) (SizedRef, error) {
	if exp.Empty() {
		// do not store empty blobs - we can generate them
		var b [1]byte
		_, err := r.Read(b[:])
		if err == io.EOF {
			return SizedRef{Ref: exp, Size: 0}, nil
		}
		return SizedRef{}, fmt.Errorf("expected empty blob")
	}
	if !exp.Zero() {
		if sz, err := s.StatBlob(ctx, exp); err == nil {
			return SizedRef{Ref: exp, Size: sz}, nil
		}
	}
	return s.st.StoreBlob(ctx, exp, r)
}

func (s *Storage) StoreSchema(ctx context.Context, o schema.Object) (SizedRef, error) {
	buf := new(bytes.Buffer)
	if err := schema.Encode(buf, o); err != nil {
		return SizedRef{}, err
	}
	exp := types.BytesRef(buf.Bytes())
	return s.StoreBlob(ctx, exp, buf)
}
