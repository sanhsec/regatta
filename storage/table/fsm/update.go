package fsm

import (
	"bytes"
	"encoding/binary"
	"sync/atomic"

	"github.com/cockroachdb/pebble"
	sm "github.com/lni/dragonboat/v3/statemachine"
	"github.com/wandera/regatta/proto"
	"github.com/wandera/regatta/storage/table/key"
)

// Update updates the object.
func (p *FSM) Update(updates []sm.Entry) ([]sm.Entry, error) {
	db := (*pebble.DB)(atomic.LoadPointer(&p.pebble))

	ctx := &updateContext{
		batch:  db.NewBatch(),
		db:     db,
		wo:     p.wo,
		cmd:    proto.CommandFromVTPool(),
		keyBuf: bytes.NewBuffer(make([]byte, 0, key.LatestVersionLen)),
	}

	defer func() {
		_ = ctx.Close()
	}()

	for i := 0; i < len(updates); i++ {
		if err := ctx.Init(updates[i]); err != nil {
			return nil, err
		}

		var (
			result sm.Result
			err    error
		)
		switch ctx.cmd.Type {
		case proto.Command_PUT:
			result, err = handlePut(ctx)
		case proto.Command_DELETE:
			result, err = handleDelete(ctx)
		case proto.Command_PUT_BATCH:
			result, err = handlePutBatch(ctx)
		case proto.Command_DELETE_BATCH:
			result, err = handleDeleteBatch(ctx)
		case proto.Command_DUMMY:
			result = sm.Result{Value: ResultSuccess}
		}
		if err != nil {
			return nil, err
		}
		updates[i].Result = result
	}

	if err := ctx.Commit(); err != nil {
		return nil, err
	}

	return updates, nil
}

func handlePut(ctx *updateContext) (sm.Result, error) {
	if err := encodeUserKey(ctx.keyBuf, ctx.cmd.Kv.Key); err != nil {
		return sm.Result{Value: ResultFailure}, err
	}
	if err := ctx.batch.Set(ctx.keyBuf.Bytes(), ctx.cmd.Kv.Value, nil); err != nil {
		return sm.Result{Value: ResultFailure}, err
	}
	return sm.Result{Value: ResultSuccess}, nil
}

func handleDelete(ctx *updateContext) (sm.Result, error) {
	if err := encodeUserKey(ctx.keyBuf, ctx.cmd.Kv.Key); err != nil {
		return sm.Result{Value: ResultFailure}, err
	}
	if ctx.cmd.RangeEnd != nil {
		end := ctx.cmd.RangeEnd
		if bytes.Equal(end, wildcard) {
			end = key.LatestMaxKey
		}
		upperBuf := bytes.NewBuffer(make([]byte, 0, key.LatestKeyLen(len(end))))
		if err := encodeUserKey(upperBuf, end); err != nil {
			return sm.Result{Value: ResultFailure}, err
		}

		if err := ctx.batch.DeleteRange(ctx.keyBuf.Bytes(), upperBuf.Bytes(), nil); err != nil {
			return sm.Result{Value: ResultFailure}, err
		}
	} else {
		if err := ctx.batch.Delete(ctx.keyBuf.Bytes(), nil); err != nil {
			return sm.Result{Value: ResultFailure}, err
		}
	}
	return sm.Result{Value: ResultSuccess}, nil
}

func handlePutBatch(ctx *updateContext) (sm.Result, error) {
	for _, kv := range ctx.cmd.Batch {
		if err := encodeUserKey(ctx.keyBuf, kv.Key); err != nil {
			return sm.Result{Value: ResultFailure}, err
		}
		if err := ctx.batch.Set(ctx.keyBuf.Bytes(), kv.Value, nil); err != nil {
			return sm.Result{Value: ResultFailure}, err
		}
		ctx.keyBuf.Reset()
	}
	return sm.Result{Value: ResultSuccess}, nil
}

func handleDeleteBatch(ctx *updateContext) (sm.Result, error) {
	for _, kv := range ctx.cmd.Batch {
		if err := encodeUserKey(ctx.keyBuf, kv.Key); err != nil {
			return sm.Result{Value: ResultFailure}, err
		}
		if err := ctx.batch.Delete(ctx.keyBuf.Bytes(), nil); err != nil {
			return sm.Result{Value: ResultFailure}, err
		}
		ctx.keyBuf.Reset()
	}
	return sm.Result{Value: ResultSuccess}, nil
}

type updateContext struct {
	batch  *pebble.Batch
	wo     *pebble.WriteOptions
	db     *pebble.DB
	cmd    *proto.Command
	keyBuf *bytes.Buffer
	index  uint64
}

func (c *updateContext) EnsureIndexed() error {
	if c.batch.Indexed() {
		return nil
	}

	indexed := c.db.NewIndexedBatch()
	if err := indexed.Apply(c.batch, nil); err != nil {
		return err
	}
	if err := c.batch.Close(); err != nil {
		return err
	}
	c.batch = indexed
	return nil
}

func (c *updateContext) Init(entry sm.Entry) error {
	c.index = entry.Index
	c.cmd.ResetVT()
	if err := c.cmd.UnmarshalVT(entry.Cmd); err != nil {
		return err
	}
	c.keyBuf.Reset()
	return nil
}

func (c *updateContext) Commit() error {
	// Set leader index if present in the proposal
	if c.cmd.LeaderIndex != nil {
		leaderIdx := make([]byte, 8)
		binary.LittleEndian.PutUint64(leaderIdx, *c.cmd.LeaderIndex)
		if err := c.batch.Set(sysLeaderIndex, leaderIdx, nil); err != nil {
			return err
		}
	}
	// Set local index
	idx := make([]byte, 8)
	binary.LittleEndian.PutUint64(idx, c.index)
	if err := c.batch.Set(sysLocalIndex, idx, nil); err != nil {
		return err
	}
	return c.batch.Commit(c.wo)
}

func (c *updateContext) Close() error {
	if err := c.batch.Close(); err != nil {
		return err
	}
	c.cmd.ReturnToVTPool()
	return nil
}
