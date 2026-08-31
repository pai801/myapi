package ratio

import (
	"sync"
	"testing"
)

// TestConcurrentUpdateAndMarshalModelRatioNoRace 在 -race 下真实触发并发路径：
// 写方 UpdateModelRatioByJSONString 会 make 重赋值全局 map，
// 读者 ModelRatio2JSONString/GetModelRatio 与之并发，三处访问者必须全部持锁，
// 否则 marshal 裸读 map 会触发 fatal error: concurrent map read and map write
func TestConcurrentUpdateAndMarshalModelRatioNoRace(t *testing.T) {
	const workers = 4
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if err := UpdateModelRatioByJSONString(`{"gpt-4":15,"gpt-4o":2.5}`); err != nil {
				t.Errorf("update model ratio: %v", err)
				return
			}
		}
	}()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				_ = ModelRatio2JSONString()
				_ = GetModelRatio("gpt-4", 1)
			}
		}()
	}
	wg.Wait()
}

// TestConcurrentUpdateAndMarshalCompletionRatioNoRace 同上，覆盖 completionRatioLock 三处访问者
func TestConcurrentUpdateAndMarshalCompletionRatioNoRace(t *testing.T) {
	const workers = 4
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if err := UpdateCompletionRatioByJSONString(`{"gpt-4":2,"gpt-4o":2.5}`); err != nil {
				t.Errorf("update completion ratio: %v", err)
				return
			}
		}
	}()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				_ = CompletionRatio2JSONString()
				_ = GetCompletionRatio("gpt-4", 1)
			}
		}()
	}
	wg.Wait()
}
