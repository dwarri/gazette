// Package message is a client-only library which implements exactly-once message
// semantics atop raw, at-least-once Journal byte-streams.
//
// It specifies a common Message interface type which must be implemented by
// consumer Applications, and a RFC 4122 v1 UUID type which enables de-duplication
// and atomic commits of multiple messages.
//
// MappingKeyFunc extracts a stable mapping identifier from a custom message type.
// To map messages on a session ID:
//
//      var mapOnSessionFn MappingKeyFunc = func(m Mappable, w io.Writer) {
//          w.Write([]byte(m.(*MyMsgType).SessionID))
//      }
//
// MappingFunc then defines the means of mapping messages to a journal. Several
// routines, like ModuloMapping, help in the construction of MappingFuncs and
// can be used to implement "data shuffles" which stably map messages having a
// shared mapping key to a common journal.
//
// Combine with client.PolledList to build MappingFuncs that publish to a dynamic,
// automatically updating "topic" of selected journal partitions:
//
//      var myClient pb.AsyncJournalClient = ...
//
//      var partitions, _ = pb.ParseLabelSelector("logs=pageviews, source=mobile")
//      var pl, _ = client.NewPolledList(ctx, myClient, time.Minute, pb.ListRequest{
//          Selector: partitions,
//      })
//      // Use RendezvousMapping to minimally shuffle the mapping of
//      // SessionID <=> journal when the topic partitioning is updated.
//      var mapFn = RendezvousMapping(mapOnSessionFn, pl.List)
//
// Then, use a Publisher to publish messages:
//
//      var pub = NewPublisher(myClient, nil)
//      for _, msg := range messages {
//          // Each message is mapped on its SessionID to a current topic
//          // partition (ie, journal), sequenced with a UUID, marshalled,
//          // and queued for appended to the mapped journal.
//          pub.PublishCommitted(mapFn, msg)
//      }
//      for op := myClient.PendingExcept("") {
//          <-op.Done() // Wait for all async appends to complete.
//      }
//
// When reading, NewMessageFunc provides the package with a means of constructing
// new messages of the users's type.
//
//      var newMsgFn NewMessageFunc = func(*pb.JournalSpec) (Message, error) {
//          return new(MyMsgType), nil
//      }
//
// ReadUncommittedIter reads "uncommitted" messages from a journal. Uncommitted
// messages may include duplicates, or messages which are never acknowledged or
// are later explicitly rolled back.
//
//      var rr = client.NewRetryReader(ctx, rjc, pb.ReadRequest{
//          Journal:    "my/journal",
//          Block:      true,
//      })
//      var it = NewReadUncommittedIter(rr, newMsgFn)
//      for {
//          var env, err = it.Next()
//
//          // Handle |env| and |err|.
//      }
//
// Use a Sequencer to sequence read-uncommitted messages into read-committed ones,
// and a ReadCommittedIter to read only committed messages from the journal.
// ReadCommittedIter is nothing more than the composition of a ReadUncommittedIter
// with a Sequencer.
//
//      var seq = NewSequencer(nil, 4096)
//      var it = NewReadCommittedIter(rr, newMsgFn, seq)
//      for {
//          var env, err = it.Next()
//
//          // Handle |env| and |err|. We're assured the message has been
//          // acknowledged and is not a duplicate.
//      }
//
package message

import (
	"bufio"
	"fmt"
	"io"

	pb "go.gazette.dev/core/broker/protocol"
)

// Message is an arbitrary user-defined type which may be read from and written
// to a journal. Journals themselves hold only raw message serializations.
// Gazette therefore asks that Messages help with representation by taking,
// persisting, and when asked, returning UUIDs generated by Gazette. UUIDs may
// also be directly useful to users, as they're universally unique and
// they encode a precise publishing timestamp.
//
// In some cases, user types may be unable to represent a UUID. The interface
// can be implemented with no-ops to opt the type out of exactly-once processing,
// falling back to at-least-once semantics.
type Message interface {
	// GetUUID returns the UUID previously set on the Message. If the Message
	// is not capable of tracking UUIDs, GetUUID returns a zero-valued UUID
	// to opt the Message out of exactly-once processing semantics. In this
	// case, SetUUID is also a no-op.
	GetUUID() UUID
	// SetUUID sets the UUID of the Message.
	SetUUID(UUID)
	// NewAcknowledgement returns a new Message instance of this same type which
	// will represent an acknowledgement of this (and future) Messages published
	// to the Journal within the context of a transaction.
	NewAcknowledgement(pb.Journal) Message
}

// Validator is an optional interface of a Message able to Validate itself.
// An attempt to publish a Message which does not Validate will error.
type Validator = pb.Validator

// Envelope wraps a Message with associated metadata.
type Envelope struct {
	Journal    *pb.JournalSpec // JournalSpec of the Message.
	Begin, End pb.Offset       // [Begin, End) byte offset of the Message within the Journal.
	Message                    // Wrapped message.
}

// JournalProducer composes an Journal and ProducerID.
type JournalProducer struct {
	Journal  pb.Journal
	Producer ProducerID
}

// ProducerState is a snapshot of a Producer's state within a Journal.
// It's marshalled into consumer checkpoints to allow a Sequencer to
// recover producer sequence states after a consumer process fault.
type ProducerState struct {
	JournalProducer
	// LastAck is the Clock of the Producer's last ACK_TXN or OUTSIDE_TXN.
	LastAck Clock
	// Begin is the offset of the first message byte having CONTINUE_TXN that's
	// larger than LastAck. Eg, it's the offset which opens the next transaction.
	// If there is no such message, Begin is -1.
	Begin pb.Offset
}

// Frameable is an interface suitable for serialization by a Framing.
// The interface requirements of a Frameable are specific to the Framing
// used, and asserted at run-time. Generally a Frameable is a Message
// but the Framing interface doesn't require this.
type Frameable interface{}

// Framing specifies the serialization used to encode Messages within a journal.
type Framing interface {
	// ContentType of the Framing.
	ContentType() string
	// Marshal a Message to a bufio.Writer. Marshal may assume the Message has
	// passed validation, if implemented for the message type. It may ignore
	// any error returned by the provided Writer.
	Marshal(Frameable, *bufio.Writer) error
	// Unpack reads and returns a complete framed message from the Reader,
	// including any applicable message header or suffix. It returns an error of
	// the underlying Reader, or of a framing corruption. The returned []byte may
	// be invalidated by a subsequent use of the Reader or another Unpack call.
	Unpack(*bufio.Reader) ([]byte, error)
	// Unmarshal a Frameable message from the supplied frame previously produced
	// by Unpack. It returns only message-level decoding errors, which do not
	// invalidate the Framing or the Reader (eg, further frames may be unpacked).
	Unmarshal([]byte, Frameable) error
}

// Mappable is an interface suitable for mapping by a MappingFunc.
// Typically a MappingKeyFunc will cast and assert Mappable's exact
// type at run-time. Generally a Mappable is a Message but the
// MappingFunc interface doesn't require this.
type Mappable interface{}

// MappingFunc maps a Mappable message to a responsible journal. Gazette imposes
// no formal requirement on exactly how that mapping is performed, or the nature
// of the mapped journal.
//
// It's often desired to spread a collection of like messages across a number
// of journals, with each journal playing the role of a topic partition. Such
// partitions can be distinguished through a JournalSpec Label such as
// "app.gazette.dev/message-type: MyMessage". Note that "partition" and "topic"
// are useful terminology, but play no formal role and have no explicit
// implementation within Gazette (aside from their expression via Labels and
// LabelSelectors). See `labels` package documentation for naming conventions.
//
// A Mapper implementation would typically:
//  1) Apply domain knowledge to introspect the Mappable and determine a "topic",
//     expressed as a LabelSelector.
//  2) Query the broker List RPC to determine current partitions of the topic,
//     caching and refreshing List results as needed (see client.PolledList).
//  3) Use a ModuloMapping or RendezvousMapping to select among partitions.
type MappingFunc func(Mappable) (pb.Journal, Framing, error)

// MappingKeyFunc extracts an appropriate mapping key from the Mappable
// by writing its value into the provided io.Writer, whose Write() is
// guaranteed to never return an error.
type MappingKeyFunc func(Mappable, io.Writer)

// PartitionsFunc returns a ListResponse of journal partitions from which a
// MappingFunc may select. The returned instance pointer may change across
// invocations, but a returned ListResponse may not be modified. PartitionsFunc
// should seek to preserve pointer equality of result instances when no
// substantive change has occurred. See also: client.PolledList.
type PartitionsFunc func() *pb.ListResponse

// NewMessageFunc returns a Message instance of an appropriate type for the
// reading the given JournalSpec. Implementations may want to introspect the
// JournalSpec, for example by examining application-specific labels therein.
// An error is returned if an appropriate Message type cannot be determined.
type NewMessageFunc func(*pb.JournalSpec) (Message, error)

// ErrEmptyListResponse is returned by a MappingFunc which received an empty
// ListResponse from a PartitionsFunc.
var ErrEmptyListResponse = fmt.Errorf("empty ListResponse")
