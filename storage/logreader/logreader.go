// Copyright JAMF Software, LLC

package logreader

import (
	"context"
	"sync"

	serrors "github.com/jamf/regatta/storage/errors"
	"github.com/jamf/regatta/util"
	"github.com/lni/dragonboat/v4"
	"github.com/lni/dragonboat/v4/raftio"
	"github.com/lni/dragonboat/v4/raftpb"
)

type Interface interface {
	raftio.ISystemEventListener
	// QueryRaftLog for all the entries in a given cluster within the right half-open range
	// defined by dragonboat.LogRange. MaxSize denotes the maximum cumulative size of the entries,
	// but this serves only as a hint and the actual size of returned entries may be larger than maxSize.
	QueryRaftLog(context.Context, uint64, dragonboat.LogRange, uint64) ([]raftpb.Entry, error)
}

type logQuerier interface {
	GetLogReader(shardID uint64) (dragonboat.ReadonlyLogReader, error)
}

type shard struct {
	*cache
	mtx sync.Mutex
}

type unimplementedLogReader struct{}

func (u unimplementedLogReader) NodeHostShuttingDown()                       {}
func (u unimplementedLogReader) NodeUnloaded(raftio.NodeInfo)                {}
func (u unimplementedLogReader) NodeDeleted(raftio.NodeInfo)                 {}
func (u unimplementedLogReader) NodeReady(raftio.NodeInfo)                   {}
func (u unimplementedLogReader) MembershipChanged(raftio.NodeInfo)           {}
func (u unimplementedLogReader) ConnectionEstablished(raftio.ConnectionInfo) {}
func (u unimplementedLogReader) ConnectionFailed(raftio.ConnectionInfo)      {}
func (u unimplementedLogReader) SendSnapshotStarted(raftio.SnapshotInfo)     {}
func (u unimplementedLogReader) SendSnapshotCompleted(raftio.SnapshotInfo)   {}
func (u unimplementedLogReader) SendSnapshotAborted(raftio.SnapshotInfo)     {}
func (u unimplementedLogReader) SnapshotReceived(raftio.SnapshotInfo)        {}
func (u unimplementedLogReader) SnapshotRecovered(raftio.SnapshotInfo)       {}
func (u unimplementedLogReader) SnapshotCreated(raftio.SnapshotInfo)         {}
func (u unimplementedLogReader) SnapshotCompacted(raftio.SnapshotInfo)       {}
func (u unimplementedLogReader) LogCompacted(raftio.EntryInfo)               {}
func (u unimplementedLogReader) LogDBCompacted(raftio.EntryInfo)             {}
func (u unimplementedLogReader) QueryRaftLog(context.Context, uint64, dragonboat.LogRange, uint64) ([]raftpb.Entry, error) {
	panic("unimplemented")
}

type Simple struct {
	unimplementedLogReader
	LogQuerier logQuerier
}

func (l *Simple) QueryRaftLog(ctx context.Context, clusterID uint64, logRange dragonboat.LogRange, maxSize uint64) ([]raftpb.Entry, error) {
	// Empty log range should return immediately.
	if logRange.FirstIndex == logRange.LastIndex {
		return nil, nil
	}
	return readLog(l.LogQuerier, clusterID, logRange, maxSize)
}

type Cached struct {
	unimplementedLogReader
	ShardCacheSize int
	LogQuerier     logQuerier
	shardCache     util.SyncMap[uint64, *shard]
}

func (l *Cached) QueryRaftLog(ctx context.Context, clusterID uint64, logRange dragonboat.LogRange, maxSize uint64) ([]raftpb.Entry, error) {
	// Empty log range should return immediately.
	if logRange.FirstIndex == logRange.LastIndex {
		return nil, nil
	}

	// Try to read the commands from the cache first.
	sh, ok := l.shardCache.Load(clusterID)
	if !ok {
		return nil, dragonboat.ErrShardNotReady
	}
	// Lock this shard.
	sh.mtx.Lock()
	defer sh.mtx.Unlock()

	cachedEntries, prependIndices, appendIndices := sh.get(logRange)

	if prependIndices.FirstIndex != 0 && prependIndices.LastIndex != 0 {
		// We have to query the log for the beginning of the range and prepend the cached entries.
		le, err := readLog(l.LogQuerier, clusterID, prependIndices, maxSize)
		if err != nil {
			return nil, err
		}

		// Only if cached and queried entries form a sequence append and cache them, otherwise return the prependIndices without caching.
		if len(le) == 0 {
			return fixSize(cachedEntries, maxSize), nil
		} else if len(cachedEntries) > 0 && le[len(le)-1].Index == cachedEntries[0].Index-1 {
			entries := append(le, cachedEntries...)
			return fixSize(entries, maxSize), nil
		} else {
			if sh.len() == 0 {
				sh.put(le)
			}
			return le, nil
		}
	}

	if appendIndices.FirstIndex != 0 && appendIndices.LastIndex != 0 {
		// We have to query the log for the end of the range and append the cached entries.
		le, err := readLog(l.LogQuerier, clusterID, appendIndices, maxSize)
		if err != nil {
			return nil, err
		}
		if len(le) == 0 {
			return fixSize(cachedEntries, maxSize), nil
		} else if len(cachedEntries) > 0 {
			sh.put(le)
			return fixSize(append(cachedEntries, le...), maxSize), nil
		} else {
			// Is consecutive to the cache if so cache the range.
			if le[0].Index-1 == sh.largestIndex() {
				sh.put(le)
			}
			return le, nil
		}
	}

	return fixSize(cachedEntries, maxSize), nil
}

func (l *Cached) NodeDeleted(info raftio.NodeInfo) {
	l.shardCache.Delete(info.ShardID)
}

func (l *Cached) NodeReady(info raftio.NodeInfo) {
	l.shardCache.ComputeIfAbsent(info.ShardID, func(shardId uint64) *shard { return &shard{cache: newCache(l.ShardCacheSize)} })
}

func (l *Cached) LogCompacted(info raftio.EntryInfo) {
	l.shardCache.Store(info.ShardID, &shard{cache: newCache(l.ShardCacheSize)})
}

func readLog(q logQuerier, clusterID uint64, logRange dragonboat.LogRange, maxSize uint64) ([]raftpb.Entry, error) {
	r, err := q.GetLogReader(clusterID)
	if err != nil {
		return nil, err
	}

	rFirst, rLast := r.GetRange()
	// Follower is up-to-date with the leader, therefore there are no new data to be sent.
	if rLast+1 == logRange.FirstIndex {
		return nil, nil
	}
	// Follower is ahead of the leader, has to be manually fixed.
	if rLast < logRange.FirstIndex {
		return nil, serrors.ErrLogBehind
	}
	// Follower's leaderIndex is in the leader's snapshot, not in the log.
	if logRange.FirstIndex < rFirst {
		return nil, serrors.ErrLogAhead
	}

	return r.Entries(logRange.FirstIndex, logRange.LastIndex, maxSize)
}

func fixSize(entries []raftpb.Entry, maxSize uint64) []raftpb.Entry {
	size := 0
	for i := 0; i < len(entries); i++ {
		size += entries[i].SizeUpperLimit()
		if uint64(size) >= maxSize {
			return entries[:i]
		}
	}
	return entries
}
