// Copyright JAMF Software, LLC

package regattaserver

import (
	"context"
	"testing"

	"github.com/jamf/regatta/regattapb"
	"github.com/jamf/regatta/storage/table"
	"github.com/lni/dragonboat/v4/raftpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMetadataServer_Get(t *testing.T) {
	type fields struct {
		TableManager TableService
	}
	tests := []struct {
		name    string
		fields  fields
		want    *regattapb.MetadataResponse
		wantErr error
	}{
		{
			name: "Get metadata - no tables",
			fields: fields{
				TableManager: MockTableService{
					tables: []table.Table{},
				},
			},
			want: &regattapb.MetadataResponse{Tables: nil},
		},
		{
			name: "Get metadata - single table",
			fields: fields{
				TableManager: MockTableService{
					tables: []table.Table{
						{
							Name: "foo",
						},
					},
				},
			},
			want: &regattapb.MetadataResponse{Tables: []*regattapb.Table{
				{
					Name: "foo",
					Type: regattapb.Table_REPLICATED,
				},
			}},
		},
		{
			name: "Get metadata - multiple tables",
			fields: fields{
				TableManager: MockTableService{
					tables: []table.Table{
						{
							Name: "foo",
						},
						{
							Name: "bar",
						},
					},
				},
			},
			want: &regattapb.MetadataResponse{Tables: []*regattapb.Table{
				{
					Name: "foo",
					Type: regattapb.Table_REPLICATED,
				},
				{
					Name: "bar",
					Type: regattapb.Table_REPLICATED,
				},
			}},
		},
		{
			name: "Get metadata - deadline exceeded",
			fields: fields{
				TableManager: MockTableService{
					error: context.DeadlineExceeded,
				},
			},
			wantErr: status.Errorf(codes.Unavailable, "unknown err %v", context.DeadlineExceeded),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)
			m := &MetadataServer{
				Tables: tt.fields.TableManager,
			}
			got, err := m.Get(context.TODO(), &regattapb.MetadataRequest{})
			if tt.wantErr != nil {
				r.ErrorIs(err, tt.wantErr)
				return
			}
			r.Equal(tt.want, got)
		})
	}
}

func TestEntryToCommand(t *testing.T) {
	zero := uint64(0)
	tests := []struct {
		name    string
		entry   raftpb.Entry
		wantCmd *regattapb.Command
		wantErr error
	}{
		{
			name:    "ConfigChange Entry Type",
			entry:   raftpb.Entry{Type: raftpb.ConfigChangeEntry, Index: 0},
			wantCmd: &regattapb.Command{Type: regattapb.Command_DUMMY, LeaderIndex: &zero},
			wantErr: nil,
		},
		{
			name: "Valid Entry",
			entry: raftpb.Entry{
				Type: raftpb.EncodedEntry,
				Cmd:  []byte{0, 10, 12, 114, 101, 103, 97, 116, 116, 97, 45, 116, 101, 115, 116, 26, 23, 10, 12, 49, 54, 50, 56, 48, 48, 50, 54, 52, 57, 95, 48, 34, 7, 118, 97, 108, 117, 101, 95, 48},
			},
			wantCmd: &regattapb.Command{
				Kv: &regattapb.KeyValue{
					Key:   []byte("1628002649_0"),
					Value: []byte("value_0"),
				},
				Table:       []byte("regatta-test"),
				LeaderIndex: &zero,
			},
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)

			gotCmd, gotErr := entryToCommand(tt.entry)
			if tt.wantErr == nil {
				r.NoError(gotErr)
			} else {
				r.Error(gotErr)
			}

			r.Equal(tt.wantCmd.LeaderIndex, gotCmd.LeaderIndex)
			r.Equal(tt.wantCmd.Table, gotCmd.Table)
			r.Equal(tt.wantCmd.Type, gotCmd.Type)
			if tt.wantCmd.Kv != nil {
				r.Equal(tt.wantCmd.Kv.Value, gotCmd.Kv.Value)
				r.Equal(tt.wantCmd.Kv.Key, gotCmd.Kv.Key)
			}
		})
	}
}
