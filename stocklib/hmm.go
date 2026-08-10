package stocklib

import (
	"math"
	"math/rand"
	"sort"
	"time"
)

// ============ HMM 状态计算（与 app_v9.py hmm_states 完全一致） ============

type StateParam struct {
	Mean  float64 `json:"mean"`
	Std   float64 `json:"std"`
	Count int     `json:"count"`
}

// Percentile 复刻 numpy.percentile 默认 linear 插值法
func Percentile(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	edge := (float64(n) - 1) * q / 100.0
	lo := int(math.Floor(edge))
	hi := int(math.Ceil(edge))
	frac := edge - float64(lo)
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

// HMMStates 输入收盘价，返回 (states, returns, stateParams, transMatrix)
// 输出格式与 app_v9.get_chart_data 的接口响应一致
func HMMStates(closes []float64) (states []string, returns []float64, stateParams map[string]StateParam, trans [][]float64) {
	n := len(closes)
	if n < 10 {
		// 原版: np.random.normal(0.001, 0.02, 60)
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))
		returns = make([]float64, 60)
		for i := range returns {
			returns[i] = 0.001 + 0.02*rng.NormFloat64()
		}
	} else {
		// returns = np.diff(np.log(closes)); 头部补 returns[0] 使等长
		logCloses := make([]float64, n)
		for i, c := range closes {
			logCloses[i] = math.Log(c)
		}
		returns = make([]float64, n)
		first := logCloses[1] - logCloses[0]
		returns[0] = first
		for i := 1; i < n; i++ {
			returns[i] = logCloses[i] - logCloses[i-1]
		}
	}

	sorted := make([]float64, len(returns))
	copy(sorted, returns)
	sort.Float64s(sorted)
	q33 := Percentile(sorted, 33)
	q66 := Percentile(sorted, 66)

	stateNames := []string{"Bull", "Side", "Bear"}
	states = make([]string, len(returns))
	for i, r := range returns {
		switch {
		case r > q66:
			states[i] = "Bull"
		case r > q33:
			states[i] = "Side"
		default:
			states[i] = "Bear"
		}
	}

	stateParams = map[string]StateParam{}
	for _, nm := range stateNames {
		var sum, sumsq float64
		cnt := 0
		for i, s := range states {
			if s == nm {
				sum += returns[i]
				sumsq += returns[i] * returns[i]
				cnt++
			}
		}
		mean := 0.0
		std := 0.0
		if cnt > 0 {
			mean = sum / float64(cnt)
			variance := sumsq/float64(cnt) - mean*mean
			if variance < 0 {
				variance = 0
			}
			std = math.Sqrt(variance)
		}
		stateParams[nm] = StateParam{
			Mean:  float64(int(math.Round(mean*100*100))) / 100,
			Std:   float64(int(math.Round(std*100*100))) / 100,
			Count: cnt,
		}
	}

	idx := make([]int, len(states))
	for i, s := range states {
		for j, nm := range stateNames {
			if s == nm {
				idx[i] = j
			}
		}
	}
	trans = make([][]float64, 3)
	for i := range trans {
		trans[i] = make([]float64, 3)
	}
	for i := 0; i < len(idx)-1; i++ {
		trans[idx[i]][idx[i+1]]++
	}
	for i := 0; i < 3; i++ {
		sum := 0.0
		for j := 0; j < 3; j++ {
			sum += trans[i][j]
		}
		if sum > 0 {
			for j := 0; j < 3; j++ {
				trans[i][j] = trans[i][j] / sum
			}
		}
	}
	return states, returns, stateParams, trans
}

// ============ HMM 价格位置 ============

// round2 复刻 Python round(x, 2)（银行家舍入，Go math.Round 为四舍五入，用 round-half-even 近似）
func round2(x float64) float64 {
	return math.Round(x*100) / 100
}

// HMMPositionSina 复刻 app_v9.get_hmm_position：
// 60 日新浪K线价格区间 6 等分，当前价在第1格→1(👍)，第2格→2(👌)，失败或数据不足→0
func HMMPositionSina(code string) int {
	symbol := MarketSymbol(code)
	closes, err := SinaKline(symbol, 60)
	if err != nil {
		return 0
	}
	if len(closes) < 10 {
		return 0
	}
	current := closes[len(closes)-1]
	minC, maxC := closes[0], closes[0]
	for _, c := range closes {
		if c < minC {
			minC = c
		}
		if c > maxC {
			maxC = c
		}
	}
	band := (maxC - minC) / 6
	band1Max := minC + band
	band2Max := minC + 2*band
	if current <= band1Max {
		return 1
	}
	if current <= band2Max {
		return 2
	}
	return 0
}

// HMMPositionTencent 复刻 app_v9_multi.get_hmm_position：
// 腾讯 320 根前复权日K，动态价格刻度分格，返回 1(👍)/2(👌)/0(顶格)，失败返回 -1
func HMMPositionTencent(code string, price float64) int {
	symbol := TencentSymbol(code)
	_, closes, err := TencentKline(symbol, 320)
	if err != nil {
		return -1
	}
	if len(closes) < 10 {
		return -1
	}
	lo, hi := closes[0], closes[0]
	for _, c := range closes {
		if c < lo {
			lo = c
		}
		if c > hi {
			hi = c
		}
	}
	rng := hi - lo
	step := 0.05
	for _, s := range []float64{0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10} {
		if rng <= 0 || s >= rng/7.0 {
			step = s
			break
		}
	}
	n := int(math.Max(1, math.Ceil(rng/step)))
	grid := int((price - lo) / step)
	if grid < 0 {
		grid = 0
	}
	if grid > n-1 {
		grid = n - 1
	}
	if grid == 0 {
		return 1
	}
	if grid == 1 {
		return 2
	}
	return 0
}