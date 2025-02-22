// Copyright JAMF Software, LLC

package storage

import (
	"context"
	"time"

	"github.com/jamf/regatta/regattapb"
	"github.com/jamf/regatta/storage/cluster"
	"github.com/jamf/regatta/storage/logreader"
	"github.com/jamf/regatta/storage/table"
	"github.com/lni/dragonboat/v4"
	"github.com/lni/dragonboat/v4/config"
	"github.com/lni/dragonboat/v4/plugin/tan"
	"github.com/lni/dragonboat/v4/raftio"
	protobuf "google.golang.org/protobuf/proto"
)

const defaultQueryTimeout = 5 * time.Second

func New(cfg Config) (*Engine, error) {
	e := &Engine{
		cfg: cfg,
	}
	nh, err := createNodeHost(cfg, e, e)
	if err != nil {
		return nil, err
	}

	manager := table.NewManager(
		nh,
		cfg.InitialMembers,
		table.Config{
			NodeID: cfg.NodeID,
			Table:  table.TableConfig(cfg.Table),
			Meta:   table.MetaConfig(cfg.Meta),
		},
	)
	if cfg.LogCacheSize > 0 {
		e.LogReader = &logreader.Cached{LogQuerier: nh, ShardCacheSize: cfg.LogCacheSize}
	} else {
		e.LogReader = &logreader.Simple{LogQuerier: nh}
	}
	e.NodeHost = nh
	clst, err := cluster.New(cfg.Gossip.BindAddress, cfg.Gossip.AdvertiseAddress, e.clusterInfo)
	if err != nil {
		return nil, err
	}
	e.Cluster = clst
	e.Manager = manager
	return e, nil
}

type Engine struct {
	*dragonboat.NodeHost
	*table.Manager
	cfg       Config
	LogReader logreader.Interface
	Cluster   *cluster.Cluster
}

func (e *Engine) Start() error {
	e.Cluster.Start(e.cfg.Gossip.InitialMembers)
	return e.Manager.Start()
}

func (e *Engine) Close() error {
	e.Manager.Close()
	e.NodeHost.Close()
	return nil
}

func (e *Engine) Range(ctx context.Context, req *regattapb.RangeRequest) (*regattapb.RangeResponse, error) {
	t, err := e.Manager.GetTable(string(req.Table))
	if err != nil {
		return nil, err
	}
	rng, err := withDefaultTimeout(ctx, req, t.Range)
	if err != nil {
		return nil, err
	}
	rng.Header = e.getHeader(nil, t.ClusterID)
	return rng, nil
}

func (e *Engine) Put(ctx context.Context, req *regattapb.PutRequest) (*regattapb.PutResponse, error) {
	t, err := e.Manager.GetTable(string(req.Table))
	if err != nil {
		return nil, err
	}
	put, err := withDefaultTimeout(ctx, req, t.Put)
	if err != nil {
		return nil, err
	}
	put.Header = e.getHeader(put.Header, t.ClusterID)
	return put, nil
}

func (e *Engine) Delete(ctx context.Context, req *regattapb.DeleteRangeRequest) (*regattapb.DeleteRangeResponse, error) {
	t, err := e.Manager.GetTable(string(req.Table))
	if err != nil {
		return nil, err
	}
	del, err := withDefaultTimeout(ctx, req, t.Delete)
	if err != nil {
		return nil, err
	}
	del.Header = e.getHeader(del.Header, t.ClusterID)
	return del, nil
}

func (e *Engine) Txn(ctx context.Context, req *regattapb.TxnRequest) (*regattapb.TxnResponse, error) {
	t, err := e.Manager.GetTable(string(req.Table))
	if err != nil {
		return nil, err
	}
	tx, err := withDefaultTimeout(ctx, req, t.Txn)
	if err != nil {
		return nil, err
	}
	tx.Header = e.getHeader(tx.Header, t.ClusterID)
	return tx, nil
}

func (e *Engine) getHeader(header *regattapb.ResponseHeader, shardID uint64) *regattapb.ResponseHeader {
	if header == nil {
		header = &regattapb.ResponseHeader{}
	}
	header.ReplicaId = e.cfg.NodeID
	header.ShardId = shardID
	info := e.Cluster.ShardInfo(shardID)
	header.RaftTerm = info.Term
	header.RaftLeaderId = info.LeaderID
	return header
}

func (e *Engine) NodeDeleted(info raftio.NodeInfo) {
	if info.ReplicaID == e.cfg.NodeID {
		e.LogReader.NodeDeleted(info)
	}
}

func (e *Engine) NodeReady(info raftio.NodeInfo) {
	if info.ReplicaID == e.cfg.NodeID {
		e.LogReader.NodeReady(info)
	}
}

func (e *Engine) LeaderUpdated(info raftio.LeaderInfo)             {}
func (e *Engine) NodeHostShuttingDown()                            {}
func (e *Engine) NodeUnloaded(info raftio.NodeInfo)                {}
func (e *Engine) MembershipChanged(info raftio.NodeInfo)           {}
func (e *Engine) ConnectionEstablished(info raftio.ConnectionInfo) {}
func (e *Engine) ConnectionFailed(info raftio.ConnectionInfo)      {}
func (e *Engine) SendSnapshotStarted(info raftio.SnapshotInfo)     {}
func (e *Engine) SendSnapshotCompleted(info raftio.SnapshotInfo)   {}
func (e *Engine) SendSnapshotAborted(info raftio.SnapshotInfo)     {}
func (e *Engine) SnapshotReceived(info raftio.SnapshotInfo)        {}
func (e *Engine) SnapshotRecovered(info raftio.SnapshotInfo)       {}
func (e *Engine) SnapshotCreated(info raftio.SnapshotInfo)         {}
func (e *Engine) SnapshotCompacted(info raftio.SnapshotInfo)       {}
func (e *Engine) LogCompacted(info raftio.EntryInfo) {
	if info.ReplicaID == e.cfg.NodeID {
		e.LogReader.LogCompacted(info)
	}
}
func (e *Engine) LogDBCompacted(info raftio.EntryInfo) {}

func (e *Engine) clusterInfo() cluster.Info {
	nhi := e.NodeHost.GetNodeHostInfo(dragonboat.DefaultNodeHostInfoOption)
	return cluster.Info{
		NodeHostID:    e.NodeHost.ID(),
		NodeID:        e.cfg.NodeID,
		RaftAddress:   e.cfg.RaftAddress,
		ShardInfoList: nhi.ShardInfoList,
		LogInfo:       nhi.LogInfo,
	}
}

func createNodeHost(cfg Config, sel raftio.ISystemEventListener, rel raftio.IRaftEventListener) (*dragonboat.NodeHost, error) {
	nhc := config.NodeHostConfig{
		WALDir:              cfg.WALDir,
		NodeHostDir:         cfg.NodeHostDir,
		RTTMillisecond:      cfg.RTTMillisecond,
		RaftAddress:         cfg.RaftAddress,
		ListenAddress:       cfg.ListenAddress,
		EnableMetrics:       true,
		MaxReceiveQueueSize: cfg.MaxReceiveQueueSize,
		MaxSendQueueSize:    cfg.MaxSendQueueSize,
		SystemEventListener: sel,
		RaftEventListener:   rel,
	}

	if cfg.LogDBImplementation == Tan {
		nhc.Expert.LogDBFactory = tan.Factory
	}
	nhc.Expert.LogDB = buildLogDBConfig()

	if cfg.FS != nil {
		nhc.Expert.FS = cfg.FS
	}

	err := nhc.Prepare()
	if err != nil {
		return nil, err
	}

	nh, err := dragonboat.NewNodeHost(nhc)
	if err != nil {
		return nil, err
	}
	return nh, nil
}

func buildLogDBConfig() config.LogDBConfig {
	cfg := config.GetSmallMemLogDBConfig()
	cfg.KVRecycleLogFileNum = 4
	cfg.KVMaxBytesForLevelBase = 128 * 1024 * 1024
	return cfg
}

func withDefaultTimeout[R protobuf.Message, S protobuf.Message](ctx context.Context, req R, f func(context.Context, R) (S, error)) (S, error) {
	if _, ok := ctx.Deadline(); !ok {
		dctx, cancel := context.WithTimeout(ctx, defaultQueryTimeout)
		defer cancel()
		ctx = dctx
	}
	return f(ctx, req)
}
