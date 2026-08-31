package model

import (
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/logger"
	"sync"
	"time"
)

const (
	BatchUpdateTypeUserQuota = iota
	BatchUpdateTypeUsedQuota
	BatchUpdateTypeChannelUsedQuota
	BatchUpdateTypeRequestCount
	BatchUpdateTypeCount // if you add a new type, you need to add a new map and a new lock
)

var batchUpdateStores []map[int]int64
var batchUpdateLocks []sync.Mutex

func init() {
	for i := 0; i < BatchUpdateTypeCount; i++ {
		batchUpdateStores = append(batchUpdateStores, make(map[int]int64))
		batchUpdateLocks = append(batchUpdateLocks, sync.Mutex{})
	}
}

func InitBatchUpdater() {
	go func() {
		for {
			time.Sleep(time.Duration(config.BatchUpdateInterval) * time.Second)
			batchUpdate()
		}
	}()
}

func addNewRecord(type_ int, id int, value int64) {
	batchUpdateLocks[type_].Lock()
	defer batchUpdateLocks[type_].Unlock()
	if _, ok := batchUpdateStores[type_][id]; !ok {
		batchUpdateStores[type_][id] = value
	} else {
		batchUpdateStores[type_][id] += value
	}
}

// getPendingUserQuotaDelta 返回批量缓冲中该用户尚未落盘的 quota 变化量（含正负）。
// 批量模式下 DB 值落后于真实可用额度，任何"从 DB 回读回写缓存"的路径
// （缓存回源、管理员修正、消费后刷新）都必须叠加此差值，
// 否则会用陈旧 DB 值覆盖缓存，使批量间隔内的额度检查形同虚设（可被持续透支）。
// 已知竞态：batchUpdate 已换出缓冲但尚未写盘时本函数返回 0，此窗口内的回读回写
// 会以未落盘的 DB 旧值覆盖缓存——预扣在途（delta 为负）时缓存偏大、可能多放行，
// 退回在途（delta 为正）时偏小、保守拒绝。窗口为毫秒级且需恰好并发命中，
// 偏差由下一次回读回写（消费结算/回源/管理员修正）或 TTL 过期重置自愈。
func getPendingUserQuotaDelta(id int) int64 {
	batchUpdateLocks[BatchUpdateTypeUserQuota].Lock()
	defer batchUpdateLocks[BatchUpdateTypeUserQuota].Unlock()
	return batchUpdateStores[BatchUpdateTypeUserQuota][id]
}

func batchUpdate() {
	logger.Log.Infof("batch update started")
	for i := 0; i < BatchUpdateTypeCount; i++ {
		batchUpdateLocks[i].Lock()
		store := batchUpdateStores[i]
		batchUpdateStores[i] = make(map[int]int64)
		batchUpdateLocks[i].Unlock()
		// TODO: maybe we can combine updates with same key?
		for key, value := range store {
			switch i {
			case BatchUpdateTypeUserQuota:
				err := increaseUserQuota(key, value)
				if err != nil {
					logger.Log.Errorf("failed to batch update user quota: " + err.Error())
				}
			case BatchUpdateTypeUsedQuota:
				updateUserUsedQuota(key, value)
			case BatchUpdateTypeRequestCount:
				updateUserRequestCount(key, int(value))
			case BatchUpdateTypeChannelUsedQuota:
				updateChannelUsedQuota(key, value)
			}
		}
	}
	logger.Log.Infof("batch update finished")
}
