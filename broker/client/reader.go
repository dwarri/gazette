package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"

	"go.gazette.dev/core/broker/codecs"
	pb "go.gazette.dev/core/broker/protocol"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Reader adapts a Read RPC to the io.Reader interface. It additionally supports
// directly reading Fragment URLs advertised but not proxied by the broker (eg,
// because DoNotProxy of the ReadRequest is true). A Reader is invalidated by its
// first returned error, with the exception of ErrOffsetJump: this error is
// returned to notify the client that the next Journal offset to be Read is not
// the offset that was requested, but the Reader is prepared to continue at the
// updated offset.
type Reader struct {
	Request  pb.ReadRequest  // ReadRequest of the Reader.
	Response pb.ReadResponse // Most recent ReadResponse from broker.

	ctx    context.Context
	client pb.RoutedJournalClient // Client against which Read is dispatched.
	stream pb.Journal_ReadClient  // Server stream.
	direct io.ReadCloser          // Directly opened Fragment URL.
}

// NewReader returns a Reader initialized with the given BrokerClient and ReadRequest.
func NewReader(ctx context.Context, client pb.RoutedJournalClient, req pb.ReadRequest) *Reader {
	var r = &Reader{
		Request: req,
		ctx:     ctx,
		client:  client,
	}
	return r
}

func (r *Reader) Read(p []byte) (n int, err error) {
	// If we have an open direct reader of a persisted fragment, delegate to it.
	if r.direct != nil {
		if n, err = r.direct.Read(p); err != nil {
			_ = r.direct.Close()
		}
		r.Request.Offset += int64(n)
		return
	}

	// Is there remaining content in the last ReadResponse?
	if l, d := len(r.Response.Content), int(r.Request.Offset-r.Response.Offset); l != 0 && l > d {
		n = copy(p, r.Response.Content[d:])
		r.Request.Offset += int64(n)
		return
	}

	// Lazy initialization: begin the Read RPC.
	if r.stream == nil {
		if r.stream, err = r.client.Read(
			pb.WithDispatchItemRoute(r.ctx, r.client, r.Request.Journal.String(), false),
			&r.Request,
		); err == nil {
			n, err = r.Read(p) // Recurse to attempt read against opened |r.stream|.
		} else {
			err = mapGRPCCtxErr(r.ctx, err)
		}
		return
	}

	// Read and Validate the next frame.
	if err = r.stream.RecvMsg(&r.Response); err == nil {
		if err = r.Response.Validate(); err != nil {
			err = pb.ExtendContext(err, "ReadResponse")
		} else if r.Response.Status == pb.Status_OK && r.Response.Offset < r.Request.Offset {
			err = pb.NewValidationError("invalid ReadResponse offset (%d; expected >= %d)",
				r.Response.Offset, r.Request.Offset) // Violation of Read API contract.
		}
	} else {
		err = mapGRPCCtxErr(r.ctx, err)
	}

	// A note on resource leaks: an invariant of Read is that in invocations where
	// the returned error != nil, an error has also been read from |r.stream|,
	// implying that the gRPC stream has been torn down. The exception is if
	// response validation fails, which indicates a client / server API version
	// incompatibility and cannot happen in normal operation.

	if err == nil {
		// If a Header was sent, advise of its advertised journal Route.
		if r.Response.Header != nil {
			r.client.UpdateRoute(r.Request.Journal.String(), &r.Response.Header.Route)
		}

		if r.Request.Offset < r.Response.Offset {
			// Offset jumps are uncommon, but possible if fragments were removed,
			// or if the requested offset was -1.
			r.Request.Offset = r.Response.Offset
			err = ErrOffsetJump
		}

		if r.Response.Status == pb.Status_OK {
			// Return empty read, to allow inspection of the updated |r.Response|.
		} else {
			// The broker will send a stream closure following a !OK status.
			// Recurse to read that closure, and _then_ return a final error.
			n, err = r.Read(p)
		}
		return

	} else if err != io.EOF {
		// We read an error _other_ than a graceful tear-down of the stream.
		return
	}

	// We read a graceful stream closure (err == io.EOF).

	// If the frame preceding EOF provided a fragment URL, open it directly.
	if !r.Request.MetadataOnly && r.Response.Status == pb.Status_OK && r.Response.FragmentUrl != "" {
		if r.direct, err = OpenFragmentURL(r.ctx, *r.Response.Fragment,
			r.Request.Offset, r.Response.FragmentUrl); err == nil {
			n, err = r.Read(p) // Recurse to attempt read against opened |r.direct|.
		}
		return
	}

	// Otherwise, map Status of the preceding frame into a more specific error.
	switch r.Response.Status {
	case pb.Status_OK:
		if err != io.EOF {
			panic(err.Error()) // Status_OK implies graceful stream closure.
		}
	case pb.Status_NOT_JOURNAL_BROKER:
		err = ErrNotJournalBroker
	case pb.Status_OFFSET_NOT_YET_AVAILABLE:
		err = ErrOffsetNotYetAvailable
	default:
		err = errors.New(r.Response.Status.String())
	}
	return
}

// AdjustedOffset returns the current journal offset, adjusted for content read
// by |br| (which wraps this Reader) but not yet consumed from |br|'s buffer.
func (r *Reader) AdjustedOffset(br *bufio.Reader) int64 {
	return r.Request.Offset - int64(br.Buffered())
}

// Seek provides a limited form of seeking support. Specifically, iff a
// Fragment URL is being directly read, the Seek offset is ahead of the current
// Reader offset, and the Fragment also covers the desired Seek offset, then a
// seek is performed by reading and discarding to the seeked offset. Seek will
// otherwise return ErrSeekRequiresNewReader.
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		// |offset| is already absolute.
	case io.SeekCurrent:
		offset = r.Request.Offset + offset
	case io.SeekEnd:
		return r.Request.Offset, errors.New("io.SeekEnd whence is not supported")
	default:
		panic("invalid whence")
	}

	if r.direct == nil || offset < r.Request.Offset || offset >= r.Response.Fragment.End {
		return r.Request.Offset, ErrSeekRequiresNewReader
	}

	var _, err = io.CopyN(ioutil.Discard, r, offset-r.Request.Offset)
	return r.Request.Offset, err
}

// OpenFragmentURL directly opens |fragment|, which must be available at URL
// |url|, and returns a *FragmentReader which has been pre-seeked to |offset|.
func OpenFragmentURL(ctx context.Context, fragment pb.Fragment, offset int64, url string) (*FragmentReader, error) {
	var req, err = http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	if fragment.CompressionCodec == pb.CompressionCodec_GZIP_OFFLOAD_DECOMPRESSION {
		// Require that the server send us un-encoded content, offloading
		// decompression onto the storage API. Go's standard `gzip` package is slow,
		// and we also see a parallelism benefit by offloading decompression work
		// onto the cloud storage system.
		req.Header.Set("Accept-Encoding", "identity")
	} else if fragment.CompressionCodec == pb.CompressionCodec_GZIP {
		// Explicitly request gzip. Doing so disables the http client's transparent
		// handling for gzip decompression if the Fragment happened to be written with
		// "Content-Encoding: gzip", and it instead directly surfaces the compressed
		// bytes to us.
		req.Header.Set("Accept-Encoding", "gzip")
	}

	resp, err := httpClient.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	} else if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("!OK fetching (%s, %q)", resp.Status, url)
	}

	// Technically the store _must_ decompress in response to honor our
	// Accept-Encoding header, but some implementations (eg, Minio) don't.
	if fragment.CompressionCodec == pb.CompressionCodec_GZIP_OFFLOAD_DECOMPRESSION &&
		resp.Header.Get("Content-Encoding") == "gzip" {

		fragment.CompressionCodec = pb.CompressionCodec_GZIP // Decompress client-side.
	}
	return NewFragmentReader(resp.Body, fragment, offset)
}

// NewFragmentReader wraps |rc|, which is a io.ReadCloser of raw Fragment bytes,
// with a returned *FragmentReader which has been pre-seeked to |offset|.
func NewFragmentReader(rc io.ReadCloser, fragment pb.Fragment, offset int64) (*FragmentReader, error) {
	var decomp, err = codecs.NewCodecReader(rc, fragment.CompressionCodec)
	if err != nil {
		_ = rc.Close()
		return nil, err
	}

	var fr = &FragmentReader{
		decomp:   decomp,
		raw:      rc,
		Fragment: fragment,
		Offset:   fragment.Begin,
	}

	// Attempt to seek to |offset| within the fragment.
	var delta = offset - fragment.Begin
	if _, err = io.CopyN(ioutil.Discard, fr, delta); err != nil {
		_ = fr.Close()
		return nil, err
	}
	return fr, nil
}

// FragmentReader is a io.ReadCloser of a Fragment.
type FragmentReader struct {
	pb.Fragment       // Fragment being read.
	Offset      int64 // Next journal offset to be read, in range [Begin, End).

	decomp io.ReadCloser
	raw    io.ReadCloser
}

// Read returns the next bytes of decompressed Fragment content. When Read
// returns, Offset has been updated to reflect the next byte to be read.
// Read returns EOF only if the underlying Reader returns EOF at precisely
// Offset == Fragment.End. If the underlying Reader is too short,
// io.ErrUnexpectedEOF is returned. If it's too long, ErrDidNotReadExpectedEOF
// is returned.
func (fr *FragmentReader) Read(p []byte) (n int, err error) {
	n, err = fr.decomp.Read(p)
	fr.Offset += int64(n)

	if fr.Offset > fr.Fragment.End {
		// Did we somehow read beyond Fragment.End?
		n -= int(fr.Offset - fr.Fragment.End)
		err = ErrDidNotReadExpectedEOF
	} else if err == io.EOF && fr.Offset != fr.Fragment.End {
		// Did we read EOF before the reaching Fragment.End?
		err = io.ErrUnexpectedEOF
	}
	return
}

// Close closes the underlying ReadCloser and associated
// decompressor (if any).
func (fr *FragmentReader) Close() error {
	var errA, errB = fr.decomp.Close(), fr.raw.Close()
	if errA != nil {
		return errA
	}
	return errB
}

// InstallFileTransport registers a file:// protocol handler rooted at |root|
// with the http.Client used by OpenFragmentURL. The returned cleanup function
// removes the handler and restores the prior http.Client.
func InstallFileTransport(root string) (remove func()) {
	var transport = new(http.Transport)
	*transport = *http.DefaultTransport.(*http.Transport) // Clone.
	transport.RegisterProtocol("file", http.NewFileTransport(http.Dir(root)))

	var prevClient = httpClient
	httpClient = &http.Client{Transport: transport}

	return func() { httpClient = prevClient }
}

// mapGRPCCtxErr returns ctx.Err() iff |err| represents a gRPC error with a
// status code matching ctx.Err(). Otherwise, it returns |err| unmodified.
// In other words, this routine "unwraps" gRPC errors which have their root cause
// in a local Context error, instead mapping to the common context package errors.
func mapGRPCCtxErr(ctx context.Context, err error) error {
	if ctx.Err() == context.DeadlineExceeded && status.Code(err) == codes.DeadlineExceeded {
		return ctx.Err()
	}
	if ctx.Err() == context.Canceled && status.Code(err) == codes.Canceled {
		return ctx.Err()
	}
	return err
}

var (
	// Map common broker error statuses into named errors.
	ErrNotJournalBroker        = errors.New(pb.Status_NOT_JOURNAL_BROKER.String())
	ErrNotJournalPrimaryBroker = errors.New(pb.Status_NOT_JOURNAL_PRIMARY_BROKER.String())
	ErrOffsetNotYetAvailable   = errors.New(pb.Status_OFFSET_NOT_YET_AVAILABLE.String())
	ErrWrongAppendOffset       = errors.New(pb.Status_WRONG_APPEND_OFFSET.String())

	ErrOffsetJump            = errors.New("offset jump")
	ErrSeekRequiresNewReader = errors.New("seek offset requires new Reader")
	ErrDidNotReadExpectedEOF = errors.New("did not read EOF at expected Fragment.End")

	// httpClient is the http.Client used by OpenFragmentURL
	httpClient = http.DefaultClient
)
