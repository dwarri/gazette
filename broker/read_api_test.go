package broker

import (
	"context"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.gazette.dev/core/broker/codecs"
	"go.gazette.dev/core/broker/fragment"
	pb "go.gazette.dev/core/broker/protocol"
	"go.gazette.dev/core/etcdtest"
)

func TestReadStreaming(t *testing.T) {
	var ctx, etcd = pb.WithDispatchDefault(context.Background()), etcdtest.TestClient()
	defer etcdtest.Cleanup()

	// Make |chunkSize| small so we can test for chunking effects.
	defer func(cs int) { chunkSize = cs }(chunkSize)
	chunkSize = 5

	var broker = newTestBroker(t, etcd, pb.ProcessSpec_ID{Zone: "local", Suffix: "broker"})
	setTestJournal(broker, pb.JournalSpec{Name: "a/journal", Replication: 1}, broker.id)

	// Grab the spool so we can apply fixtures we'll expect to read.
	var spool = <-broker.replica("a/journal").spoolCh

	ctx, cancel := context.WithCancel(ctx)

	var stream, err = broker.client().Read(ctx,
		&pb.ReadRequest{
			Journal:      "a/journal",
			Offset:       0,
			Block:        true,
			DoNotProxy:   true,
			MetadataOnly: false,
		})
	assert.NoError(t, err)

	spool.MustApply(&pb.ReplicateRequest{Content: []byte("foobarbaz")})
	spool.MustApply(&pb.ReplicateRequest{Proposal: boxFragment(spool.Next())})

	expectReadResponse(t, stream, pb.ReadResponse{
		Status:    pb.Status_OK,
		Header:    broker.header("a/journal"),
		Offset:    0,
		WriteHead: 9,
		Fragment: &pb.Fragment{
			Journal:          "a/journal",
			Begin:            0,
			End:              9,
			Sum:              pb.SHA1SumOf("foobarbaz"),
			CompressionCodec: pb.CompressionCodec_NONE,
		},
	})
	expectReadResponse(t, stream, pb.ReadResponse{
		Status:  pb.Status_OK,
		Offset:  0,
		Content: []byte("fooba"),
	})
	expectReadResponse(t, stream, pb.ReadResponse{
		Status:  pb.Status_OK,
		Offset:  5,
		Content: []byte("rbaz"),
	})

	// Commit more content. Expect the committed Fragment metadata is sent,
	// along with new commit content.
	spool.MustApply(&pb.ReplicateRequest{Content: []byte("bing")})
	spool.MustApply(&pb.ReplicateRequest{Proposal: boxFragment(spool.Next())})

	expectReadResponse(t, stream, pb.ReadResponse{
		Status:    pb.Status_OK,
		Offset:    9,
		WriteHead: 13,
		Fragment: &pb.Fragment{
			Journal:          "a/journal",
			Begin:            0,
			End:              13,
			Sum:              pb.SHA1SumOf("foobarbazbing"),
			CompressionCodec: pb.CompressionCodec_NONE,
		},
	})
	expectReadResponse(t, stream, pb.ReadResponse{
		Status:  pb.Status_OK,
		Offset:  9,
		Content: []byte("bing"),
	})

	cancel()
	_, err = stream.Recv()
	assert.EqualError(t, err, `rpc error: code = Canceled desc = context canceled`)

	broker.replica("a/journal").spoolCh <- spool
	broker.cleanup()
}

func TestReadMetadataAndNonBlocking(t *testing.T) {
	var ctx, etcd = pb.WithDispatchDefault(context.Background()), etcdtest.TestClient()
	defer etcdtest.Cleanup()

	var broker = newTestBroker(t, etcd, pb.ProcessSpec_ID{Zone: "local", Suffix: "broker"})
	setTestJournal(broker, pb.JournalSpec{Name: "a/journal", Replication: 1}, broker.id)

	// Grab the spool so we can apply fixtures we'll expect to read.
	var spool = <-broker.replica("a/journal").spoolCh

	spool.MustApply(&pb.ReplicateRequest{Content: []byte("feedbeef")})
	spool.MustApply(&pb.ReplicateRequest{Proposal: boxFragment(spool.Next())})

	stream, err := broker.client().Read(ctx, &pb.ReadRequest{
		Journal:      "a/journal",
		Offset:       3,
		Block:        false,
		MetadataOnly: false,
	})
	assert.NoError(t, err)

	expectReadResponse(t, stream, pb.ReadResponse{
		Status:    pb.Status_OK,
		Header:    broker.header("a/journal"),
		Offset:    3,
		WriteHead: 8,
		Fragment: &pb.Fragment{
			Journal:          "a/journal",
			Begin:            0,
			End:              8,
			Sum:              pb.SHA1SumOf("feedbeef"),
			CompressionCodec: pb.CompressionCodec_NONE,
		},
	})
	expectReadResponse(t, stream, pb.ReadResponse{
		Status:  pb.Status_OK,
		Offset:  3,
		Content: []byte("dbeef"),
	})
	expectReadResponse(t, stream, pb.ReadResponse{
		Status:    pb.Status_OFFSET_NOT_YET_AVAILABLE,
		Offset:    8,
		WriteHead: 8,
	})
	_, err = stream.Recv() // Broker closes.
	assert.Equal(t, io.EOF, err)

	// Now, issue a blocking metadata-only request.
	stream, err = broker.client().Read(ctx, &pb.ReadRequest{
		Journal:      "a/journal",
		Offset:       8,
		Block:        true,
		MetadataOnly: true,
	})
	assert.NoError(t, err)

	// Commit more content, unblocking our metadata request.
	spool.MustApply(&pb.ReplicateRequest{Content: []byte("bing")})
	spool.MustApply(&pb.ReplicateRequest{Proposal: boxFragment(spool.Next())})

	expectReadResponse(t, stream, pb.ReadResponse{
		Status:    pb.Status_OK,
		Header:    broker.header("a/journal"),
		Offset:    8,
		WriteHead: 12,
		Fragment: &pb.Fragment{
			Journal:          "a/journal",
			Begin:            0,
			End:              12,
			Sum:              pb.SHA1SumOf("feedbeefbing"),
			CompressionCodec: pb.CompressionCodec_NONE,
		},
	})
	// Expect no data is sent, and the stream is closed.
	_, err = stream.Recv()
	assert.Equal(t, io.EOF, err)

	broker.replica("a/journal").spoolCh <- spool
	broker.cleanup()
}

func TestReadProxyCases(t *testing.T) {
	var ctx, etcd = pb.WithDispatchDefault(context.Background()), etcdtest.TestClient()
	defer etcdtest.Cleanup()

	var broker = newTestBroker(t, etcd, pb.ProcessSpec_ID{Zone: "local", Suffix: "broker"})
	var peer = newMockBroker(t, etcd, pb.ProcessSpec_ID{Zone: "peer", Suffix: "broker"})
	setTestJournal(broker, pb.JournalSpec{Name: "a/journal", Replication: 1}, peer.id)

	// Case: successfully proxies from peer.
	var req = &pb.ReadRequest{
		Journal:      "a/journal",
		Block:        true,
		DoNotProxy:   false,
		MetadataOnly: false,
	}
	var stream, _ = broker.client().Read(ctx, req)

	// Expect initial request is proxied to the peer, with attached Header.
	req.Header = broker.header("a/journal")
	assert.Equal(t, req, <-peer.ReadReqCh)

	// Peer responds, and broker proxies.
	peer.ReadRespCh <- &pb.ReadResponse{Offset: 1234}
	peer.ReadRespCh <- &pb.ReadResponse{Offset: 5678}
	peer.ErrCh <- nil // EOF.

	expectReadResponse(t, stream, pb.ReadResponse{Offset: 1234})
	expectReadResponse(t, stream, pb.ReadResponse{Offset: 5678})
	var _, err = stream.Recv() // Broker proxies EOF.
	assert.Equal(t, io.EOF, err)

	// Case: proxy is not allowed.
	req = &pb.ReadRequest{
		Journal:    "a/journal",
		DoNotProxy: true,
	}
	stream, _ = broker.client().Read(ctx, req)

	expectReadResponse(t, stream, pb.ReadResponse{
		Status: pb.Status_NOT_JOURNAL_BROKER,
		Header: boxHeaderProcessID(*broker.header("a/journal"), broker.id),
	})
	_, err = stream.Recv() // Broker closes.
	assert.Equal(t, io.EOF, err)

	// Case: remote broker returns an error.
	req = &pb.ReadRequest{
		Journal: "a/journal",
		Offset:  0,
	}
	stream, _ = broker.client().Read(ctx, req)

	// Peer reads request, and returns an error.
	_ = <-peer.ReadReqCh
	peer.ErrCh <- errors.New("some kind of error")
	_, err = stream.Recv() // Broker proxies error.
	assert.EqualError(t, err, `rpc error: code = Unknown desc = some kind of error`)

	// Case: remote read is blocked, but we're signaled to stop proxying.
	stream, _ = broker.client().Read(ctx, req)

	// Peer reads request, sends one chunk and then blocks.
	_ = <-peer.ReadReqCh
	peer.ReadRespCh <- &pb.ReadResponse{Offset: 1234}
	expectReadResponse(t, stream, pb.ReadResponse{Offset: 1234})

	// Cancel. Expect we immediately read EOF.
	close(broker.svc.stopProxyReadsCh)
	_, err = stream.Recv()
	assert.Equal(t, io.EOF, err)
	broker.svc.stopProxyReadsCh = make(chan struct{}) // Cleanup.

	broker.cleanup()
	peer.Cleanup()
}

func TestReadRemoteFragmentCases(t *testing.T) {
	var ctx, etcd = pb.WithDispatchDefault(context.Background()), etcdtest.TestClient()
	defer etcdtest.Cleanup()

	var broker = newTestBroker(t, etcd, pb.ProcessSpec_ID{Zone: "local", Suffix: "broker"})
	setTestJournal(broker, pb.JournalSpec{Name: "a/journal", Replication: 1}, broker.id)

	// Create a remote fragment fixture with journal content.
	var frag, tmpDir = buildRemoteFragmentFixture(t)

	defer func() { assert.NoError(t, os.RemoveAll(tmpDir)) }()
	defer func(s string) { fragment.FileSystemStoreRoot = s }(fragment.FileSystemStoreRoot)
	fragment.FileSystemStoreRoot = tmpDir

	// Resolve, and update the replica index to reflect the remote fragment fixture.
	broker.replica("a/journal").index.ReplaceRemote(fragment.CoverSet{fragment.Fragment{Fragment: frag}})

	// Case: non-blocking read which is permitted to proxy. Expect the remote
	// fragment is decompressed and seek'd to the desired offset.
	var stream, err = broker.client().Read(pb.WithDispatchDefault(ctx),
		&pb.ReadRequest{
			Journal:      "a/journal",
			Offset:       100,
			Block:        false,
			DoNotProxy:   false,
			MetadataOnly: false,
		})
	assert.NoError(t, err)

	expectReadResponse(t, stream, pb.ReadResponse{
		Status:      pb.Status_OK,
		Header:      broker.header("a/journal"),
		Offset:      100,
		WriteHead:   120,
		Fragment:    &frag,
		FragmentUrl: "file:///" + frag.ContentPath(),
	})
	expectReadResponse(t, stream, pb.ReadResponse{
		Status:  pb.Status_OK,
		Offset:  100,
		Content: []byte("remote fragment data"),
	})
	expectReadResponse(t, stream, pb.ReadResponse{
		Status:    pb.Status_OFFSET_NOT_YET_AVAILABLE,
		Offset:    120,
		WriteHead: 120,
	})
	_, err = stream.Recv() // Broker closes with remote fragment EOF.
	assert.Equal(t, io.EOF, err)

	// Case: non-blocking read which is not permitted to proxy. Remote fragment is not read.
	stream, err = broker.client().Read(pb.WithDispatchDefault(ctx),
		&pb.ReadRequest{
			Journal:      "a/journal",
			Offset:       100,
			Block:        false,
			DoNotProxy:   true,
			MetadataOnly: false,
		})
	assert.NoError(t, err)

	expectReadResponse(t, stream, pb.ReadResponse{
		Status:      pb.Status_OK,
		Header:      broker.header("a/journal"),
		Offset:      100,
		WriteHead:   120,
		Fragment:    &frag,
		FragmentUrl: "file:///" + frag.ContentPath(),
	})
	_, err = stream.Recv() // Broker closes.
	assert.Equal(t, io.EOF, err)

	broker.cleanup()
}

func TestReadRequestErrorCases(t *testing.T) {
	var ctx, etcd = pb.WithDispatchDefault(context.Background()), etcdtest.TestClient()
	defer etcdtest.Cleanup()

	var broker = newTestBroker(t, etcd, pb.ProcessSpec_ID{Zone: "local", Suffix: "broker"})

	// Case: ReadRequest which fails to validate.
	var stream, err = broker.client().Read(ctx, &pb.ReadRequest{Journal: "/invalid/journal"})
	assert.NoError(t, err)

	_, err = stream.Recv()
	assert.EqualError(t, err, `rpc error: code = Unknown desc = Journal: cannot begin with '/' (/invalid/journal)`)

	// Case: Read of a write-only journal.
	setTestJournal(broker, pb.JournalSpec{Name: "write/only", Replication: 1, Flags: pb.JournalSpec_O_WRONLY}, broker.id)
	stream, err = broker.client().Read(ctx, &pb.ReadRequest{Journal: "write/only"})
	assert.NoError(t, err)

	resp, err := stream.Recv()
	assert.NoError(t, err)
	assert.Equal(t, &pb.ReadResponse{
		Status: pb.Status_NOT_ALLOWED,
		Header: broker.header("write/only"),
	}, resp)

	broker.cleanup()
}

func buildRemoteFragmentFixture(t assert.TestingT) (frag pb.Fragment, dir string) {
	const data = "XXXXXremote fragment data"

	var err error
	dir, err = ioutil.TempDir("", "BrokerSuite")
	assert.NoError(t, err)

	frag = pb.Fragment{
		Journal:          "a/journal",
		Begin:            95,
		End:              120,
		Sum:              pb.SHA1SumOf(data),
		CompressionCodec: pb.CompressionCodec_SNAPPY,
		BackingStore:     pb.FragmentStore("file:///"),
		ModTime:          time.Unix(1234567, 0).Unix(),
	}

	var path = filepath.Join(dir, frag.ContentPath())
	assert.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))

	file, err := os.Create(path)
	assert.NoError(t, err)

	comp, err := codecs.NewCodecWriter(file, pb.CompressionCodec_SNAPPY)
	assert.NoError(t, err)
	_, err = comp.Write([]byte(data))
	assert.NoError(t, err)
	assert.NoError(t, comp.Close())
	assert.NoError(t, file.Close())
	return
}

func boxFragment(f pb.Fragment) *pb.Fragment { return &f }

func expectReadResponse(t assert.TestingT, stream pb.Journal_ReadClient, expect pb.ReadResponse) {
	var resp, err = stream.Recv()
	assert.NoError(t, err)
	assert.Equal(t, expect, *resp)
}
