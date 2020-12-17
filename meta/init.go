package meta

import (
	"context"

	"github.com/XiaoMi/pegasus-go-client/idl/replication"
	"github.com/XiaoMi/pegasus-go-client/idl/rrdb"
	"github.com/pegasus-kv/meta-proxy/rpc"
)

// Init meta API registration.
func Init() {
	rpc.Register("RPC_CM_QUERY_PARTITION_CONFIG_BY_INDEX", &rpc.MethodDefinition{
		RequestCreator: func() rpc.RequestArgs {
			return &rrdb.MetaQueryCfgArgs{
				Query: replication.NewQueryCfgRequest(),
			}
		},
		Handler: func(ctx context.Context, args rpc.RequestArgs) rpc.ResponseResult {
			// TODO(wutao)
			return nil
		},
	})
}
