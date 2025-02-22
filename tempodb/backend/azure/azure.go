package azure

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"io/ioutil"
	"path"
	"strings"

	blob "github.com/Azure/azure-storage-blob-go/azblob"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"

	"github.com/grafana/tempo/tempodb/backend"
)

const (
	// dir represents the char separator used by the blob virtual directory structure
	dir = "/"
	// max parallelism on uploads
	maxParallelism = 3
)

type readerWriter struct {
	cfg                *Config
	containerURL       blob.ContainerURL
	hedgedContainerURL blob.ContainerURL
}

type appendTracker struct {
	Name string
}

// New gets the Azure blob container
func New(cfg *Config) (backend.RawReader, backend.RawWriter, backend.Compactor, error) {
	ctx := context.Background()

	container, err := GetContainer(ctx, cfg, false)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "getting storage container")
	}

	hedgedContainer, err := GetContainer(ctx, cfg, true)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "getting hedged storage container")
	}

	rw := &readerWriter{
		cfg:                cfg,
		containerURL:       container,
		hedgedContainerURL: hedgedContainer,
	}

	return rw, rw, rw, nil
}

// Write implements backend.Writer
func (rw *readerWriter) Write(ctx context.Context, name string, keypath backend.KeyPath, data io.Reader, _ int64, _ bool) error {
	return rw.writer(ctx, bufio.NewReader(data), backend.ObjectFileName(keypath, name))
}

// Append implements backend.Writer
func (rw *readerWriter) Append(ctx context.Context, name string, keypath backend.KeyPath, tracker backend.AppendTracker, buffer []byte) (backend.AppendTracker, error) {
	var a appendTracker
	if tracker == nil {
		a.Name = backend.ObjectFileName(keypath, name)

		err := rw.writeAll(ctx, a.Name, buffer)
		if err != nil {
			return nil, err
		}
	} else {
		a = tracker.(appendTracker)

		err := rw.append(ctx, buffer, a.Name)
		if err != nil {
			return nil, err
		}
	}

	return a, nil
}

// CloseAppend implements backend.Writer
func (rw *readerWriter) CloseAppend(ctx context.Context, tracker backend.AppendTracker) error {
	return nil
}

// List implements backend.Reader
func (rw *readerWriter) List(ctx context.Context, keypath backend.KeyPath) ([]string, error) {
	marker := blob.Marker{}
	prefix := path.Join(keypath...)

	if len(prefix) > 0 {
		prefix = prefix + dir
	}

	objects := make([]string, 0)
	for {
		list, err := rw.containerURL.ListBlobsHierarchySegment(ctx, marker, dir, blob.ListBlobsSegmentOptions{
			Prefix:  prefix,
			Details: blob.BlobListingDetails{},
		})
		if err != nil {
			return objects, errors.Wrap(err, "iterating tenants")

		}
		marker = list.NextMarker

		for _, blob := range list.Segment.BlobPrefixes {
			objects = append(objects, strings.TrimPrefix(strings.TrimSuffix(blob.Name, dir), prefix))
		}

		// Continue iterating if we are not done.
		if !marker.NotDone() {
			break
		}
	}
	return objects, nil
}

// Read implements backend.Reader
func (rw *readerWriter) Read(ctx context.Context, name string, keypath backend.KeyPath, _ bool) (io.ReadCloser, int64, error) {
	span, derivedCtx := opentracing.StartSpanFromContext(ctx, "Read")
	defer span.Finish()

	object := backend.ObjectFileName(keypath, name)
	b, err := rw.readAll(derivedCtx, object)
	if err != nil {
		return nil, 0, readError(err)
	}

	return ioutil.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

// ReadRange implements backend.Reader
func (rw *readerWriter) ReadRange(ctx context.Context, name string, keypath backend.KeyPath, offset uint64, buffer []byte) error {
	span, derivedCtx := opentracing.StartSpanFromContext(ctx, "ReadRange")
	defer span.Finish()

	object := backend.ObjectFileName(keypath, name)
	err := rw.readRange(derivedCtx, object, int64(offset), buffer)
	if err != nil {
		return readError(err)
	}

	return nil
}

// Shutdown implements backend.Reader
func (rw *readerWriter) Shutdown() {
}

func (rw *readerWriter) writeAll(ctx context.Context, name string, b []byte) error {
	err := rw.writer(ctx, bytes.NewReader(b), name)
	if err != nil {
		return err
	}

	return nil
}

func (rw *readerWriter) append(ctx context.Context, src []byte, name string) error {
	appendBlobURL := rw.containerURL.NewBlockBlobURL(name)

	// These helper functions convert a binary block ID to a base-64 string and vice versa
	// NOTE: The blockID must be <= 64 bytes and ALL blockIDs for the block must be the same length
	blockIDBinaryToBase64 := func(blockID []byte) string { return base64.StdEncoding.EncodeToString(blockID) }

	blockIDIntToBase64 := func(blockID int) string {
		binaryBlockID := (&[64]byte{})[:]
		binary.LittleEndian.PutUint32(binaryBlockID, uint32(blockID))
		return blockIDBinaryToBase64(binaryBlockID)
	}

	l, err := appendBlobURL.GetBlockList(ctx, blob.BlockListAll, blob.LeaseAccessConditions{})
	if err != nil {
		return err
	}

	// generate the next block id
	id := blockIDIntToBase64(len(l.CommittedBlocks) + 1)

	_, err = appendBlobURL.StageBlock(ctx, id, bytes.NewReader(src), blob.LeaseAccessConditions{}, nil)
	if err != nil {
		return err
	}

	base64BlockIDs := make([]string, len(l.CommittedBlocks)+1)
	for i := 0; i < len(l.CommittedBlocks); i++ {
		base64BlockIDs[i] = l.CommittedBlocks[i].Name
	}

	base64BlockIDs[len(l.CommittedBlocks)] = id

	// After all the blocks are uploaded, atomically commit them to the blob.
	_, err = appendBlobURL.CommitBlockList(ctx, base64BlockIDs, blob.BlobHTTPHeaders{}, blob.Metadata{}, blob.BlobAccessConditions{})
	if err != nil {
		return err
	}
	return nil
}

func (rw *readerWriter) writer(ctx context.Context, src io.Reader, name string) error {
	blobURL := rw.containerURL.NewBlockBlobURL(name)

	if _, err := blob.UploadStreamToBlockBlob(ctx, src, blobURL,
		blob.UploadStreamToBlockBlobOptions{
			BufferSize: rw.cfg.BufferSize,
			MaxBuffers: rw.cfg.MaxBuffers,
		},
	); err != nil {
		return errors.Wrapf(err, "cannot upload blob, name: %s", name)
	}
	return nil
}

func (rw *readerWriter) readRange(ctx context.Context, name string, offset int64, destBuffer []byte) error {
	blobURL := rw.hedgedContainerURL.NewBlockBlobURL(name)

	var props *blob.BlobGetPropertiesResponse
	props, err := blobURL.GetProperties(ctx, blob.BlobAccessConditions{})
	if err != nil {
		return err
	}

	length := int64(len(destBuffer))
	var size int64

	if length > 0 && length <= props.ContentLength()-offset {
		size = length
	} else {
		size = props.ContentLength() - offset
	}

	if err := blob.DownloadBlobToBuffer(context.Background(), blobURL.BlobURL, offset, size,
		destBuffer, blob.DownloadFromBlobOptions{
			BlockSize:   blob.BlobDefaultDownloadBlockSize,
			Parallelism: maxParallelism,
			Progress:    nil,
			RetryReaderOptionsPerBlock: blob.RetryReaderOptions{
				MaxRetryRequests: maxRetries,
			},
		},
	); err != nil {
		return errors.Wrapf(err, "cannot download blob, name: %s", name)
	}

	_, err = bytes.NewReader(destBuffer).Read(destBuffer)
	if err != nil {
		return err
	}

	return nil
}

func (rw *readerWriter) readAll(ctx context.Context, name string) ([]byte, error) {
	blobURL := rw.hedgedContainerURL.NewBlockBlobURL(name)

	var props *blob.BlobGetPropertiesResponse
	props, err := blobURL.GetProperties(ctx, blob.BlobAccessConditions{})
	if err != nil {
		return nil, err
	}

	destBuffer := make([]byte, props.ContentLength())

	if err := blob.DownloadBlobToBuffer(context.Background(), blobURL.BlobURL, 0, props.ContentLength(),
		destBuffer, blob.DownloadFromBlobOptions{
			BlockSize:   blob.BlobDefaultDownloadBlockSize,
			Parallelism: uint16(maxParallelism),
			Progress:    nil,
			RetryReaderOptionsPerBlock: blob.RetryReaderOptions{
				MaxRetryRequests: maxRetries,
			},
		},
	); err != nil {
		return nil, errors.Wrapf(err, "cannot download blob, name: %s", name)
	}

	return destBuffer, nil
}

func readError(err error) error {
	ret, ok := err.(blob.StorageError)
	if !ok {
		return errors.Wrap(err, "reading storage container")
	}
	if ret.ServiceCode() == "BlobNotFound" {
		return backend.ErrDoesNotExist
	}
	return errors.Wrap(err, "reading Azure blob container")
}
