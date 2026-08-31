package middleware

import (
	"sync"
	"testing"

	"github.com/pai801/myapi/model"
)

// TestConcurrentNextAutoChannelNoRace 守护 nextAutoChannel 对包级 map 的读改写被互斥锁覆盖：
// 锁一旦被移除，本用例在 -race 下会以 concurrent map writes 暴露回归
func TestConcurrentNextAutoChannelNoRace(t *testing.T) {
	channels := []*model.Channel{
		{Id: 1},
		{Id: 2},
		{Id: 3},
	}
	const workers = 8
	const iterations = 200

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				nextAutoChannel("race-test-same-group", channels)
			}
		}()
	}
	// 并发写不同 key 同样是对 map 结构的写入，一并纳入锁覆盖验证
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				nextAutoChannel("race-test-group-"+string(rune('a'+w)), channels)
			}
		}(w)
	}
	wg.Wait()
}
