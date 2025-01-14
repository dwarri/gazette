package store_rocksdb

import (
	"context"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	rocks "github.com/tecbot/gorocksdb"
	"go.gazette.dev/core/broker/client"
	pb "go.gazette.dev/core/broker/protocol"
	"go.gazette.dev/core/brokertest"
	"go.gazette.dev/core/consumer/recoverylog"
	"go.gazette.dev/core/etcdtest"
)

func TestSimpleStopAndStart(t *testing.T) {
	var bk, cleanup = newBrokerAndLog(t)
	defer cleanup()

	var replica1 = NewTestReplica(t, bk)
	defer replica1.teardown()

	replica1.startWriting(aRecoveryLog)
	replica1.put("key3", "value three!")
	replica1.put("key1", "value one")
	replica1.put("key2", "value2")

	var hints, _ = replica1.recorder.BuildHints()

	// |replica1| was initialized from empty hints and began writing at the
	// recoverylog head (offset -1). However, expect that the produced hints
	// reference absolute offsets of the log.
	assert.NotNil(t, hints.LiveNodes)
	for _, node := range hints.LiveNodes {
		for _, s := range node.Segments {
			assert.True(t, s.FirstOffset >= 0)
		}
	}

	var replica2 = NewTestReplica(t, bk)
	defer replica2.teardown()

	replica2.startReading(hints)
	replica2.makeLive()

	replica2.expectValues(map[string]string{
		"key1": "value one",
		"key2": "value2",
		"key3": "value three!",
	})

	// Expect |replica1| & |replica2| share identical non-empty properties
	// (specifically, properties hold the /IDENTITY GUID that RocksDB creates at initialization).
	var h1, _ = replica1.recorder.BuildHints()
	var h2, _ = replica2.recorder.BuildHints()

	assert.NotEmpty(t, h1.Properties)
	assert.Equal(t, h1.Properties[0].Path, "/IDENTITY")
	assert.Equal(t, h1.Properties, h2.Properties)
}

func TestWarmStandbyHandoff(t *testing.T) {
	var bk, cleanup = newBrokerAndLog(t)
	defer cleanup()

	var fo = rocks.NewDefaultFlushOptions()
	fo.SetWait(true) // Sync to log before returning.
	defer fo.Destroy()

	var replica1 = NewTestReplica(t, bk)
	defer replica1.teardown()
	var replica2 = NewTestReplica(t, bk)
	defer replica2.teardown()
	var replica3 = NewTestReplica(t, bk)
	defer replica3.teardown()

	replica1.startWriting(aRecoveryLog)
	var hints, _ = replica1.recorder.BuildHints()

	// Both replicas begin reading at the same time.
	replica2.startReading(hints)
	replica3.startReading(hints)

	// |replica1| writes content, while |replica2| & |replica3| are reading.
	replica1.put("key foo", "baz")
	replica1.put("key bar", "bing")
	assert.NoError(t, replica1.db.Flush(fo))

	// Make |replica2| live. Expect |replica1|'s content to be present.
	replica2.makeLive()
	replica2.expectValues(map[string]string{
		"key foo": "baz",
		"key bar": "bing",
	})

	// Begin raced writes. We expect that the hand-off mechanism allows |replica3|
	// to consistently follow |replica2|'s fork of history.
	replica1.put("raced", "and loses")
	assert.NoError(t, replica1.db.Flush(fo))
	replica2.put("raced", "and wins")
	assert.NoError(t, replica2.db.Flush(fo))

	replica3.makeLive()
	replica2.expectValues(map[string]string{
		"key foo": "baz",
		"key bar": "bing",
		"raced":   "and wins",
	})

	// Expect |replica2| & |replica3| share identical, non-empty properties.
	var h1, _ = replica1.recorder.BuildHints()
	var h2, _ = replica2.recorder.BuildHints()

	assert.NotEmpty(t, h1.Properties)
	assert.Equal(t, h1.Properties[0].Path, "/IDENTITY")
	assert.Equal(t, h1.Properties, h2.Properties)
}

func TestResolutionOfConflictingWriters(t *testing.T) {
	var bk, cleanup = newBrokerAndLog(t)
	defer cleanup()

	// Begin with two replicas.
	var replica1 = NewTestReplica(t, bk)
	defer replica1.teardown()
	var replica2 = NewTestReplica(t, bk)
	defer replica2.teardown()

	// |replica1| begins as primary.
	replica1.startWriting(aRecoveryLog)
	var hints, _ = replica1.recorder.BuildHints()
	replica2.startReading(hints)
	replica1.put("key one", "value one")

	// |replica2| now becomes live. |replica1| and |replica2| intersperse writes.
	replica2.makeLive()
	replica1.put("rep1 foo", "value foo")
	replica2.put("rep2 bar", "value bar")
	replica1.put("rep1 baz", "value baz")
	replica2.put("rep2 bing", "value bing")

	var fo = rocks.NewDefaultFlushOptions()
	fo.SetWait(true) // Sync to log before returning.
	defer fo.Destroy()

	assert.NoError(t, replica1.db.Flush(fo))
	assert.NoError(t, replica2.db.Flush(fo))

	// New |replica3| is hinted from |replica1|, and |replica4| from |replica2|.
	var replica3 = NewTestReplica(t, bk)
	defer replica3.teardown()
	var replica4 = NewTestReplica(t, bk)
	defer replica4.teardown()

	hints, _ = replica1.recorder.BuildHints()
	replica3.startReading(hints)
	replica3.makeLive()

	hints, _ = replica2.recorder.BuildHints()
	replica4.startReading(hints)
	replica4.makeLive()

	// Expect |replica3| recovered |replica1| history.
	replica3.expectValues(map[string]string{
		"key one":  "value one",
		"rep1 foo": "value foo",
		"rep1 baz": "value baz",
	})
	// Expect |replica4| recovered |replica2| history.
	replica4.expectValues(map[string]string{
		"key one":   "value one",
		"rep2 bar":  "value bar",
		"rep2 bing": "value bing",
	})
}

func TestPlayThenCancel(t *testing.T) {
	var bk, cleanup = newBrokerAndLog(t)
	defer cleanup()

	var r = NewTestReplica(t, bk)
	defer r.teardown()

	// Create a Context which will cancel itself after a delay.
	var deadlineCtx, _ = context.WithDeadline(context.Background(), time.Now().Add(time.Millisecond*10))
	// Blocks until |ctx| is cancelled.
	var err = r.player.Play(deadlineCtx, recoverylog.FSMHints{Log: aRecoveryLog}, r.tmpdir, bk)
	assert.Equal(t, context.DeadlineExceeded, errors.Cause(err))

	r.player.FinishAtWriteHead() // A raced call to FinishAtWriteHead doesn't block.
	<-r.player.Done()

	// Expect the local directory was deleted.
	_, err = os.Stat(r.tmpdir)
	assert.True(t, os.IsNotExist(err))
}

func TestCancelThenPlay(t *testing.T) {
	var bk, cleanup = newBrokerAndLog(t)
	defer cleanup()

	var r = NewTestReplica(t, bk)
	defer r.teardown()

	// Create a Context which is cancelled immediately.
	var cancelCtx, cancelFn = context.WithCancel(context.Background())
	cancelFn()

	assert.EqualError(t, r.player.Play(cancelCtx, recoverylog.FSMHints{Log: aRecoveryLog}, r.tmpdir, bk),
		`determining log head: context canceled`)

	<-r.player.Done()
}

// Models the typical lifetime of an observed rocks database:
//  * Begin by reading from the most-recent available hints.
//  * When ready, make the database "Live".
//  * Perform new writes against the replica, which are recorded in the log.
type testReplica struct {
	client client.AsyncJournalClient

	tmpdir string
	dbO    *rocks.Options
	dbWO   *rocks.WriteOptions
	dbRO   *rocks.ReadOptions
	db     *rocks.DB

	author   recoverylog.Author
	recorder *recoverylog.Recorder
	player   *recoverylog.Player
	t        assert.TestingT
}

func NewTestReplica(t assert.TestingT, client client.AsyncJournalClient) *testReplica {
	var r = &testReplica{
		client: client,
		player: recoverylog.NewPlayer(),
		t:      t,
	}

	var err error
	r.tmpdir, err = ioutil.TempDir("", "store-rocksdb-test")
	assert.NoError(r.t, err)

	r.author, err = recoverylog.NewRandomAuthorID()
	assert.NoError(r.t, err)

	return r
}

func (r *testReplica) startReading(hints recoverylog.FSMHints) {
	go func() {
		assert.NoError(r.t, r.player.Play(context.Background(), hints, r.tmpdir, r.client))
	}()
}

func (r *testReplica) startWriting(log pb.Journal) {
	var fsm, err = recoverylog.NewFSM(recoverylog.FSMHints{Log: log})
	assert.NoError(r.t, err)
	r.initDB(fsm)
}

// Finish playback, build a new recorder, and open an observed database.
func (r *testReplica) makeLive() {
	r.player.InjectHandoff(r.author)
	<-r.player.Done()

	assert.NotNil(r.t, r.player.FSM)

	r.initDB(r.player.FSM)
}

func (r *testReplica) initDB(fsm *recoverylog.FSM) {
	r.recorder = recoverylog.NewRecorder(fsm, r.author, r.tmpdir, r.client)

	r.dbO = rocks.NewDefaultOptions()
	r.dbO.SetCreateIfMissing(true)
	r.dbO.SetEnv(NewHookedEnv(NewRecorder(r.recorder)))

	r.dbRO = rocks.NewDefaultReadOptions()

	r.dbWO = rocks.NewDefaultWriteOptions()
	r.dbWO.SetSync(true)

	var err error
	r.db, err = rocks.OpenDb(r.dbO, r.tmpdir)
	assert.NoError(r.t, err)
}

func (r *testReplica) put(key, value string) {
	assert.NoError(r.t, r.db.Put(r.dbWO, []byte(key), []byte(value)))
}

func (r *testReplica) expectValues(expect map[string]string) {
	it := r.db.NewIterator(r.dbRO)
	defer it.Close()

	it.SeekToFirst()
	for ; it.Valid(); it.Next() {
		key, value := string(it.Key().Data()), string(it.Value().Data())

		assert.Equal(r.t, expect[key], value)
		delete(expect, key)
	}
	assert.NoError(r.t, it.Err())
	assert.Empty(r.t, expect)
}

func (r *testReplica) teardown() {
	if r.db != nil {
		r.db.Close()
		r.dbRO.Destroy()
		r.dbWO.Destroy()
		r.dbO.Destroy()
	}
	assert.NoError(r.t, os.RemoveAll(r.tmpdir))
}

func newBrokerAndLog(t assert.TestingT) (client.AsyncJournalClient, func()) {
	var etcd = etcdtest.TestClient()
	var broker = brokertest.NewBroker(t, etcd, "local", "broker")

	brokertest.CreateJournals(t, broker, brokertest.Journal(pb.JournalSpec{Name: aRecoveryLog}))

	var rjc = pb.NewRoutedJournalClient(broker.Client(), pb.NoopDispatchRouter{})
	var as = client.NewAppendService(context.Background(), rjc)

	return as, func() {
		broker.Tasks.Cancel()
		assert.NoError(t, broker.Tasks.Wait())
		etcdtest.Cleanup()
	}
}

const aRecoveryLog pb.Journal = "test/store-rocksdb/recovery-log"
