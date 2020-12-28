package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/XiaoMi/pegasus-go-client/idl/base"
	"github.com/XiaoMi/pegasus-go-client/session"
	"github.com/bluele/gcache"
	"github.com/go-zookeeper/zk"
	"github.com/sirupsen/logrus"
)

// TODO(jiashuo) store config file
var zkAddrs = []string{""}
var zkTimeOut = 1000000000 // unit ns, equal 1s
var zkRoot = "/pegasus-cluster"
var zkWatcherCount = 1024

var globalClusterManager *ClusterManager

type ClusterManager struct {
	Mut    sync.RWMutex
	ZkConn *zk.Conn
	// table->TableInfoWatcher
	Tables gcache.Cache
	// metaAddrs->metaManager
	Metas map[string]*session.MetaManager
}

// zkContext cancels the goroutine that's watching the zkNode, when the watcher
// is evicted by LRU.
type zkContext struct {
	ctx    context.Context
	cancel context.CancelFunc
}

type TableInfoWatcher struct {
	tableName   string
	clusterName string
	metaAddrs   string
	event       <-chan zk.Event
	ctx         zkContext
}

// TODO(jishuo1) change log module
func initClusterManager() {
	zkConn, _, err := zk.Connect(zkAddrs, time.Duration(zkTimeOut))
	if err != nil {
		logrus.Panicf("failed to connect to zookeeper \"%s\": %s", zkAddrs, err)
	}

	tables := gcache.New(zkWatcherCount).LRU().EvictedFunc(func(key interface{}, value interface{}) {
		value.(*TableInfoWatcher).ctx.cancel()
		logrus.Warnf("[%s] evicted from table cache (capacity: %d)", key.(string), zkWatcherCount)
	}).Build() // TODO(jiashuo1) consider set expire time
	globalClusterManager = &ClusterManager{
		ZkConn: zkConn,
		Tables: tables,
		Metas:  make(map[string]*session.MetaManager),
	}

	logrus.Infof("init cluster manager: zk=%s, zkTimeOut=%d, zkRoot=%s, zkWatcherCount=%d",
		zkAddrs, zkTimeOut, zkRoot, zkWatcherCount)
}

func (m *ClusterManager) getMeta(table string) (*session.MetaManager, error) {
	var meta *session.MetaManager

	tableInfo, err := m.Tables.Get(table)
	if err == nil {
		meta = m.Metas[tableInfo.(*TableInfoWatcher).metaAddrs]
		if meta != nil {
			return meta, nil
		}
	}

	logrus.Infof("[%s] can't get cluster info from local cache, try fetch from zk.", table)
	m.Mut.Lock()
	defer m.Mut.Unlock()
	tableInfo, err = m.Tables.Get(table)
	if err != nil {
		tableInfo, err = m.newTableInfo(table)
		if err != nil {
			logrus.Errorf("[%s] get table info failed: %s", table, err)
			return nil, err
		}
		err = m.Tables.Set(table, tableInfo)
		if err != nil {
			logrus.Warnf("[%s] cluster info update local cache failed: %s", table, err)
		}
	}

	metaAddrs := tableInfo.(*TableInfoWatcher).metaAddrs
	meta = m.Metas[metaAddrs]
	if meta == nil {
		metaList, err := parseToMetaList(metaAddrs)
		if err != nil {
			logrus.Errorf("[%s] cluster addr[%s] format is err: %s", table, metaAddrs, err)
			return nil, base.ERR_INVALID_DATA
		}
		meta = session.NewMetaManager(metaList, session.NewNodeSession)
		m.Metas[metaAddrs] = meta
	}

	logrus.Infof("[%s] cluster info[%s(%s)] fetched from zk[%s] succeed.", table,
		tableInfo.(*TableInfoWatcher).clusterName, tableInfo.(*TableInfoWatcher).metaAddrs, zkAddrs)
	return meta, nil
}

// get table cluster info and watch it based table name from zk
// The zookeeper path layout:
// /<RegionPathRoot>/<table> =>
//                         {
//                           "cluster_name" : "clusterName",
//                           "meta_addrs" : "metaAddr1,metaAddr2,metaAddr3"
//                         }
func (m *ClusterManager) newTableInfo(table string) (*TableInfoWatcher, error) {
	path := fmt.Sprintf("%s/%s", zkRoot, table)
	value, _, watcherEvent, err := m.ZkConn.GetW(path)
	if err != nil {
		if err == zk.ErrNoNode {
			logrus.Errorf("[%s] cluster info doesn't exist on zk[%s(%s)], err: %s", table, zkAddrs, path, err)
			return nil, base.ERR_OBJECT_NOT_FOUND
		} else {
			logrus.Errorf("[%s] get cluster info from zk[%s(%s)] failed: %s", table, zkAddrs, path, err)
			return nil, base.ERR_ZOOKEEPER_OPERATION
		}
	}

	type clusterInfoStruct struct {
		Name      string `json:"cluster_name"`
		MetaAddrs string `json:"meta_addrs"`
	}
	var cluster = &clusterInfoStruct{}
	err = json.Unmarshal(value, cluster)
	if err != nil {
		logrus.Errorf("[%s] cluster info on zk[%s(%s)] format is invalid, err = %s", table, zkAddrs, path, err)
		return nil, base.ERR_INVALID_DATA
	}

	ctx, cancel := context.WithCancel(context.Background())
	tableInfo := &TableInfoWatcher{
		tableName:   table,
		clusterName: cluster.Name,
		metaAddrs:   cluster.MetaAddrs,
		event:       watcherEvent,
		ctx: zkContext{
			ctx:    ctx,
			cancel: cancel,
		},
	}
	go m.watchTableInfoChanged(tableInfo)

	return tableInfo, nil
}

func parseToMetaList(metaAddrs string) ([]string, error) {
	result := strings.Split(metaAddrs, ",")
	if len(result) < 2 {
		return []string{}, fmt.Errorf("the meta addrs[%s] is invalid", metaAddrs)
	}
	return result, nil
}

// parseToTableName extracts table name from the zookeeper path.
// The zookeeper path layout:
// /<RegionPathRoot>
//            /<table1> => {JSON}
//            /<table2> => {JSON}
func parseToTableName(path string) (string, error) {
	result := strings.Split(path, "/")
	if len(result) != 3 {
		return "", fmt.Errorf("the path[%s] is invalid", path)
	}

	return result[len(result)-1], nil
}

func (m *ClusterManager) watchTableInfoChanged(watcher *TableInfoWatcher) {
	select {
	case event := <-watcher.event:
		tableName, err := parseToTableName(event.Path)
		if err != nil {
			logrus.Panicf("zk path \"%s\" is corrupt, unable to parse table name: %s", event.Path, err)
		}
		if event.Type == zk.EventNodeDataChanged {
			tableInfo, err := m.newTableInfo(tableName)
			if err != nil {
				logrus.Panicf("[%s] get cluster info failed when trigger watcher: %s", tableName, err)
			}
			globalClusterManager.Mut.Lock()
			err = globalClusterManager.Tables.Set(tableName, tableInfo)
			globalClusterManager.Mut.Unlock()
			if err != nil {
				logrus.Panicf("[%s] local cache cluster info is updated to %s(%s) failed: %s",
					tableName, tableInfo.clusterName, tableInfo.metaAddrs, err)
			}
			logrus.Infof("[%s] local cache cluster info is updated to %s(%s) succeed", tableName,
				tableInfo.clusterName, tableInfo.metaAddrs)
		} else if event.Type == zk.EventNodeDeleted {
			globalClusterManager.Mut.Lock()
			success := globalClusterManager.Tables.Remove(tableName)
			globalClusterManager.Mut.Unlock()
			if !success {
				logrus.Panicf("[%s] local cache cluster info is removed failed!", tableName)
			}
			logrus.Infof("[%s] local cache cluster info is removed succeed", tableName)
		} else {
			logrus.Infof("[%s] cluster info is updated, type = %s.", tableName, event.Type.String())
		}

	case <-watcher.ctx.ctx.Done():
		logrus.Warnf("[%s] zk watcher is canceled from cache", watcher.tableName)
		return
	}
}
