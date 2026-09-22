// Package kafka publishes committed domain events to a Kafka topic with
// franz-go. Records are keyed by request id so a request's events stay
// ordered within one partition; the producer is idempotent and acks=all.
package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/workflow"
	"github.com/udaykishore-resu/priorauth-engine/internal/ports"
)

// Publisher implements ports.Publisher.
type Publisher struct {
	cl    *kgo.Client
	topic string
}

// NewPublisher creates the client. It does not block on broker
// connectivity; the first Publish surfaces connection errors.
func NewPublisher(brokers []string, topic string) (*Publisher, error) {
	if len(brokers) == 0 || topic == "" {
		return nil, errors.New("kafka: brokers and topic are required")
	}
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.DefaultProduceTopic(topic),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerBatchCompression(kgo.SnappyCompression()),
		kgo.RecordRetries(5),
		kgo.ProduceRequestTimeout(10*time.Second),
		kgo.ClientID("priorauth-engine"),
	)
	if err != nil {
		return nil, fmt.Errorf("kafka: client: %w", err)
	}
	return &Publisher{cl: cl, topic: topic}, nil
}

// Publish implements ports.Publisher. It produces synchronously so the
// caller knows whether delivery happened; ordering per request id is kept
// by the key.
func (p *Publisher) Publish(ctx context.Context, envs []workflow.Envelope) error {
	if len(envs) == 0 {
		return nil
	}
	recs := make([]*kgo.Record, 0, len(envs))
	for _, e := range envs {
		b, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("kafka: marshal %s: %w", e.Type, err)
		}
		recs = append(recs, &kgo.Record{
			Topic: p.topic,
			Key:   []byte(e.RequestID),
			Value: b,
			Headers: []kgo.RecordHeader{
				{Key: "type", Value: []byte(e.Type)},
				{Key: "seq", Value: []byte(fmt.Sprint(e.Seq))},
				{Key: "trace_id", Value: []byte(e.TraceID)},
			},
			Timestamp: e.At,
		})
	}
	res := p.cl.ProduceSync(ctx, recs...)
	if err := res.FirstErr(); err != nil {
		return fmt.Errorf("kafka: produce: %w", err)
	}
	return nil
}

// Close flushes and closes the client.
func (p *Publisher) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := p.cl.Flush(ctx)
	p.cl.Close()
	return err
}

var _ ports.Publisher = (*Publisher)(nil)
