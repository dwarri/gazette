package consumer

import (
	"context"
	"errors"

	gc "github.com/go-check/check"
	pc "go.gazette.dev/core/consumer/protocol"
)

type ReplicaSuite struct{}

func (s *ReplicaSuite) TestStandbyToPrimaryTransitions(c *gc.C) {
	var tf, cleanup = newTestFixture(c)
	defer cleanup()

	// Begin as a standby replica of the shard.
	tf.allocateShard(c, makeShard(shardA), remoteID, localID)

	// Expect that status transitions to BACKFILL, then to TAILING.
	expectStatusCode(c, tf.state, pc.ReplicaStatus_TAILING)

	// Re-assign as shard primary.
	tf.allocateShard(c, makeShard(shardA), localID)

	// Expect that status now transitions to PRIMARY.
	expectStatusCode(c, tf.state, pc.ReplicaStatus_PRIMARY)

	// Verify message pump and consumer loops were started.
	var res, err = tf.resolver.Resolve(ResolveArgs{Context: tf.ctx, ShardID: shardA})
	c.Check(err, gc.IsNil)
	defer res.Done()

	runSomeTransactions(c, res.Shard)

	tf.allocateShard(c, makeShard(shardA)) // Cleanup.
}

func (s *ReplicaSuite) TestDirectToPrimaryTransition(c *gc.C) {
	var tf, cleanup = newTestFixture(c)
	defer cleanup()

	tf.allocateShard(c, makeShard(shardA), localID)

	// Expect that status transitions to PRIMARY.
	expectStatusCode(c, tf.state, pc.ReplicaStatus_PRIMARY)

	// Verify message pump and consumer loops were started.
	var res, err = tf.resolver.Resolve(ResolveArgs{Context: tf.ctx, ShardID: shardA})
	c.Check(err, gc.IsNil)
	defer res.Done()

	runSomeTransactions(c, res.Shard)

	tf.allocateShard(c, makeShard(shardA)) // Cleanup.
}

func (s *ReplicaSuite) TestPlayRecoveryLogError(c *gc.C) {
	var tf, cleanup = newTestFixture(c)
	defer cleanup()

	var shard = makeShard(shardA)
	shard.RecoveryLogPrefix = "does/not/exist"
	tf.allocateShard(c, shard, remoteID, localID)

	// Expect that status transitions to FAILED, with a descriptive error.
	c.Check(expectStatusCode(c, tf.state, pc.ReplicaStatus_FAILED).Errors[0],
		gc.Matches, `playLog: fetching JournalSpec: named journal does not exist \(does/not/exist/`+shardA+`\)`)

	tf.allocateShard(c, makeShard(shardA)) // Cleanup.
}

func (s *ReplicaSuite) TestCompletePlaybackError(c *gc.C) {
	var tf, cleanup = newTestFixture(c)
	defer cleanup()

	tf.app.newStoreErr = errors.New("an error") // Cause NewStore to fail.
	tf.allocateShard(c, makeShard(shardA), localID)

	c.Check(expectStatusCode(c, tf.state, pc.ReplicaStatus_FAILED).Errors[0],
		gc.Matches, `completePlayback: initializing store: an error`)

	tf.allocateShard(c, makeShard(shardA)) // Cleanup.
}

func (s *ReplicaSuite) TestPumpMessagesError(c *gc.C) {
	var tf, cleanup = newTestFixture(c)
	defer cleanup()

	var shard = makeShard(shardA)
	shard.Sources[1].Journal = "xxx/does/not/exist"
	tf.allocateShard(c, shard, localID)

	// Expect that status transitions to FAILED, with a descriptive error.
	c.Check(expectStatusCode(c, tf.state, pc.ReplicaStatus_FAILED).Errors[0],
		gc.Matches, `pumpMessages: fetching JournalSpec: named journal does not exist \(xxx/does/not/exist\)`)

	tf.allocateShard(c, makeShard(shardA)) // Cleanup.
}

func (s *ReplicaSuite) TestConsumeMessagesErrors(c *gc.C) {
	var tf, cleanup = newTestFixture(c)
	defer cleanup()

	var cases = []struct {
		fn     func()
		expect string
	}{
		// Case: Consume() fails.
		{
			func() { tf.app.consumeErr = errors.New("an error") },
			`consumeMessages: txnStep: app.ConsumeMessage: an error`,
		},
		// Case: FinishTxn() fails.
		{
			func() { tf.app.finishErr = errors.New("an error") },
			`consumeMessages: FinishTxn: an error`,
		},
		// Case: Both fail. Consume()'s error dominates.
		{
			func() {
				tf.app.consumeErr = errors.New("an error")
				tf.app.finishErr = errors.New("shadowed error")
			},
			`consumeMessages: txnStep: app.ConsumeMessage: an error`,
		},
	}
	for _, tc := range cases {
		tf.app.consumeErr, tf.app.finishErr = nil, nil // Reset fixture.

		tf.allocateShard(c, makeShard(shardA), localID)
		expectStatusCode(c, tf.state, pc.ReplicaStatus_PRIMARY)

		var res, err = tf.resolver.Resolve(ResolveArgs{Context: context.Background(), ShardID: shardA})
		c.Assert(err, gc.IsNil)

		runSomeTransactions(c, res.Shard)

		// Set failure fixture, and write a message to trigger it.
		tc.fn()

		var aa = res.Shard.JournalClient().StartAppend(sourceB)
		_, _ = aa.Writer().WriteString(`{"key":"foo"}` + "\n")
		c.Check(aa.Release(), gc.IsNil)

		c.Check(expectStatusCode(c, tf.state, pc.ReplicaStatus_FAILED).Errors[0], gc.Matches, tc.expect)

		// Cleanup.
		res.Done()
		tf.allocateShard(c, makeShard(shardA))
	}
}

var _ = gc.Suite(&ReplicaSuite{})
