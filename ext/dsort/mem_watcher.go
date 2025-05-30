// Package dsort provides APIs for distributed archive file shuffling.
/*
 * Copyright (c) 2018-2025, NVIDIA CORPORATION. All rights reserved.
 */
package dsort

import (
	"sync"
	"time"

	"github.com/NVIDIA/aistore/cmn/atomic"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/oom"
	"github.com/NVIDIA/aistore/ext/dsort/shard"
	"github.com/NVIDIA/aistore/sys"
)

const (
	memoryReservedInterval    = 50 * time.Millisecond
	memoryExcessInterval      = time.Second
	unreserveMemoryBufferSize = 10000
)

type singleMemoryWatcher struct {
	wg     *sync.WaitGroup
	ticker *time.Ticker
	stopCh cos.StopCh
}

// memoryWatcher is responsible for monitoring memory changes and decide
// whether specific action should happen or not. It may also decide to return
type memoryWatcher struct {
	m *Manager

	excess, reserved  *singleMemoryWatcher
	maxMemoryToUse    uint64
	reservedMemory    atomic.Uint64
	memoryUsed        atomic.Uint64 // memory used in specific point in time, it is refreshed once in a while
	unreserveMemoryCh chan uint64
}

func newSingleMemoryWatcher(interval time.Duration) *singleMemoryWatcher {
	smw := &singleMemoryWatcher{wg: &sync.WaitGroup{}, ticker: time.NewTicker(interval)}
	smw.stopCh.Init()
	return smw
}

func newMemoryWatcher(m *Manager, maxMemoryUsage uint64) *memoryWatcher {
	return &memoryWatcher{
		m: m,

		excess:            newSingleMemoryWatcher(memoryExcessInterval),
		reserved:          newSingleMemoryWatcher(memoryReservedInterval),
		maxMemoryToUse:    maxMemoryUsage,
		unreserveMemoryCh: make(chan uint64, unreserveMemoryBufferSize),
	}
}

func (mw *memoryWatcher) watch() error {
	var mem sys.MemStat
	if err := mem.Get(); err != nil {
		return err
	}
	mw.memoryUsed.Store(mem.ActualUsed)

	mw.reserved.wg.Add(1)
	go mw.watchReserved()
	mw.excess.wg.Add(1)
	go mw.watchExcess(mem)
	return nil
}

func (mw *memoryWatcher) watchReserved() {
	defer mw.reserved.wg.Done()

	// Starting memory updater. Since extraction phase is concurrent and we
	// cannot know how much memory will given compressed shard extract we need
	// to employ mechanism for updating memory. Just before extraction we
	// estimate how much memory shard will contain (by multiplying file size and
	// avg compress ratio). Then we update currently used memory to actual used
	// + reserved. After we finish extraction we put reserved memory for the
	// shard into the unreserve memory channel. Note that we cannot unreserve it
	// right away because actual used memory has not yet been updated (but it
	// surely changed). Once memory updater will fetch and update currently used
	// memory in system we can unreserve memory (it is already calculated in
	// newly fetched memory usage value). This way it is almost impossible to
	// exceed maximum memory which we are able to use (set by user) -
	// unfortunately it can happen when we underestimate the amount of memory
	// which we will use when extracting compressed file.
	for {
		select {
		case <-mw.reserved.ticker.C:
			var curMem sys.MemStat
			if err := curMem.Get(); err == nil {
				mw.memoryUsed.Store(curMem.ActualUsed)

				unreserve := true
				for unreserve {
					select {
					case size := <-mw.unreserveMemoryCh:
						mw.reservedMemory.Sub(size)
					default:
						unreserve = false
					}
				}
			}
		case <-mw.m.listenAborted():
			return
		case <-mw.reserved.stopCh.Listen():
			return
		}
	}
}

// Runs in a goroutine, watches allocated memory, and frees SGLs if need be
// ------------------------------------------------------
// We also watch the memory in `watchReserved` but this may be
// insufficient because there is more factors than just `SGL`s: `Records`,
// `Shards`, `RecordContents`, `ExtractionPaths` etc. All these structures
// require memory, sometimes it can be counted in GBs. That is why we also need
// excess watcher so that it prevents memory overuse.
// --------------------------------------------------------
// Because Go's runtime does not immediately return freed memory
// to the system it'd be incorrect to treat `maxMemoryToUse - curMem.ActualUsed`
// as excessively allocated
func (mw *memoryWatcher) watchExcess(memStat sys.MemStat) {
	defer mw.excess.wg.Done()

	buf, slab := g.mem.Alloc()
	defer slab.Free(buf)

	lastMemoryUsage := memStat.ActualUsed
	for {
		select {
		case <-mw.excess.ticker.C:
			var curMem sys.MemStat
			if err := curMem.Get(); err != nil {
				continue
			}

			memExcess := int64(curMem.ActualUsed - lastMemoryUsage)
			lastMemoryUsage = curMem.ActualUsed

			if curMem.ActualUsed < mw.maxMemoryToUse {
				continue
			}

			storeType := shard.DiskStoreType
			if mw.m.shardRW.SupportsOffset() {
				storeType = shard.OffsetStoreType
			}
			mw.m.recm.RecordContents().Range(func(key, value any) bool {
				n := mw.m.recm.FreeMem(key.(string), storeType, value, buf)
				memExcess -= n
				return memExcess > 0 // continue if we need more
			})

			oom.FreeToOS(false /*force*/)
		case <-mw.m.listenAborted():
			return
		case <-mw.excess.stopCh.Listen():
			return
		}
	}
}

func (mw *memoryWatcher) reserveMem(toReserve uint64) (exceeding bool) {
	newReservedMemory := mw.reservedMemory.Add(toReserve)
	// expected total memory after all objects will be extracted is equal
	// to: previously reserved memory + uncompressed size of shard + current memory used
	expectedTotalMemoryUsed := newReservedMemory + mw.memoryUsed.Load()

	exceeding = expectedTotalMemoryUsed >= mw.maxMemoryToUse
	return
}

func (mw *memoryWatcher) unreserveMem(toUnreserve uint64) {
	mw.unreserveMemoryCh <- toUnreserve
}

func (mw *memoryWatcher) stopWatchingExcess() {
	mw.excess.ticker.Stop()
	mw.excess.stopCh.Close()
	mw.excess.wg.Wait()
}

func (mw *memoryWatcher) stopWatchingReserved() {
	mw.reserved.ticker.Stop()
	mw.reserved.stopCh.Close()
	mw.reserved.wg.Wait()
}

func (mw *memoryWatcher) stop() {
	mw.stopWatchingExcess()
	mw.stopWatchingReserved()
	close(mw.unreserveMemoryCh)
}

type inmemShardAllocator struct {
	mtx  *sync.Mutex
	cond *sync.Cond

	maxAllocated uint64
	allocated    uint64
}

func newInmemShardAllocator(maxAllocated uint64) *inmemShardAllocator {
	x := &inmemShardAllocator{
		mtx:          &sync.Mutex{},
		maxAllocated: maxAllocated,
	}
	x.cond = sync.NewCond(x.mtx)
	return x
}

func (sa *inmemShardAllocator) alloc(size uint64) {
	sa.mtx.Lock()

	for sa.freeMem() < size {
		sa.cond.Wait()
	}

	sa.allocated += size
	sa.mtx.Unlock()
}

func (sa *inmemShardAllocator) free(size uint64) {
	sa.mtx.Lock()
	sa.allocated -= size
	sa.cond.Signal()
	sa.mtx.Unlock()
}

func (sa *inmemShardAllocator) freeMem() uint64 {
	return sa.maxAllocated - sa.allocated
}
