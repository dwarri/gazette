package consumer

import (
	"go.etcd.io/etcd/mvcc/mvccpb"
	"go.gazette.dev/core/allocator"
	pb "go.gazette.dev/core/broker/protocol"
	pc "go.gazette.dev/core/consumer/protocol"
	"go.gazette.dev/core/keyspace"
)

// NewKeySpace returns a KeySpace suitable for use with an Allocator.
// It decodes allocator Items as ShardSpec messages, Members as ConsumerSpecs,
// and Assignments as RecoveryStatus enums.
func NewKeySpace(prefix string) *keyspace.KeySpace {
	return allocator.NewAllocatorKeySpace(prefix, decoder{})
}

// decoder is an instance of allocator.Decoder. Much like broker.decoder, it
// strictly enforces that ShardSpec.Id matches the Id implied by the Spec's Etcd
// key. Conceptually, the Etcd key is source-of-truth regarding entity naming,
// but it's helpful to carry these identifiers within the specs themselves,
// making them a stand-alone representation and allowing for content-addressing
// of a spec into its appropriate Etcd key. This decoder behavior provides an
// assertion that these identifiers never diverge.
type decoder struct{}

func (d decoder) DecodeItem(id string, raw *mvccpb.KeyValue) (allocator.ItemValue, error) {
	var s = new(pc.ShardSpec)

	if err := s.Unmarshal(raw.Value); err != nil {
		return nil, err
	} else if err = s.Validate(); err != nil {
		return nil, err
	} else if s.Id.String() != id {
		return nil, pb.NewValidationError("ShardSpec ID doesn't match Item ID (%+v vs %+v)", s.Id, id)
	}
	return s, nil
}

func (d decoder) DecodeMember(zone, suffix string, raw *mvccpb.KeyValue) (allocator.MemberValue, error) {
	var s = new(pc.ConsumerSpec)

	if err := s.Unmarshal(raw.Value); err != nil {
		return nil, err
	} else if err = s.Validate(); err != nil {
		return nil, err
	} else if s.Id.Zone != zone {
		return nil, pb.NewValidationError("ConsumerSpec Zone doesn't match Member Zone (%+v vs %+v)", s.Id.Zone, zone)
	} else if s.Id.Suffix != suffix {
		return nil, pb.NewValidationError("ConsumerSpec Suffix doesn't match Member Suffix (%+v vs %+v)", s.Id.Suffix, suffix)
	}
	return s, nil
}

func (d decoder) DecodeAssignment(itemID, memberZone, memberSuffix string, slot int, raw *mvccpb.KeyValue) (allocator.AssignmentValue, error) {
	var s = new(pc.ReplicaStatus)

	if err := s.Unmarshal(raw.Value); err != nil {
		return nil, err
	} else if err = s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}
