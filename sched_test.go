package grid

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

func TestPeerSched(t *testing.T) {

	kafkaparts := make(map[string][]int32)
	kafkaparts["topic1"] = []int32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	kafkaparts["topic2"] = []int32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}

	topics := make(map[string]bool)
	topics["topic1"] = true
	topics["topic2"] = true

	ops := make(map[string]*op)
	op1 := &op{n: 11, inputs: topics}
	ops["f1"] = op1
	op2 := &op{n: 7, inputs: topics}
	ops["f2"] = op2

	peers := make(map[string]*Peer)
	peers["host1-123-0"] = newPeer("host1-123-0", Follower, Active, time.Now().Unix())
	peers["host1-345-0"] = newPeer("host1-345-0", Follower, Active, time.Now().Unix())
	peers["host1-678-0"] = newPeer("host1-678-0", Follower, Active, time.Now().Unix())

	// Expected result is that between all instances of 'f1', the topics
	// 'topic1' and 'topic2' are consumed in full, ie: that all their
	// partitions are being read by one instance of 'f1' or another.
	// The same expectation holds for instances of 'f2'.
	sched := peersched(peers, ops, kafkaparts)

	// This deep map structure reflects the full list of partitions
	// that are expected to exist under each function/topic pair.
	expected_parts := make(map[string]map[string]map[int32]bool)
	expected_parts["f1"] = make(map[string]map[int32]bool)
	expected_parts["f2"] = make(map[string]map[int32]bool)
	expected_parts["f1"]["topic1"] = make(map[int32]bool)
	expected_parts["f1"]["topic2"] = make(map[int32]bool)
	expected_parts["f2"]["topic1"] = make(map[int32]bool)
	expected_parts["f2"]["topic2"] = make(map[int32]bool)

	for p := int32(0); p < 12; p++ {
		expected_parts["f1"]["topic1"][p] = true
	}

	for p := int32(0); p < 15; p++ {
		expected_parts["f1"]["topic2"][p] = true
	}

	for p := int32(0); p < 12; p++ {
		expected_parts["f2"]["topic1"][p] = true
	}

	for p := int32(0); p < 15; p++ {
		expected_parts["f2"]["topic2"][p] = true
	}

	// Now elements are deleted from the expected parts, if each expected
	// part was indeed assigned to some instance of a function it is
	// used as an index into the expected_parts mapping for deletion.
	for host, _ := range peers {
		finsts, found := sched.FunctionInstances(host)
		if !found {
			t.Fatalf("failed to find function instances for host: %v", host)
		}
		for _, fi := range finsts {
			for topic, _ := range topics {
				for _, part := range fi.topicslices[topic].parts {
					delete(expected_parts[fi.fname][topic], part)
				}
			}
		}
	}

	for host, _ := range peers {
		finsts, found := sched.FunctionInstances(host)
		if !found {
			t.Fatalf("failed to find function instances for host: %v", host)
		}
		for _, fi := range finsts {
			for topic, _ := range topics {
				if 0 != len(expected_parts[fi.fname][topic]) {
					t.Fatalf("some partitions were not scheduled for reading: %v %v %v: %v", host, fi.fname, topic, partsstr(expected_parts[fi.fname][topic]))
				}
			}
		}
	}
}

func partsstr(parts map[int32]bool) string {
	var buf bytes.Buffer
	for part, _ := range parts {
		buf.Write([]byte(fmt.Sprintf("%v ", part)))
	}

	return buf.String()
}
