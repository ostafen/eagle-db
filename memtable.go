package eagle

import (
	"bytes"
	"sync"

	"github.com/ostafen/eagle/util"
	"github.com/spaolacci/murmur3"
)

type node struct {
	seqNumber uint64
	key       []byte
	next      *node
	ptr       *ValuePointer
}

type tablePartition struct {
	resizeInProgress bool

	nextResizeIndex int
	buckets         [][]*node
	nElements       util.AtomicInt32
	nNodes          util.AtomicInt32
}

type memTable struct {
	locks      []sync.RWMutex
	partitions []*tablePartition
}

const (
	initialBucketSize = 4
	numPartitions     = 16
)

func newMemTable() *memTable {
	partitions := make([]*tablePartition, numPartitions)
	for i := 0; i < numPartitions; i++ {
		partitions[i] = newTablePartition()
	}

	return &memTable{
		partitions: partitions,
		locks:      make([]sync.RWMutex, numPartitions),
	}
}

func (t *memTable) Get(key []byte) (*ValuePointer, uint64) {
	hash := hashKey(key)

	p := hash >> 28
	t.locks[p].RLock()
	defer t.locks[p].RUnlock()

	return t.partitions[p].get(key, hash)
}

func (t *memTable) Remove(key []byte, seqNumber uint64) *ValuePointer {
	hash := hashKey(key)

	p := hash >> 28

	t.locks[p].Lock()
	defer t.locks[p].Unlock()

	return t.partitions[p].remove(key, seqNumber, hash)
}

func (t *memTable) Put(key []byte, seqNumber uint64, ptr *ValuePointer) (*ValuePointer, bool) {
	hash := hashKey(key)

	p := hash >> 28

	t.locks[p].Lock()
	defer t.locks[p].Unlock()

	return t.partitions[p].put(key, seqNumber, ptr, hash)
}

func (t *memTable) ContainsKey(key []byte) bool {
	hash := hashKey(key)

	p := hash >> 28

	t.locks[p].RLock()
	defer t.locks[p].RUnlock()

	return t.partitions[p].containsKey(key, hash)
}

func newTablePartition() *tablePartition {
	buckets := make([][]*node, 2)
	buckets[0] = make([]*node, initialBucketSize)

	return &tablePartition{
		buckets:          buckets,
		resizeInProgress: false,
		nextResizeIndex:  -1,
	}
}

func (t *memTable) Size() int {
	n := 0
	for i := 0; i < len(t.partitions); i++ {
		n += int(t.partitions[i].nElements.Get())
	}
	return n
}

func (t *tablePartition) findNode(key []byte, hash uint32) (int, *node, *node) {
	for i := 0; i <= 1; i++ {
		buckets := t.buckets[i]

		if buckets != nil {
			var prevNode *node = nil
			for node := buckets[hash%uint32(len(buckets))]; node != nil; prevNode, node = node, node.next {
				if bytes.Equal(node.key, key) {
					return i, prevNode, node
				}
			}
		}
	}
	return -1, nil, nil
}

func hashKey(key []byte) uint32 {
	hash := murmur3.New32()
	hash.Write(key)
	return hash.Sum32()
}

func (t *tablePartition) completeResize() {
	t.buckets[0] = t.buckets[1]
	t.buckets[1] = nil

	t.resizeInProgress = false
	t.nextResizeIndex = -1
}

func (t *tablePartition) get(key []byte, hash uint32) (*ValuePointer, uint64) {
	_, _, nd := t.findNode(key, hash)
	if nd != nil {
		return nd.ptr, nd.seqNumber
	}
	return nil, 0
}

// replace with Swap(key, value, func(oldValue, newValue) bool)
func (t *tablePartition) put(key []byte, seqNumber uint64, ptr *ValuePointer, hash uint32) (*ValuePointer, bool) {
	t.resizeStep()

	bucketIndex, prevNode, currNode := t.findNode(key, hash)

	if currNode == nil {
		currNode = &node{key: key}

		if t.resizeInProgress {
			bucketHash := hash % uint32(len(t.buckets[1]))
			currNode.next = t.buckets[1][bucketHash]
			t.buckets[1][bucketHash] = currNode
		} else {
			bucketHash := hash % uint32(len(t.buckets[0]))
			currNode.next = t.buckets[0][bucketHash]
			t.buckets[0][bucketHash] = currNode
		}

		t.nElements.Inc()
		t.nNodes.Inc()
	}

	if seqNumber >= currNode.seqNumber {
		prev := currNode.ptr

		if ptr == nil { // unlink if value is nil
			if prevNode != nil {
				prevNode.next = currNode.next
			} else {
				bucketHash := hash % uint32(len(t.buckets[bucketIndex]))
				t.buckets[bucketIndex][bucketHash] = currNode.next
			}
			t.nNodes.Add(-1)
		} else {
			currNode.seqNumber = seqNumber
			currNode.ptr = ptr
		}

		t.resizeIfNeeded()

		return prev, true
	}

	t.resizeIfNeeded()
	return ptr, false
}

func (t *tablePartition) resizeStep() {
	if t.resizeInProgress {

		t.nextResizeIndex++

		for nd := t.buckets[0][t.nextResizeIndex]; nd != nil; {
			newHash := hashKey(nd.key) % uint32(len(t.buckets[1]))

			nextNode := nd.next
			nd.next = t.buckets[1][newHash]
			t.buckets[1][newHash] = nd
			nd = nextNode
		}

		t.buckets[0][t.nextResizeIndex] = nil

		if t.nextResizeIndex == len(t.buckets[0])-1 {
			t.completeResize()
			return
		}
	}
}

func (t *tablePartition) resizeIfNeeded() {
	if t.resizeInProgress {
		return
	}

	nNodes := t.nNodes.Get()
	nBuckets := int32(len(t.buckets[0]))
	if nNodes > 5*nBuckets {
		t.buckets[1] = make([]*node, 2*nBuckets)
		t.resizeInProgress = true
	} else if int(nNodes) < int(nBuckets)/4 && nNodes > initialBucketSize {
		t.buckets[1] = make([]*node, nBuckets/2)
		t.resizeInProgress = true
	}

}

func (t *tablePartition) containsKey(key []byte, hash uint32) bool {
	ptr, _ := t.get(key, hash)
	return ptr != nil
}

func (t *tablePartition) remove(key []byte, seqNum uint64, hash uint32) *ValuePointer {
	t.resizeStep()

	_, _, nd := t.findNode(key, hash)
	if nd == nil {
		return nil
	}

	if seqNum >= nd.seqNumber {
		removePtr := nd.ptr

		nd.ptr = nil

		if removePtr != nil {
			t.nElements.Add(-1)
		}
		return removePtr
	}

	return nil
}
