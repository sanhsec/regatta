package fsm

import (
	"encoding/binary"

	"github.com/cockroachdb/pebble"
	sm "github.com/lni/dragonboat/v4/statemachine"
	"github.com/wandera/regatta/proto"
	pb "google.golang.org/protobuf/proto"
)

type updateContext struct {
	batch *pebble.Batch
	wo    *pebble.WriteOptions
	db    *pebble.DB
	cmd   *proto.Command
	index uint64
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

func (c *updateContext) Parse(entry sm.Entry) (command, error) {
	c.index = entry.Index
	c.cmd.ResetVT()
	if err := c.cmd.UnmarshalVT(entry.Cmd); err != nil {
		return commandDummy{c}, err
	}
	switch c.cmd.Type {
	case proto.Command_PUT:
		return commandPut{c}, nil
	case proto.Command_DELETE:
		return commandDelete{c}, nil
	case proto.Command_PUT_BATCH:
		return commandPutBatch{c}, nil
	case proto.Command_DELETE_BATCH:
		return commandDeleteBatch{c}, nil
	case proto.Command_TXN:
		return commandTxn{c}, nil
	case proto.Command_DUMMY:
		return commandDummy{c}, nil
	}
	return commandDummy{c}, nil
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

type command interface {
	handle() (UpdateResult, *proto.CommandResult, error)
}

func wrapRequestOp(req pb.Message) *proto.RequestOp {
	switch op := req.(type) {
	case *proto.RequestOp_Range:
		return &proto.RequestOp{Request: &proto.RequestOp_RequestRange{RequestRange: op}}
	case *proto.RequestOp_Put:
		return &proto.RequestOp{Request: &proto.RequestOp_RequestPut{RequestPut: op}}
	case *proto.RequestOp_DeleteRange:
		return &proto.RequestOp{Request: &proto.RequestOp_RequestDeleteRange{RequestDeleteRange: op}}
	}
	return nil
}

func wrapResponseOp(req pb.Message) *proto.ResponseOp {
	switch op := req.(type) {
	case *proto.ResponseOp_Range:
		return &proto.ResponseOp{Response: &proto.ResponseOp_ResponseRange{ResponseRange: op}}
	case *proto.ResponseOp_Put:
		return &proto.ResponseOp{Response: &proto.ResponseOp_ResponsePut{ResponsePut: op}}
	case *proto.ResponseOp_DeleteRange:
		return &proto.ResponseOp{Response: &proto.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: op}}
	}
	return nil
}
