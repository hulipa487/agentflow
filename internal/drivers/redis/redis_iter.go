package redis

import (
	"agentflow/internal/core/memory"
)

type sliceIter struct {
	keys   []string
	idx    int
	handle *Handle
	err    error
}

func (it *sliceIter) Next() bool {
	if it.idx >= len(it.keys) {
		return false
	}
	it.idx++
	return true
}

func (it *sliceIter) Record() memory.Record {
	if it.idx == 0 || it.idx > len(it.keys) {
		return memory.Record{}
	}
	key := it.keys[it.idx-1]
	v, found, err := it.handle.Get("", key)
	if err != nil {
		it.err = err
		return memory.Record{Key: key}
	}
	if !found {
		return memory.Record{Key: key}
	}
	return memory.Record{Key: key, Value: v}
}

func (it *sliceIter) Err() error { return it.err }