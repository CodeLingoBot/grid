package grid

import (
	"encoding/json"
	"flag"
	"io"
	"testing"
	"time"

	"github.com/Shopify/sarama"
)

const (
	ClientName = "grid-test-client"
	TopicName  = "grid-test-topic"
)

var tag = flag.String("tag", "", "integration")

func integrationEnabled() bool {
	return tag != nil && *tag == "integration"
}

func TestWriter(t *testing.T) {
	if !integrationEnabled() {
		return
	}

	client, err := sarama.NewClient(ClientName, []string{"localhost:10092"}, sarama.NewClientConfig())
	if err != nil {
		t.Fatalf("failed to create kafka client: %v", err)
	}

	in := make(chan Event, 0)
	startTopicWriter(TopicName, client, newTestMesgEncoder, in)

	for i := 0; i < 10; i++ {
		in <- NewWritable(TopicName, "", NewTestMesg(i))
	}
}

func TestReader(t *testing.T) {
	if !integrationEnabled() {
		return
	}

	brokers := []string{"localhost:10092"}

	pconfig := sarama.NewProducerConfig()
	cconfig := sarama.NewConsumerConfig()
	cconfig.OffsetMethod = sarama.OffsetMethodNewest

	kafkaClientConfig := &KafkaConfig{
		Brokers:        brokers,
		BaseName:       "test-client",
		ClientConfig:   sarama.NewClientConfig(),
		ProducerConfig: pconfig,
		ConsumerConfig: cconfig,
	}

	client, err := sarama.NewClient(kafkaClientConfig.BaseName, kafkaClientConfig.Brokers, kafkaClientConfig.ClientConfig)
	if err != nil {
		t.Fatalf("failed to create kafka client: %v", err)
	}

	in := make(chan Event, 0)
	startTopicWriter(TopicName, client, newTestMesgEncoder, in)

	parts, err := client.Partitions(TopicName)
	if err != nil {
		t.Fatalf("error: topic: %v: failed getting kafka partition data: %v", TopicName, err)
	}

	go func() {
		cnt := 0
		for e := range startTopicReader(TopicName, kafkaClientConfig, newTestMesgDecoder, parts) {
			switch msg := e.Message().(type) {
			case *TestMesg:
				// fmt.Printf("rx: %v\n", msg)
				if msg.Data != cnt {
					t.Fatalf("expected message #%d to equal %d, but was: %d", cnt, cnt, msg.Data)
				}
			default:
				t.Fatalf("unknown message type received on: %v: %T :: %v", TopicName, msg, msg)
			}
			cnt++
		}
	}()

	time.Sleep(2 * time.Second)

	for i := 0; i < 10; i++ {
		in <- NewWritable(TopicName, "", NewTestMesg(i))
	}

	time.Sleep(3 * time.Second)
}

type TestMesg struct {
	Data int
}

func NewTestMesg(i int) *TestMesg {
	return &TestMesg{i}
}

type testcoder struct {
	*json.Encoder
	*json.Decoder
}

func (c *testcoder) New() interface{} {
	return &TestMesg{}
}

func newTestMesgDecoder(r io.Reader) Decoder {
	return &testcoder{nil, json.NewDecoder(r)}
}

func newTestMesgEncoder(w io.Writer) Encoder {
	return &testcoder{json.NewEncoder(w), nil}
}