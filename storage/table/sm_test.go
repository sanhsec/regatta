package table

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/vfs"
	sm "github.com/lni/dragonboat/v3/statemachine"
	"github.com/stretchr/testify/require"
	"github.com/wandera/regatta/proto"
	"github.com/wandera/regatta/util"
	"go.uber.org/zap"
	pb "google.golang.org/protobuf/proto"
)

const (
	testValue          = "test"
	testTable          = "test"
	testKeyFormat      = "test%d"
	testLargeKeyFormat = "testlarge%d"
)

func TestSM_Open(t *testing.T) {
	type fields struct {
		clusterID  uint64
		nodeID     uint64
		dirname    string
		walDirname string
	}
	tests := []struct {
		name    string
		fields  fields
		wantErr bool
	}{
		{
			name: "Invalid node ID",
			fields: fields{
				clusterID: 1,
				nodeID:    0,
				dirname:   "/tmp/dir",
			},
			wantErr: true,
		},
		{
			name: "Invalid cluster ID",
			fields: fields{
				clusterID: 0,
				nodeID:    1,
				dirname:   "/tmp/dir",
			},
			wantErr: true,
		},
		{
			name: "Successfully open DB",
			fields: fields{
				clusterID: 1,
				nodeID:    1,
				dirname:   "/tmp/dir",
			},
		},
		{
			name: "Successfully open DB with WAL",
			fields: fields{
				clusterID:  1,
				nodeID:     1,
				dirname:    "/tmp/dir",
				walDirname: "/tmp/waldir",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)
			p := &SM{
				fs:         vfs.NewMem(),
				clusterID:  tt.fields.clusterID,
				nodeID:     tt.fields.nodeID,
				dirname:    tt.fields.dirname,
				walDirname: tt.fields.walDirname,
				log:        zap.S(),
			}
			_, err := p.Open(nil)
			if tt.wantErr {
				r.Error(err)
			} else {
				r.NoError(err)
				r.NoError(p.Close())
			}
		})
	}
}

func TestSMReOpen(t *testing.T) {
	r := require.New(t)
	fs := vfs.NewMem()
	const testIndex uint64 = 10
	p := &SM{
		fs:         fs,
		clusterID:  1,
		nodeID:     1,
		dirname:    "/tmp/dir",
		walDirname: "/tmp/dir",
		log:        zap.S(),
	}

	t.Log("open SM")
	index, err := p.Open(nil)
	r.NoError(err)
	r.Equal(uint64(0), index)

	t.Log("propose into SM")
	_, err = p.Update([]sm.Entry{
		{
			Index: testIndex,
			Cmd: mustMarshallProto(&proto.Command{
				Kv: &proto.KeyValue{
					Key:   []byte("foo"),
					Value: []byte("bar"),
				},
			}),
		},
	})
	r.NoError(err)
	r.NoError(p.Close())

	t.Log("reopen SM")
	index, err = p.Open(nil)
	r.NoError(err)
	r.Equal(testIndex, index)
}

func emptySM() sm.IOnDiskStateMachine {
	p := &SM{
		fs:        vfs.NewMem(),
		clusterID: 1,
		nodeID:    1,
		dirname:   "/tmp",
		log:       zap.S(),
	}
	_, err := p.Open(nil)
	if err != nil {
		zap.S().Panic(err)
	}
	return p
}

func filledSM() sm.IOnDiskStateMachine {
	const smallEntries = 10_000
	const largeEntries = 10

	entries := make([]sm.Entry, 0, smallEntries+largeEntries)
	for i := 0; i < smallEntries; i++ {
		entries = append(entries, sm.Entry{
			Index: uint64(i),
			Cmd: mustMarshallProto(&proto.Command{
				Table: []byte(testTable),
				Type:  proto.Command_PUT,
				Kv: &proto.KeyValue{
					Key:   []byte(fmt.Sprintf(testKeyFormat, i)),
					Value: []byte(testValue),
				},
			}),
		})
	}
	for i := 0; i < largeEntries; i++ {
		entries = append(entries, sm.Entry{
			Index: uint64(i),
			Cmd: mustMarshallProto(&proto.Command{
				Table: []byte(testTable),
				Type:  proto.Command_PUT,
				Kv: &proto.KeyValue{
					Key:   []byte(fmt.Sprintf(testLargeKeyFormat, i)),
					Value: []byte(largeValues[i]),
				},
			}),
		})
	}
	p := &SM{
		fs:        vfs.NewMem(),
		clusterID: 1,
		nodeID:    1,
		dirname:   "/tmp",
		log:       zap.S(),
	}
	_, err := p.Open(nil)
	if err != nil {
		zap.S().Panic(err)
	}
	_, err = p.Update(entries)
	if err != nil {
		zap.S().Panic(err)
	}
	return p
}

func filledLargeValuesSM() sm.IOnDiskStateMachine {
	entries := make([]sm.Entry, len(largeValues))
	for i := 0; i < len(entries); i++ {
		entries[i] = sm.Entry{
			Index: uint64(i),
			Cmd: mustMarshallProto(&proto.Command{
				Table: []byte(testTable),
				Type:  proto.Command_PUT,
				Kv: &proto.KeyValue{
					Key:   []byte(fmt.Sprintf(testKeyFormat, i)),
					Value: []byte(largeValues[i]),
				},
			}),
		}
	}
	p := &SM{
		fs:        vfs.NewMem(),
		clusterID: 1,
		nodeID:    1,
		dirname:   "/tmp",
		log:       zap.S(),
	}
	_, err := p.Open(nil)
	if err != nil {
		zap.S().Panic(err)
	}
	_, err = p.Update(entries)
	if err != nil {
		zap.S().Panic(err)
	}
	return p
}

var largeValues []string

func init() {
	for i := 0; i < 10_000; i++ {
		largeValues = append(largeValues, util.RandString(2048))
	}
}

func TestSM_Snapshot(t *testing.T) {
	type args struct {
		producingSMFactory func() sm.IOnDiskStateMachine
		receivingSMFactory func() sm.IOnDiskStateMachine
	}
	tests := []struct {
		name string
		args args
	}{
		{
			"Pebble -> Pebble",
			args{
				producingSMFactory: filledSM,
				receivingSMFactory: emptySM,
			},
		},
		{
			"Pebble(large) -> Pebble",
			args{
				producingSMFactory: filledLargeValuesSM,
				receivingSMFactory: emptySM,
			},
		},
	}
	for _, tt := range tests {
		t.Log("Applying snapshot to the empty DB should produce the same hash")
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)
			p := tt.args.producingSMFactory()
			defer p.Close()

			want, err := p.(sm.IHash).GetHash()
			r.NoError(err)

			snp, err := p.PrepareSnapshot()
			r.NoError(err)

			ep := tt.args.receivingSMFactory()
			defer ep.Close()

			snapf, err := os.Create(filepath.Join(t.TempDir(), "snapshot-file"))
			if err == nil {
				defer snapf.Close()
			}
			r.NoError(err)

			t.Log("Save snapshot started")
			err = p.SaveSnapshot(snp, snapf, nil)
			r.NoError(err)
			_, err = snapf.Seek(0, 0)
			r.NoError(err)

			t.Log("Recover from snapshot started")
			stopc := make(chan struct{})
			err = ep.RecoverFromSnapshot(snapf, stopc)
			r.NoError(err)

			t.Log("Recovery finished")

			got, err := ep.(sm.IHash).GetHash()
			r.NoError(err)
			r.Equal(want, got, "the hash of recovered DB should be the same as of the original one")
		})
	}
}

func TestSM_Snapshot_Stopped(t *testing.T) {
	type args struct {
		producingSMFactory func() sm.IOnDiskStateMachine
		receivingSMFactory func() sm.IOnDiskStateMachine
	}
	tests := []struct {
		name string
		args args
	}{
		{
			"Pebble(large) -> Pebble",
			args{
				producingSMFactory: filledLargeValuesSM,
				receivingSMFactory: emptySM,
			},
		},
	}
	for _, tt := range tests {
		t.Log("Applying snapshot to the empty DB should be stopped")
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)
			p := tt.args.producingSMFactory()
			defer p.Close()

			snp, err := p.PrepareSnapshot()
			r.NoError(err)

			ep := tt.args.receivingSMFactory()

			snapf, err := os.Create(filepath.Join(t.TempDir(), "snapshot-file"))
			r.NoError(err)
			t.Log("Save snapshot started")
			err = p.SaveSnapshot(snp, snapf, nil)
			r.NoError(err)
			_, err = snapf.Seek(0, 0)
			r.NoError(err)

			stopc := make(chan struct{})
			go func() {
				defer func() {
					_ = snapf.Close()
					_ = ep.Close()
					_ = snapf.Close()
				}()
				t.Log("Recover from snapshot routine started")
				err := ep.RecoverFromSnapshot(snapf, stopc)
				r.Error(err)
				r.Equal(sm.ErrSnapshotStopped, err)
			}()

			time.Sleep(10 * time.Millisecond)
			close(stopc)

			t.Log("Recovery stopped")
		})
	}
}

func TestSM_Lookup(t *testing.T) {
	type fields struct {
		smFactory func() sm.IOnDiskStateMachine
	}
	type args struct {
		key *proto.RangeRequest
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    interface{}
		wantErr bool
	}{
		{
			name: "Pebble - Lookup empty DB",
			fields: fields{
				smFactory: emptySM,
			},
			args: args{key: &proto.RangeRequest{
				Key: []byte("Hello"),
			}},
			wantErr: true,
		},
		{
			name: "Pebble - Lookup full DB with non-existent key",
			fields: fields{
				smFactory: filledSM,
			},
			args: args{key: &proto.RangeRequest{
				Key: []byte("Hello"),
			}},
			wantErr: true,
		},
		{
			name: "Pebble - Lookup full DB with existing key",
			fields: fields{
				smFactory: filledSM,
			},
			args: args{key: &proto.RangeRequest{
				Table: []byte(testTable),
				Key:   []byte(fmt.Sprintf(testKeyFormat, 0)),
			}},
			want: &proto.RangeResponse{
				Kvs: []*proto.KeyValue{
					{
						Key:   []byte(fmt.Sprintf(testKeyFormat, 0)),
						Value: []byte(testValue),
					},
				},
				Count: 1,
			},
		},
		{
			name: "Pebble - Lookup full DB with existing key and large value",
			fields: fields{
				smFactory: filledSM,
			},
			args: args{key: &proto.RangeRequest{
				Table: []byte(testTable),
				Key:   []byte(fmt.Sprintf(testLargeKeyFormat, 0)),
			}},
			want: &proto.RangeResponse{
				Kvs: []*proto.KeyValue{
					{
						Key:   []byte(fmt.Sprintf(testLargeKeyFormat, 0)),
						Value: []byte(largeValues[0]),
					},
				},
				Count: 1,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)
			p := tt.fields.smFactory()
			defer func() {
				r.NoError(p.Close())
			}()
			got, err := p.Lookup(tt.args.key)
			if tt.wantErr {
				r.Error(err)
				return
			}
			r.NoError(err)
			r.Equal(tt.want, got)
		})
	}
}

func TestSM_Update(t *testing.T) {
	type fields struct {
		smFactory func() sm.IOnDiskStateMachine
	}
	type args struct {
		updates []sm.Entry
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    []sm.Entry
		wantErr bool
	}{
		{
			name: "Pebble - Successful update of a single item",
			fields: fields{
				smFactory: emptySM,
			},
			args: args{
				updates: []sm.Entry{
					{
						Index: 1,
						Cmd: mustMarshallProto(&proto.Command{
							Table: []byte("test"),
							Type:  proto.Command_PUT,
							Kv: &proto.KeyValue{
								Key:   []byte("test"),
								Value: []byte("test"),
							},
						}),
					},
				},
			},
			want: []sm.Entry{
				{
					Index: 1,
					Cmd: mustMarshallProto(&proto.Command{
						Table: []byte("test"),
						Type:  proto.Command_PUT,
						Kv: &proto.KeyValue{
							Key:   []byte("test"),
							Value: []byte("test"),
						},
					}),
					Result: sm.Result{
						Value: 1,
						Data:  nil,
					},
				},
			},
		},
		{
			name: "Pebble - Successful update of a batch",
			fields: fields{
				smFactory: emptySM,
			},
			args: args{
				updates: []sm.Entry{
					{
						Index: 1,
						Cmd: mustMarshallProto(&proto.Command{
							Table: []byte("test"),
							Type:  proto.Command_PUT,
							Kv: &proto.KeyValue{
								Key:   []byte("test"),
								Value: []byte("test"),
							},
						}),
					},
					{
						Index: 2,
						Cmd: mustMarshallProto(&proto.Command{
							Table: []byte("test"),
							Type:  proto.Command_DELETE,
							Kv: &proto.KeyValue{
								Key: []byte("test"),
							},
						}),
					},
				},
			},
			want: []sm.Entry{
				{
					Index: 1,
					Cmd: mustMarshallProto(&proto.Command{
						Table: []byte("test"),
						Type:  proto.Command_PUT,
						Kv: &proto.KeyValue{
							Key:   []byte("test"),
							Value: []byte("test"),
						},
					}),
					Result: sm.Result{
						Value: 1,
						Data:  nil,
					},
				},
				{
					Index: 2,
					Cmd: mustMarshallProto(&proto.Command{
						Table: []byte("test"),
						Type:  proto.Command_DELETE,
						Kv: &proto.KeyValue{
							Key: []byte("test"),
						},
					}),
					Result: sm.Result{
						Value: 1,
						Data:  nil,
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)
			p := tt.fields.smFactory()
			defer func() {
				r.NoError(p.Close())
			}()
			got, err := p.Update(tt.args.updates)
			if tt.wantErr {
				r.Error(err)
				return
			}
			r.Equal(tt.want, got)
		})
	}
}

func mustMarshallProto(message pb.Message) []byte {
	bytes, err := pb.Marshal(message)
	if err != nil {
		zap.S().Panic(err)
	}
	return bytes
}
