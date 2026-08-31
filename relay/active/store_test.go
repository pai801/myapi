package active

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// newTestStore 构造独立存储实例，避免用例间共享 Global 状态
func newTestStore() *ActiveStore {
	return &ActiveStore{items: make(map[string]*ActiveRequest)}
}

// TestConcurrentUpdateAndListNoRace 在 -race 下真实触发并发路径：
// 多 goroutine 循环执行 Update（写共享 entry 字段）与 List（结构体值拷贝 + 计算 ElapsedMs）
func TestConcurrentUpdateAndListNoRace(t *testing.T) {
	s := newTestStore()
	const workers = 8
	const iterations = 200

	for i := 0; i < workers; i++ {
		s.Add(&ActiveRequest{
			RequestID: fmt.Sprintf("req-%d", i),
			TokenName: "tk",
			StartedAt: time.Now().UnixMilli(),
		})
	}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				s.Update(fmt.Sprintf("req-%d", w), func(req *ActiveRequest) {
					req.FirstTokenMs = int64(i)
					req.ElapsedMs = int64(i)
				})
			}
		}(w)
	}
	for r := 0; r < workers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				for _, item := range s.List() {
					_ = item.ElapsedMs
					_ = item.FirstTokenMs
					_ = item.TokenName
				}
			}
		}()
	}
	wg.Wait()
}

// TestListReturnsValueCopies 验证 List 返回值副本：修改副本不得影响内部状态，ElapsedMs 按实时重算
func TestListReturnsValueCopies(t *testing.T) {
	s := newTestStore()
	startedAt := time.Now().UnixMilli() - 1000
	s.Add(&ActiveRequest{RequestID: "req-1", TokenName: "tk", StartedAt: startedAt})

	list := s.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(list))
	}
	if list[0].ElapsedMs < 1000 {
		t.Fatalf("ElapsedMs should be computed from StartedAt, got %d", list[0].ElapsedMs)
	}

	// 修改副本：内部状态不应受影响
	list[0].TokenName = "mutated"
	list[0].ElapsedMs = -1

	again := s.List()
	if again[0].TokenName != "tk" {
		t.Fatalf("copy mutation leaked into store: TokenName = %q", again[0].TokenName)
	}
	if again[0].ElapsedMs < 0 {
		t.Fatalf("ElapsedMs should be recomputed per List call, got %d", again[0].ElapsedMs)
	}
}

// TestUpdateOverwritesSameKey 验证 Update 覆盖同 key 的语义；未知 key 不应创建条目
func TestUpdateOverwritesSameKey(t *testing.T) {
	s := newTestStore()
	s.Add(&ActiveRequest{RequestID: "req-1", TokenName: "before", StartedAt: time.Now().UnixMilli()})

	s.Update("req-1", func(req *ActiveRequest) {
		req.TokenName = "after"
		req.FirstTokenMs = 120
	})

	got := s.Get("req-1")
	if got == nil {
		t.Fatal("expected entry to exist after Update")
	}
	if got.TokenName != "after" || got.FirstTokenMs != 120 {
		t.Fatalf("Update should overwrite fields in place, got %+v", got)
	}

	s.Update("missing", func(req *ActiveRequest) { req.TokenName = "ghost" })
	if s.Get("missing") != nil {
		t.Fatal("Update on missing key must not create an entry")
	}
}
