// Copyright JAMF Software, LLC

package table

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"github.com/cenkalti/backoff/v4"
	"github.com/cockroachdb/pebble"
	"github.com/jamf/regatta/regattapb"
	serrors "github.com/jamf/regatta/storage/errors"
	"github.com/jamf/regatta/storage/kv"
	"github.com/jamf/regatta/storage/table/fsm"
	"github.com/lni/dragonboat/v4"
	"github.com/lni/dragonboat/v4/config"
	"go.uber.org/zap"
)

type store interface {
	Exists(key string) (bool, error)
	Set(key string, value string, ver uint64) (kv.Pair, error)
	Delete(key string, ver uint64) error
	Get(key string) (kv.Pair, error)
	GetAll(pattern string) ([]kv.Pair, error)
}

const (
	keyPrefix                 = "/tables/"
	sequenceKey               = keyPrefix + "sys/idseq"
	metaFSMClusterID          = 1000
	tableIDsRangeStart uint64 = 10000
)

func NewManager(nh *dragonboat.NodeHost, members map[uint64]string, cfg Config) *Manager {
	blockCache := pebble.NewCache(cfg.Table.BlockCacheSize)
	tableCache := pebble.NewTableCache(blockCache, runtime.GOMAXPROCS(-1), cfg.Table.TableCacheSize)
	return &Manager{
		nh:                 nh,
		reconcileInterval:  30 * time.Second,
		cleanupInterval:    30 * time.Second,
		cleanupGracePeriod: 5 * time.Minute,
		cleanupTimeout:     5 * time.Minute,
		readyChan:          make(chan struct{}),
		members:            members,
		cfg:                cfg,
		store: &kv.RaftStore{
			NodeHost:  nh,
			ClusterID: metaFSMClusterID,
		},
		cache: struct {
			mu     sync.RWMutex
			tables map[string]ActiveTable
		}{
			tables: make(map[string]ActiveTable),
		},
		closed:     make(chan struct{}),
		log:        zap.S().Named("manager"),
		blockCache: blockCache,
		tableCache: tableCache,
	}
}

type Manager struct {
	store store
	nh    *dragonboat.NodeHost
	mtx   sync.RWMutex
	cache struct {
		mu     sync.RWMutex
		tables map[string]ActiveTable
	}
	members            map[uint64]string
	closed             chan struct{}
	cfg                Config
	readyChan          chan struct{}
	reconcileInterval  time.Duration
	cleanupInterval    time.Duration
	cleanupGracePeriod time.Duration
	cleanupTimeout     time.Duration
	log                *zap.SugaredLogger
	blockCache         *pebble.Cache
	tableCache         *pebble.TableCache
}

type Lease struct {
	ID    uint64    `json:"id"`
	Until time.Time `json:"until"`
}

func (m *Manager) LeaseTable(name string, lease time.Duration) error {
	key := storedTableName(name) + "/lease"
	get, err := m.store.Get(key)

	unclaimed := errors.Is(err, kv.ErrNotExist)
	if err != nil && !unclaimed {
		return err
	}

	l := Lease{}
	if !unclaimed {
		err = json.Unmarshal([]byte(get.Value), &l)
		if err != nil {
			return err
		}
	}

	if unclaimed || l.ID == m.cfg.NodeID || l.Until.Before(time.Now()) {
		l.ID = m.cfg.NodeID
		l.Until = time.Now().Add(lease)

		bts, err := json.Marshal(l)
		if err != nil {
			return err
		}
		_, err = m.store.Set(key, string(bts), get.Ver)
		if err != nil {
			return err
		}
		return nil
	}

	return serrors.ErrLeaseNotAcquired
}

// ReturnTable returns true if it was leased previously.
func (m *Manager) ReturnTable(name string) (bool, error) {
	key := storedTableName(name) + "/lease"
	get, err := m.store.Get(key)

	if errors.Is(err, kv.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	l := Lease{}
	err = json.Unmarshal([]byte(get.Value), &l)
	if err != nil {
		return false, err
	}

	if l.ID != m.cfg.NodeID {
		return false, nil
	}

	err = m.store.Delete(key, get.Ver)
	if err != nil {
		return false, err
	}

	return true, nil
}

func (m *Manager) CreateTable(name string) error {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	created, err := m.createTable(name)
	if err != nil {
		return err
	}

	return m.startTable(created.Name, created.ClusterID)
}

func (m *Manager) createTable(name string) (Table, error) {
	storeName := storedTableName(name)
	exists, err := m.store.Exists(storeName)
	if err != nil {
		return Table{}, err
	}
	if exists {
		return Table{}, serrors.ErrTableExists
	}
	seq, err := m.incAndGetIDSeq()
	if err != nil {
		return Table{}, err
	}
	tab := Table{
		Name:      name,
		ClusterID: seq,
	}
	err = m.setTableVersion(tab, 0)
	if err != nil {
		if errors.Is(err, kv.ErrVersionMismatch) {
			return Table{}, serrors.ErrTableExists
		}
		return Table{}, err
	}
	return tab, nil
}

func (m *Manager) DeleteTable(name string) error {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	storeName := storedTableName(name)
	tab, err := m.store.Get(storeName)
	if err != nil {
		return err
	}

	return m.store.Delete(storeName, tab.Ver)
}

func storedTableName(name string) string {
	return fmt.Sprintf("%s%s", keyPrefix, name)
}

func (m *Manager) GetTable(name string) (ActiveTable, error) {
	m.cache.mu.RLock()
	defer m.cache.mu.RUnlock()
	if t, ok := m.cache.tables[name]; ok {
		return t, nil
	}

	m.mtx.RLock()
	defer m.mtx.RUnlock()
	tab, _, err := m.getTableVersion(name)
	if err != nil {
		return ActiveTable{}, err
	}
	return tab.AsActive(m.nh), nil
}

func (m *Manager) GetTableByID(id uint64) (ActiveTable, error) {
	m.mtx.RLock()
	defer m.mtx.RUnlock()
	tables, err := m.getTables()
	if err != nil {
		return ActiveTable{}, err
	}
	for _, t := range tables {
		if t.ClusterID == id {
			return t.AsActive(m.nh), nil
		}
	}
	return ActiveTable{}, serrors.ErrTableNotFound
}

func (m *Manager) GetTables() ([]Table, error) {
	m.mtx.RLock()
	defer m.mtx.RUnlock()

	tabs, err := m.getTables()
	if err != nil {
		return nil, err
	}
	rtabs := make([]Table, 0, len(tabs))
	for _, t := range tabs {
		rtabs = append(rtabs, t)
	}
	return rtabs, nil
}

func (m *Manager) Start() error {
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-m.closed:
				return
			case <-t.C:
				_, _, ok, _ := m.nh.GetLeaderID(metaFSMClusterID)
				if ok {
					go m.reconcileLoop()
					go m.cleanupLoop()
					close(m.readyChan)
					return
				}
			}
		}
	}()

	if m.nh.HasNodeInfo(metaFSMClusterID, m.cfg.NodeID) {
		return m.nh.StartConcurrentReplica(map[uint64]dragonboat.Target{}, false, kv.NewLFSM(), metaRaftConfig(m.cfg.NodeID, m.cfg.Meta))
	}
	return m.nh.StartConcurrentReplica(m.members, false, kv.NewLFSM(), metaRaftConfig(m.cfg.NodeID, m.cfg.Meta))
}

func (m *Manager) WaitUntilReady() error {
	for {
		select {
		case <-m.readyChan:
			return nil
		case <-m.closed:
			return serrors.ErrManagerClosed
		}
	}
}

func (m *Manager) Close() {
	close(m.closed)
}

func (m *Manager) reconcileLoop() {
	t := time.NewTicker(m.reconcileInterval)
	defer t.Stop()
	for {
		err := m.reconcile()
		if err != nil {
			m.log.Errorf("reconcile failed: %v", err)
		}
		select {
		case <-m.closed:
			return
		case <-t.C:
			continue
		}
	}
}

func (m *Manager) reconcile() error {
	// FIXME there is still a distributed race condition across the instances
	tabs, nhi, err := func() (map[string]Table, *dragonboat.NodeHostInfo, error) {
		m.mtx.RLock()
		defer m.mtx.RUnlock()
		tabs, err := m.getTables()
		if err != nil {
			return nil, nil, err
		}
		nhi := m.nh.GetNodeHostInfo(dragonboat.DefaultNodeHostInfoOption)
		if nhi == nil {
			return nil, nil, serrors.ErrNodeHostInfoUnavailable
		}
		return tabs, nhi, nil
	}()
	if err != nil {
		return err
	}

	for _, t := range tabs {
		m.cacheTable(t)
	}

	start, stop := diffTables(tabs, nhi.ShardInfoList)
	for id, tbl := range start {
		err = m.startTable(tbl.Name, id)
		if err != nil {
			return err
		}
		m.cacheTable(tbl)
	}

	for _, tab := range stop {
		err = m.stopTable(tab)
		if err != nil {
			return err
		}
		m.clearTable(tab)
	}

	return nil
}

func (m *Manager) cleanupLoop() {
	t := time.NewTicker(m.cleanupInterval)
	defer t.Stop()
	for {
		err := m.cleanup()
		if err != nil {
			m.log.Errorf("cleanup failed: %v", err)
		}
		select {
		case <-m.closed:
			return
		case <-t.C:
			continue
		}
	}
}

func (m *Manager) cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), m.cleanupTimeout)
	defer cancel()
	ls, err := m.store.GetAll(fmt.Sprintf("/cleanup/%d/*", m.cfg.NodeID))
	if err != nil {
		return err
	}
	for _, l := range ls {
		c := Cleanup{}
		if err := json.Unmarshal([]byte(l.Value), &c); err != nil {
			return err
		}
		if c.Created.Before(time.Now().Add(-m.cleanupGracePeriod)) {
			// Distributed data race guard
			if _, err := m.GetTableByID(c.ClusterID); !errors.Is(err, serrors.ErrTableNotFound) {
				m.log.Warnf("[%d:%d] cluster data cleanup skipped, table should not be deleted", c.ClusterID, m.cfg.NodeID)
				return m.store.Delete(l.Key, l.Ver)
			}

			if err := m.nh.SyncRemoveData(ctx, c.ClusterID, m.cfg.NodeID); err != nil {
				return err
			}
			if err := m.cfg.Table.FS.RemoveAll(c.SMDataPath); err != nil {
				return err
			}
			if err := m.store.Delete(l.Key, l.Ver); err != nil {
				return err
			}
			m.log.Infof("[%d:%d] cluster data cleaned", c.ClusterID, m.cfg.NodeID)
		}
	}
	return nil
}

func (m *Manager) incAndGetIDSeq() (uint64, error) {
	seq, err := m.store.Get(sequenceKey)
	if err != nil {
		if errors.Is(err, kv.ErrNotExist) {
			seq = kv.Pair{
				Key:   sequenceKey,
				Value: strconv.FormatUint(tableIDsRangeStart, 10),
				Ver:   0,
			}
		} else {
			return 0, err
		}
	}
	currSeq, err := strconv.ParseUint(seq.Value, 10, 64)
	if err != nil {
		return 0, err
	}
	next := currSeq + 1

	_, err = m.store.Set(seq.Key, strconv.FormatUint(next, 10), seq.Ver)
	return next, err
}

func (m *Manager) getTables() (map[string]Table, error) {
	tables := make(map[string]Table)
	all, err := m.store.GetAll(keyPrefix + "*")
	if err != nil {
		return nil, err
	}
	for _, v := range all {
		tab := Table{}
		err = json.Unmarshal([]byte(v.Value), &tab)
		if err != nil {
			return nil, err
		}
		tables[tab.Name] = tab
	}

	return tables, nil
}

func diffTables(tables map[string]Table, raftInfo []dragonboat.ShardInfo) (toStart map[uint64]Table, toStop []uint64) {
	tableIDs := make(map[uint64]Table)
	for _, t := range tables {
		if t.ClusterID != 0 {
			tableIDs[t.ClusterID] = t
		}
		if t.RecoverID != 0 {
			tableIDs[t.RecoverID] = t
		}
	}
	raftTableIDs := make(map[uint64]struct{})
	for _, t := range raftInfo {
		raftTableIDs[t.ShardID] = struct{}{}
	}

	for tID, tName := range tableIDs {
		_, found := raftTableIDs[tID]
		if !found && tID > tableIDsRangeStart {
			if toStart == nil {
				toStart = make(map[uint64]Table)
			}
			toStart[tID] = tName
		}
	}

	for rID := range raftTableIDs {
		_, found := tableIDs[rID]
		if !found && rID > tableIDsRangeStart {
			toStop = append(toStop, rID)
		}
	}
	return
}

func (m *Manager) startTable(name string, id uint64) error {
	if m.nh.HasNodeInfo(id, m.cfg.NodeID) {
		return m.nh.StartOnDiskReplica(
			map[uint64]dragonboat.Target{},
			false,
			fsm.New(name, m.cfg.Table.DataDir, m.cfg.Table.FS, m.blockCache, m.tableCache, fsm.SnapshotRecoveryType(m.cfg.Table.RecoveryType)),
			tableRaftConfig(m.cfg.NodeID, id, m.cfg.Table),
		)
	}
	return m.nh.StartOnDiskReplica(
		m.members,
		false,
		fsm.New(name, m.cfg.Table.DataDir, m.cfg.Table.FS, m.blockCache, m.tableCache, fsm.SnapshotRecoveryType(m.cfg.Table.RecoveryType)),
		tableRaftConfig(m.cfg.NodeID, id, m.cfg.Table),
	)
}

func (m *Manager) cacheTable(tbl Table) {
	m.cache.mu.Lock()
	defer m.cache.mu.Unlock()
	m.cache.tables[tbl.Name] = tbl.AsActive(m.nh)
}

func (m *Manager) clearTable(clusterID uint64) {
	m.cache.mu.Lock()
	defer m.cache.mu.Unlock()
	for name, activeTable := range m.cache.tables {
		if activeTable.ClusterID == clusterID {
			delete(m.cache.tables, name)
		}
	}
}

type Cleanup struct {
	Created    time.Time `json:"created"`
	ClusterID  uint64    `json:"cluster_id"`
	SMDataPath string    `json:"sm_data_path"`
}

func (m *Manager) stopTable(clusterID uint64) error {
	v, err := m.nh.StaleRead(clusterID, fsm.PathRequest{})
	if err != nil {
		return err
	}
	pr := v.(*fsm.PathResponse)
	b, err := json.Marshal(&Cleanup{
		Created:    time.Now(),
		ClusterID:  clusterID,
		SMDataPath: pr.Path,
	})
	if err != nil {
		return err
	}
	key := fmt.Sprintf("/cleanup/%d/%d", m.cfg.NodeID, clusterID)
	c, err := m.store.Get(key)
	if err != nil && !errors.Is(err, kv.ErrNotExist) {
		return err
	}
	_, err = m.store.Set(key, string(b), c.Ver)
	if err != nil {
		return err
	}
	if err := m.nh.StopShard(clusterID); err != nil {
		return err
	}

	// Unregister metrics, check dragonboat/v4/event.go for metric names
	if m.nh.NodeHostConfig().EnableMetrics {
		label := fmt.Sprintf(`{shardid="%d",replicaid="%d"}`, clusterID, m.cfg.NodeID)
		metrics.UnregisterMetric(fmt.Sprintf(`dragonboat_raftnode_campaign_launched_total%s`, label))
		metrics.UnregisterMetric(fmt.Sprintf(`dragonboat_raftnode_campaign_skipped_total%s`, label))
		metrics.UnregisterMetric(fmt.Sprintf(`dragonboat_raftnode_snapshot_rejected_total%s`, label))
		metrics.UnregisterMetric(fmt.Sprintf(`dragonboat_raftnode_replication_rejected_total%s`, label))
		metrics.UnregisterMetric(fmt.Sprintf(`dragonboat_raftnode_proposal_dropped_total%s`, label))
		metrics.UnregisterMetric(fmt.Sprintf(`dragonboat_raftnode_read_index_dropped_total%s`, label))
		metrics.UnregisterMetric(fmt.Sprintf(`dragonboat_raftnode_has_leader%s`, label))
	}
	return nil
}

func (m *Manager) Restore(name string, reader io.Reader) error {
	tbl, version, err := m.getTableVersion(name)
	if err != nil && !errors.Is(err, serrors.ErrTableNotFound) {
		return err
	}
	recoveryID, err := m.incAndGetIDSeq()
	if err != nil {
		return err
	}

	tbl.Name = name
	tbl.RecoverID = recoveryID

	err = m.startTable(tbl.Name, tbl.RecoverID)
	if err != nil {
		return err
	}

	err = m.setTableVersion(tbl, version)
	if err != nil {
		return err
	}

	err = m.waitForLeader(tbl.RecoverID)
	if err != nil {
		return err
	}

	err = m.readIntoTable(tbl.RecoverID, reader)
	if err != nil {
		return err
	}

	tbl, version, err = m.getTableVersion(name)
	if err != nil {
		return err
	}

	tbl.ClusterID = recoveryID
	tbl.RecoverID = 0
	err = m.setTableVersion(tbl, version)
	if err != nil {
		return err
	}
	m.cacheTable(tbl)
	return nil
}

func (m *Manager) getTableVersion(name string) (Table, uint64, error) {
	v, err := m.store.Get(storedTableName(name))
	if err != nil {
		if errors.Is(err, kv.ErrNotExist) {
			return Table{}, 0, serrors.ErrTableNotFound
		}
		return Table{}, 0, err
	}
	tab := Table{}
	err = json.Unmarshal([]byte(v.Value), &tab)
	return tab, v.Ver, err
}

func (m *Manager) setTableVersion(tbl Table, version uint64) error {
	storeName := storedTableName(tbl.Name)
	bts, err := json.Marshal(&tbl)
	if err != nil {
		return err
	}
	_, err = m.store.Set(storeName, string(bts), version)
	if err != nil {
		return err
	}
	return nil
}

func (m *Manager) readIntoTable(id uint64, reader io.Reader) error {
	backOff := backoff.NewExponentialBackOff()
	backOff.MaxElapsedTime = 0
	session := m.nh.GetNoOPSession(id)
	msg := make([]byte, 1024*1024*4)

	cmd := &regattapb.Command{}
	batchCmd := &regattapb.Command{
		Type: regattapb.Command_PUT_BATCH,
	}
	last := false

	estimatedSize := 0
	for {
		n, err := reader.Read(msg)
		if err != nil {
			if err == io.EOF {
				last = true
			} else {
				return err
			}
		}
		estimatedSize += n

		if !last {
			cmd.Reset()
			err = cmd.UnmarshalVT(msg[:n])
			if err != nil {
				return err
			}

			batchCmd.Table = cmd.Table
			batchCmd.LeaderIndex = cmd.LeaderIndex

			if uint64(estimatedSize) < m.cfg.Table.MaxInMemLogSize/2 {
				batchCmd.Batch = append(batchCmd.Batch, cmd.Kv)
				continue
			}
		}

		bb, err := batchCmd.MarshalVT()
		if err != nil {
			return err
		}
		batchCmd.LeaderIndex = nil
		batchCmd.Batch = batchCmd.Batch[:0]

		err = backoff.Retry(func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, err := m.nh.SyncPropose(ctx, session, bb)
			if err != nil {
				if errors.Is(err, dragonboat.ErrShardNotFound) {
					m.log.Warn("cluster not found recovery probably started on a different node")
					return backoff.Permanent(err)
				}
				m.log.Warnf("error proposing batch %v", err)
				return err
			}
			return nil
		}, backOff)

		if err != nil {
			return err
		}

		estimatedSize = 0

		if last {
			break
		}
	}
	return nil
}

func (m *Manager) waitForLeader(clusterID uint64) error {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()

	// TODO make configurable
	ctx, cancel := context.WithTimeout(context.Background(), m.reconcileInterval*2)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			_, _, ok, _ := m.nh.GetLeaderID(clusterID)
			if ok {
				return nil
			}
		}
	}
}

func tableRaftConfig(nodeID, clusterID uint64, cfg TableConfig) config.Config {
	return config.Config{
		ReplicaID:               nodeID,
		ShardID:                 clusterID,
		CheckQuorum:             true,
		OrderedConfigChange:     true,
		ElectionRTT:             cfg.ElectionRTT,
		HeartbeatRTT:            cfg.HeartbeatRTT,
		SnapshotEntries:         cfg.SnapshotEntries,
		CompactionOverhead:      cfg.CompactionOverhead,
		MaxInMemLogSize:         cfg.MaxInMemLogSize,
		SnapshotCompressionType: config.Snappy,
	}
}

func metaRaftConfig(nodeID uint64, cfg MetaConfig) config.Config {
	return config.Config{
		ReplicaID:           nodeID,
		ShardID:             metaFSMClusterID,
		CheckQuorum:         true,
		PreVote:             true,
		ElectionRTT:         cfg.ElectionRTT,
		HeartbeatRTT:        cfg.HeartbeatRTT,
		SnapshotEntries:     cfg.SnapshotEntries,
		CompactionOverhead:  cfg.CompactionOverhead,
		OrderedConfigChange: true,
		MaxInMemLogSize:     cfg.MaxInMemLogSize,
	}
}
