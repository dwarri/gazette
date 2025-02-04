package main

import (
	"context"
	"io"
	"io/ioutil"
	"os"

	"github.com/jessevdk/go-flags"
	log "github.com/sirupsen/logrus"
	pb "go.gazette.dev/core/broker/protocol"
	pc "go.gazette.dev/core/consumer/protocol"
	"go.gazette.dev/core/examples/word-count"
	mbp "go.gazette.dev/core/mainboilerplate"
)

var (
	Config = new(struct {
		WordCount mbp.AddressConfig `group:"Consumer" namespace:"consumer" env-namespace:"CONSUMER"`
		Log       mbp.LogConfig     `group:"Logging" namespace:"log" env-namespace:"LOG"`
	})
)

type cmdPublish struct {
	File string `long:"file" description:"Input file to read. Use - for stdin."`
}

func (cmd *cmdPublish) Execute([]string) (err error) {
	var b []byte
	var fin io.ReadCloser

	if cmd.File == "-" {
		fin = ioutil.NopCloser(os.Stdin)
	} else if fin, err = os.Open(cmd.File); err != nil {
		return err
	}
	if b, err = ioutil.ReadAll(fin); err != nil {
		return err
	}
	var ctx = pb.WithDispatchDefault(context.Background())
	var client = word_count.NewNGramClient(Config.WordCount.MustDial(ctx))
	_, err = client.Publish(ctx, &word_count.PublishRequest{Text: string(b)})

	return err
}

type cmdQuery struct {
	Prefix  string `long:"prefix" description:"NGram prefix to query."`
	ShardID string `long:"shard" description:"(Optional) Shard ID to which query is directed."`
}

func (cmd *cmdQuery) Execute([]string) error {
	var ctx = pb.WithDispatchDefault(context.Background())
	var client = word_count.NewNGramClient(Config.WordCount.MustDial(ctx))

	var resp, err = client.Query(ctx, &word_count.QueryRequest{
		Prefix: word_count.NGram(cmd.Prefix),
		Shard:  pc.ShardID(cmd.ShardID),
	})
	if err != nil {
		return err
	}
	for _, r := range resp.Grams {
		log.WithFields(log.Fields{
			"gram":  r.NGram,
			"count": r.Count,
		}).Info("gram")
	}
	return nil
}

func main() {
	var err error
	var parser = flags.NewParser(Config, flags.Default)

	_, err = parser.AddCommand("publish", "Publish NGram text",
		"Publish text to include in the NGram model", &cmdPublish{})
	mbp.Must(err, "failed to add publish command")

	_, err = parser.AddCommand("query", "Query NGrams",
		"Query an NGram or prefix thereof", &cmdQuery{})
	mbp.Must(err, "failed to add query command")

	pb.RegisterGRPCDispatcher("local")
	mbp.MustParseArgs(parser)
}
