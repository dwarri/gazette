package topic

import (
	"sync"

	rocks "github.com/tecbot/gorocksdb"

	"github.com/pippio/api-server/gazette"
)

type ShardID int

type ConsumerContext struct {
	// The shard for which we’re consuming.
	Shard ShardID

	// A consumer may wish to maintain in-memory state for
	// performance reasons. Examples could include:
	//  * Objects we’re reducing over, for which we wish to avoid
	//    excessive database writes.
	//  * An LRU of "hot" objects we expect to reference again soon.
	// However, to guarantee required transactionality properties,
	// consumers must be careful not to mix states between shards. |Cache|
	// is available to consumers for shard-level isolation of a
	// consumer-defined local memory context.
	Cache interface{}

	// Local state database of the shard.
	Database *rocks.DB

	// Current transaction of the consumer shard. All writes to |Database|
	// must be issued through |Transaction|.
	Transaction *rocks.WriteBatch

	// Client to which Publish()'d messages are emitted.
	Writer gazette.JournalWriter

	// Used for flushing by the v2 consumer adapter. Not required by the real
	// implementation, which will drain consumer queues prior to flushing.
	tmpMu sync.Mutex
}

// All messages emitted under this context should be issued via |Publish|.
// Eventually, this method will note the partitions written to under the
// current transaction (for later confirmation), and will also ensure that
// messages are appropriately tagged and sequenced.
func (c *ConsumerContext) Publish(message interface{}, to *TopicDescription) error {
	buffer := publishBufferPool.Get().([]byte)

	err := frame(to, message, &buffer)
	if err == nil {
		_, err = c.Writer.Write(to.RoutedJournal(message), buffer)
	}
	publishBufferPool.Put(buffer)
	return err
}

var publishBufferPool = sync.Pool{
	New: func() interface{} { return make([]byte, 4096) },
}