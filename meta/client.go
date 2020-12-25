package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/XiaoMi/pegasus-go-client/idl/base"
	"github.com/sirupsen/logrus"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/XiaoMi/pegasus-go-client/session"
	"github.com/bluele/gcache"
	"github.com/go-zookeeper/zk"
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

type Context struct {
	ctx    context.Context
	cancel context.CancelFunc
}

type TableInfoWatcher struct {
	tableName   string
	clusterName string
	metaAddrs   string
	event       <-chan zk.Event
	context     Context
}

// TODO(jishuo1) change log module
func initClusterManager() {
	zkConn, _, err := zk.Connect(zkAddrs, time.Duration(zkTimeOut))
	if err != nil {
		panic(fmt.Errorf("connect to %s failed: %s", zkAddrs, err))
	}

	tables := gcache.New(zkWatcherCount).LRU().EvictedFunc(func(key interface{}, value interface{}) {
		value.(*TableInfoWatcher).context.cancel()
		logrus.Warnf("table cache has exceeded %d and remove the oldest table[%s]", zkWatcherCount, key.(string))
	}).Build() // TODO(jiashuo1) consider set expire time
	globalClusterManager = &ClusterManager{
		ZkConn: zkConn,
		Tables: tables,
		Metas:  make(map[string]*session.MetaManager),
	}
}

func (m *ClusterManager) getMetaConnector(table string) (*session.MetaManager, error) {
	var metaConnector *session.MetaManager

	tableInfo, err := globalClusterManager.Tables.Get(table)
	if err == nil {
		metaConnector = globalClusterManager.Metas[tableInfo.(*TableInfoWatcher).metaAddrs]
		if metaConnector != nil {
			return metaConnector, nil
		}
	}

	log.Printf("[%s] can't get cluster info from local cache, try fetch from zk.", table)
	globalClusterManager.Mut.Lock()
	tableInfo, err = globalClusterManager.Tables.Get(table)
	if err != nil {
		tableInfo, err = m.getTableInfo(table)
		if err != nil {
			logrus.Errorf("get table[%s] info failed: %s", table, err)
			return nil, err
		}
		err = globalClusterManager.Tables.Set(table, tableInfo)
		if err != nil {
			log.Printf("[%s] cluster info update local cache failed: %s", table, err)
		}
	}

	metaAddrs := tableInfo.(*TableInfoWatcher).metaAddrs
	metaConnector = globalClusterManager.Metas[metaAddrs]
	if metaConnector == nil {
		metaList, err := parseToMetaList(metaAddrs)
		if err != nil {
			logrus.Errorf("The table[%s] cluster addr[%s] format is err: %s", table, metaAddrs, err)
			return nil, base.ERR_INVALID_DATA
		}
		metaConnector = session.NewMetaManager(metaList, session.NewNodeSession)
		globalClusterManager.Metas[metaAddrs] = metaConnector
	}

	globalClusterManager.Mut.Unlock()
	return metaConnector, nil
}

// get table cluster info and watch it based table name from zk
// The zookeeper path layout:
// <RegionPathRoot>/<table> =>
//                         {
//                           "cluster_name" : "clusterName",
//                           "meta_addrs" : "metaAddr1,metaAddr2,metaAddr3"
//                         }
func (m *ClusterManager) getTableInfo(table string) (*TableInfoWatcher, error) {
	path := fmt.Sprintf("%s/%s", zkRoot, table)
	value, _, watcherEvent, err := globalClusterManager.ZkConn.GetW(path)
	if err != nil {
		if err == zk.ErrNoNode {
			logrus.Errorf("the table[%s] info doesn't exist on zk[%s], err: %s", table, path, err)
			return nil, base.ERR_CLUSTER_NOT_FOUND
		} else {
			logrus.Errorf("get table[%s] info from zk[%s] failed: %s", table, path, err)
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
		logrus.Errorf("table[%s] info on zk[%s] format is invalid, err = %s", table, path, err)
		return nil, base.ERR_INVALID_DATA
	}

	ctx, cancel := context.WithCancel(context.Background())
	tableInfo := TableInfoWatcher{
		tableName:   table,
		clusterName: cluster.Name,
		metaAddrs:   cluster.MetaAddrs,
		event:       watcherEvent,
		context: Context{
			ctx:    ctx,
			cancel: cancel,
		},
	}
	go watchTableInfoChanged(tableInfo)

	return &tableInfo, nil
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
// <RegionPathRoot>
//            /<table1> => {JSON}
//            /<table2> => {JSON}
func parseToTableName(path string) (string, error) {
	result := strings.Split(path, "/")
	if len(result) < 2 {
		return "", fmt.Errorf("the path[%s] is invalid", path)
	}

	return result[len(result)-1], nil
}

func watchTableInfoChanged(watcher TableInfoWatcher) {
	select {
	case event := <-watcher.event:
		tableName, err := parseToTableName(event.Path)
		if err != nil {
			logrus.Panicf("zk path \"%s\" is corrupt, unable to parse table name: %s", event.Path, err)
		}
		if event.Type == zk.EventNodeDataChanged {
			tableInfo, err := globalClusterManager.getTableInfo(tableName)
			if err != nil {
				log.Printf("[%s] get cluster info failed when triger watcher: %s", tableName, err)
				return
			}
			log.Printf("[%s] cluster info is updated to %s(%s)", tableName, tableInfo.clusterName, tableInfo.metaAddrs)
			globalClusterManager.Mut.Lock()
			err = globalClusterManager.Tables.Set(tableName, tableInfo)
			globalClusterManager.Mut.Unlock()
			if err != nil {
				log.Printf("[%s] cluster info local cache updated to %s(%s) failed: %s",
					tableName, tableInfo.clusterName, tableInfo.metaAddrs, err)
			}
		} else if event.Type == zk.EventNodeDeleted {
			log.Printf("[%s] cluster info is removed from zk", tableName)
			globalClusterManager.Mut.Lock()
			success := globalClusterManager.Tables.Remove(tableName)
			globalClusterManager.Mut.Unlock()
			if !success {
				log.Printf("[%s] cluster info local cache removed failed!", tableName)
			}
		} else {
			log.Printf("[%s] cluster info is updated, type = %s.", tableName, event.Type.String())
		}

	case <-watcher.context.ctx.Done():
		logrus.Warnf("table[%s] watcher is canceled from cache", watcher.tableName)
		return
	}
}
