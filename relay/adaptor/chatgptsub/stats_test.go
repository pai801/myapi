package chatgptsub

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStatsReportSuccess(t *testing.T) {
	m := NewStatsManager()

	// 先报一次失败让 errorRate 非零
	m.ReportResult(1, false, nil)
	require.Equal(t, 1.0, m.GetErrorRate(1))

	// 再报一次成功 → errorRate 应下降
	m.ReportResult(1, true, nil)
	rate := m.GetErrorRate(1)
	require.Less(t, rate, 1.0, "errorRate should decrease after success")
	require.Equal(t, 0.8, rate) // 1.0*0.8 + 0.2*0 = 0.8
}

func TestStatsReportFailure(t *testing.T) {
	m := NewStatsManager()

	// 第一次上报失败 → count==0, errorRate = sample = 1.0
	m.ReportResult(1, false, nil)
	require.Equal(t, 1.0, m.GetErrorRate(1))

	// 第二次上报失败 → EWMA: 0.2*1 + 0.8*1.0 = 1.0
	m.ReportResult(1, false, nil)
	require.Equal(t, 1.0, m.GetErrorRate(1))
}

func TestStatsEWMAConvergence(t *testing.T) {
	m := NewStatsManager()
	chID := 99

	// 第一次失败 → count==0, errorRate = 1.0
	m.ReportResult(chID, false, nil)

	// 连续上报 success（sample=0）, 验证 EWMA 趋近于 0
	// errRate_n = 0.8^n * 1.0
	for i := 0; i < 10; i++ {
		m.ReportResult(chID, true, nil)
	}

	rate := m.GetErrorRate(chID)
	expected := 1.0
	for i := 0; i < 10; i++ {
		expected = 0.2*0.0 + 0.8*expected
	}
	// expected ≈ 0.107
	require.InDelta(t, expected, rate, 0.001, "EWMA should converge toward 0 after repeated successes")

	// 再报 10 次 success, 进一步趋近 0
	for i := 0; i < 10; i++ {
		m.ReportResult(chID, true, nil)
	}

	rate = m.GetErrorRate(chID)
	expected2 := expected
	for i := 0; i < 10; i++ {
		expected2 = 0.2*0.0 + 0.8*expected2
	}
	// expected2 ≈ 0.0115
	require.InDelta(t, expected2, rate, 0.001, "EWMA should be even closer to 0")
	require.Less(t, rate, 0.02, "errorRate should be near 0 after 20 successes")
}

func TestStatsGetErrorRateDefault(t *testing.T) {
	m := NewStatsManager()

	rate := m.GetErrorRate(999)
	require.Equal(t, 0.0, rate, "unreported channel should return 0")
}

func TestStatsReset(t *testing.T) {
	m := NewStatsManager()

	m.ReportResult(1, false, nil)
	require.Equal(t, 1.0, m.GetErrorRate(1))

	m.Reset(1)
	require.Equal(t, 0.0, m.GetErrorRate(1), "after reset, errorRate should be 0")
}

func TestStatsConcurrent(t *testing.T) {
	m := NewStatsManager()
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				success := (j % 2) == 0
				m.ReportResult(n, success, nil)
				_ = m.GetErrorRate(n)
				_ = m.GetCandidateWeight(n)
			}
		}(i)
	}

	wg.Wait()
	// 如果 -race 通过则无数据竞争
}

func TestStatsGetCandidateWeight(t *testing.T) {
	m := NewStatsManager()

	// errorRate=0 → weight=1.0
	w := m.GetCandidateWeight(1)
	require.Equal(t, 1.0, w, "errorRate=0 should give weight=1.0")

	// errorRate=0.5 → weight=0 (1.0 - 0.5*5 = -1.5, clamped to 0)
	m.ReportResult(2, false, nil)                 // errorRate=1.0
	m.ReportResult(2, true, nil)                  // errorRate=0.8
	m.ReportResult(2, true, nil)                  // errorRate=0.64
	m.ReportResult(2, true, nil)                  // errorRate=0.512
	w = m.GetCandidateWeight(2)
	require.Equal(t, 0.0, w, "errorRate=0.5 should give weight=0")

	// errorRate=1.0 → weight=0 (clamped)
	m2 := NewStatsManager()
	m2.ReportResult(3, false, nil) // errorRate=1.0
	w = m2.GetCandidateWeight(3)
	require.Equal(t, 0.0, w, "errorRate=1.0 should clamp to 0")
}
