package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "embed"
	"stocklib"
)

//go:embed page.html
var pageHTML string

const (
	PORT           = 18090
	CACHE_DIR      = "cache"
	SCAN_INTERVAL  = 1800 // 30分钟
	MAIN_BOARD_FS  = "m:0+t:6,m:1+t:2" // 沪深主板（x2 同款口径，不含科创/京/创业板）
	INDUSTRY_FS    = "m:90+t:2"       // 东财行业板块
	KLINE_BATCH    = 200
	KLINE_DAYS     = 300
	KLINE_WORKERS  = 8
	FIN_BATCH_SIZE = 450
	HITTRACK_FILE  = "/root/cow/data/hittrack.json"
)

const klineURL = "http://proxy.finance.qq.com/ifzqgtimg/appstock/app/newfqkline/get"

type SelectStock struct {
	Code     string   `json:"code"`
	Name     string   `json:"name"`
	Price    *float64 `json:"price"`
	Change   *float64 `json:"change"`
	Turnover *float64 `json:"turnover"`
	Sector   *string  `json:"sector"`
	NPYoy    *float64 `json:"np_yoy"`
	PubDate  *string  `json:"pub_date"`
	Ind1     *float64 `json:"ind1"`
	Ind2     *float64 `json:"ind2"`
}

type IndustryInfo struct {
	Name   string   `json:"name"`
	Change *float64 `json:"change"`
	NetIn  *float64 `json:"netin"`
}

type HCPoint struct {
	Date  string  `json:"d"`
	Close float64 `json:"c"`
}

type HitEntry struct {
	Name    string    `json:"name"`
	Sector  string    `json:"sector"`
	HitDate string    `json:"hit_date"`
	Base    float64   `json:"base"`
	Close   []HCPoint `json:"close"`
}

type Snapshot struct {
	GeneratedAt string           `json:"generated_at"`
	ElapsedSec  float64          `json:"elapsed_sec"`
	Total       int              `json:"total"`
	KlineDate   string           `json:"kline_date"`
	Industries  []IndustryInfo   `json:"industries"`
	Stocks      []SelectStock    `json:"stocks"`
	HitTrack    map[string]*HitEntry `json:"hit_track"`
}

var (
	snapMu   sync.RWMutex
	latest   *Snapshot
	scanMu   sync.Mutex
	scanning bool
)

func init() { os.MkdirAll(CACHE_DIR, 0755) }

func cachePath() string { return filepath.Join(CACHE_DIR, "select_snapshot.json") }

func round2(x float64) float64 { return float64(int(x*100+0.5)) / 100 }

func toFloatPtr(v interface{}) *float64 {
	switch x := v.(type) {
	case float64:
		return &x
	case string:
		if f, err := strconv.ParseFloat(x, 64); err == nil {
			return &f
		}
	}
	return nil
}

func toStrPtr(v interface{}) *string {
	if s, ok := v.(string); ok && s != "" {
		return &s
	}
	return nil
}

func MarketSymbol(code string) string {
	if strings.HasPrefix(code, "6") {
		return "sh" + code
	}
	return "sz" + code
}

func cleanCode(v interface{}) string {
	code, _ := v.(string)
	code = strings.TrimSpace(code)
	for len(code) < 6 {
		code = "0" + code
	}
	return code
}

// ============ 1. 沪深主板列表（东财 clist）============

func fetchMainBoard() []map[string]interface{} {
	var rows []map[string]interface{}
	for pn := 1; pn <= 100; pn++ {
		data, err := stocklib.FetchEM(map[string]string{
			"pn": strconv.Itoa(pn), "pz": "100", "fid": "f12",
			"fs": MAIN_BOARD_FS, "fields": "f12,f14,f2,f3,f8,f100",
		})
		if err != nil {
			log.Printf("[main] 主板列表第%d页失败: %v", pn, err)
			break
		}
		if len(data.Data.Diff) == 0 {
			break
		}
		rows = append(rows, data.Data.Diff...)
		if pn*100 >= data.Data.Total {
			break
		}
	}
	return rows
}

// ============ 2. 行业板块（东财 clist m:90+t:2）============

func fetchIndustries() []IndustryInfo {
	var out []IndustryInfo
	for pn := 1; pn <= 10; pn++ {
		data, err := stocklib.FetchEM(map[string]string{
			"pn": strconv.Itoa(pn), "pz": "100", "fid": "f104",
			"fs": INDUSTRY_FS, "fields": "f12,f14,f3,f104,f62",
		})
		if err != nil {
			log.Printf("[ind] 行业板块第%d页失败: %v", pn, err)
			break
		}
		for _, it := range data.Data.Diff {
			var inf IndustryInfo
			inf.Name, _ = it["f14"].(string)
			if inf.Name == "" {
				continue
			}
			inf.Change = toFloatPtr(it["f3"])
			// f62=主力净流入（元，有正有负），换算为亿元展示；原 f106 在板块场景无负值已废弃
			if raw := toFloatPtr(it["f62"]); raw != nil {
				yi := round2(*raw / 1e8)
				inf.NetIn = &yi
			}
			out = append(out, inf)
		}
		if len(data.Data.Diff) == 0 || len(out) >= data.Data.Total {
			break
		}
	}
	return out
}
// ============ 3. 腾讯单只K线（proxy.finance.qq.com，前复权，含当日换手率）============
// 注：该接口 param 多 symbol 只有第一个生效，必须单只请求；用磁盘缓存降低重复拉取

type klineResp struct {
	Code int `json:"code"`
	Msg  string `json:"msg"`
	Data map[string]struct {
		QFQDay [][]interface{} `json:"qfqday"`
		Day    [][]interface{} `json:"day"`
	} `json:"data"`
}

// KBar 精简K线（缓存/指标只用 date/open/close/换手率，避免 []interface{} 内存膨胀）
type KBar struct {
	Date string  `json:"d"`
	Open float64 `json:"o"`
	Clse float64 `json:"c"`
	Turn float64 `json:"t"`
}

func kNum(v interface{}) float64 {
	switch x := v.(type) {
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	case float64:
		return x
	default:
		return 0
	}
}

// parseKRow 腾讯K线一行 [date, open, close, high, low, vol, {}, 换手率%, 成交额万, ...]
func parseKRow(item []interface{}) KBar {
	if len(item) < 6 {
		return KBar{}
	}
	b := KBar{}
	b.Date, _ = item[0].(string)
	b.Open = kNum(item[1])
	b.Clse = kNum(item[2])
	if len(item) > 7 {
		b.Turn = kNum(item[7])
	}
	return b
}

func getKlineSingle(sym string) ([]KBar, error) {
	u := klineURL + "?param=" + url.QueryEscape(fmt.Sprintf("%s,day,,,%d,qfq", sym, KLINE_DAYS))
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var r klineResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.Code != 0 {
		return nil, fmt.Errorf("kline err code=%d msg=%s", r.Code, r.Msg)
	}
	node := r.Data[sym]
	raw := node.QFQDay
	if len(raw) == 0 {
		raw = node.Day
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("kline empty for %s", sym)
	}
	bars := make([]KBar, 0, len(raw))
	for _, it := range raw {
		bars = append(bars, parseKRow(it))
	}
	return bars, nil
}

type klineEntry struct {
	Date string  `json:"date"`
	Ts   int64   `json:"ts"`
	Bars []KBar  `json:"bars"`
}

const klineCacheFile = "cache/kline_day.json"

func loadKlineCache() map[string]klineEntry {
	out := map[string]klineEntry{}
	if err := stocklib.CacheReadJSON(klineCacheFile, &out); err != nil {
		return out
	}
	return out
}

func saveKlineCache(c map[string]klineEntry) {
	if err := stocklib.CacheWriteJSON(klineCacheFile, c); err != nil {
		log.Printf("[kline] 缓存保存失败: %v", err)
	}
}

// fetchKlineAll 单只拉取 + 磁盘缓存：
// - 缓存内 date==最新K线日 且 25分钟内拉取过的直接复用
// - 其余并发拉取（12 worker，错峰50ms），失败重试一次
// - 失败预算：连续失败300个即放弃本轮拉取（防风控雪崩），保留缓存旧值
func fetchKlineAll(symbols []string) map[string][]KBar {
	const freshSec = 25 * 60
	now := time.Now()
	known := loadKlineCache()

	var latestDay string
	for _, e := range known {
		if e.Date > latestDay {
			latestDay = e.Date
		}
	}
	need := make([]string, 0, len(symbols))
	for _, sym := range symbols {
		e, ok := known[sym]
		if ok && e.Date == latestDay && now.Unix()-e.Ts < freshSec {
			continue
		}
		need = append(need, sym)
	}
	log.Printf("[kline] 缓存命中 %d / %d, 需拉取 %d (基准日=%s)", len(symbols)-len(need), len(symbols), len(need), latestDay)

	result := map[string][]KBar{}
	if len(need) == 0 {
		for sym, e := range known {
			result[sym] = e.Bars
		}
		return result
	}

	var mu sync.Mutex
	var consecFail int
	aborted := false
	sem := make(chan struct{}, KLINE_WORKERS)
	var wg sync.WaitGroup
	for i, sym := range need {
		wg.Add(1)
		go func(s string, idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			time.Sleep(time.Duration(idx%KLINE_WORKERS) * 50 * time.Millisecond)
			mu.Lock()
			if aborted {
				mu.Unlock()
				return
			}
			mu.Unlock()
			arr, err := getKlineSingle(s)
			if err != nil {
				time.Sleep(500 * time.Millisecond)
				arr, err = getKlineSingle(s)
			}
			mu.Lock()
			if err != nil || arr == nil {
				consecFail++
				if consecFail >= 300 {
					aborted = true
					log.Printf("[kline] 连续失败>=300，本轮放弃K线拉取（保留缓存旧值）")
				}
				mu.Unlock()
				return
			}
			consecFail = 0
			lastDate := arr[len(arr)-1].Date
			known[s] = klineEntry{Date: lastDate, Ts: now.Add(time.Minute).Unix(), Bars: arr}
			result[s] = arr
			mu.Unlock()
			time.Sleep(50 * time.Millisecond)
		}(sym, i)
	}
	wg.Wait()

	for sym, e := range known {
		if _, ok := result[sym]; !ok {
			result[sym] = e.Bars
		}
	}
	if !aborted {
		saveKlineCache(known)
	}
	log.Printf("[kline] 完成: 新拉取%d, aborted=%v", len(result), aborted)
	return result
}

// ============ 4. 指标计算（复刻TDX公式）============

func ma(values []float64, n int) (float64, bool) {
	if len(values) < n {
		return 0, false
	}
	sum := 0.0
	for i := len(values) - n; i < len(values); i++ {
		sum += values[i]
	}
	return sum / float64(n), true
}

// calcInd1 指标1：涨=(C-REFC)/REFC, 换手=VOL/CAPITAL, 比率=MA(涨/换手,5),
// 方向=IF(C>O,IF(比率>0,1,-1),-1), 趋势=SUM(方向,10), 合一=趋势*0.5+MA(趋势,5)*0.5, 均线=MA(合一,10)
func calcInd1(b []KBar) *float64 {
	n := len(b)
	if n < 24 {
		return nil
	}
	ratio := make([]float64, 0, n)
	direction := make([]float64, 0, n)
	prevClose := 0.0
	for i := 0; i < n; i++ {
		o, c, to := b[i].Open, b[i].Clse, b[i].Turn
		if to <= 0 {
			return nil // 无换手率字段的来源不可靠
		}
		var r float64
		if prevClose > 0 {
			r = ((c - prevClose) / prevClose) / (to / 100)
		}
		ratio = append(ratio, r)
		if len(ratio) >= 5 {
			maRatio, _ := ma(ratio, 5)
			if c > o && maRatio > 0 {
				direction = append(direction, 1)
			} else {
				direction = append(direction, -1)
			}
		} else {
			direction = append(direction, -1) // 数据不足期冷启动
		}
		prevClose = c
	}
	// 趋势=SUM(方向,10) 逐bar
	trendSeries := make([]float64, 0, n)
	sum10 := 0.0
	for i := 0; i < len(direction); i++ {
		sum10 += direction[i]
		if i >= 10 {
			sum10 -= direction[i-10]
		}
		trendSeries = append(trendSeries, sum10)
	}
	// 合一=趋势*0.5+MA(趋势,5)*0.5
	heYi := make([]float64, 0, len(trendSeries))
	for i := 4; i < len(trendSeries); i++ {
		m5, _ := ma(trendSeries[:i+1], 5)
		heYi = append(heYi, trendSeries[i]*0.5+m5*0.5)
	}
	if len(heYi) < 10 {
		return nil
	}
	v, ok := ma(heYi, 10) // 均线=MA(合一,10)
	if !ok {
		return nil
	}
	rv := round2(v)
	return &rv
}

// calcInd2 指标2：ZD=(C/REFC-1)*100, X=ZD/(VOL/CAPITAL), 统=SUM(X>0,7)
func calcInd2(b []KBar) *float64 {
	n := len(b)
	if n < 8 {
		return nil
	}
	cnt := 0.0
	for i := n - 7; i < n; i++ {
		prevClose := b[i-1].Clse
		c := b[i].Clse
		to := b[i].Turn
		if to <= 0 || prevClose <= 0 {
			continue
		}
		if (c/prevClose-1)*100/(to/100) > 0 {
			cnt++
		}
	}
	return &cnt
}
// ============ 5. 净利润同比+公告日（10jqka业绩数据库 本地合并缓存）============
// 缓存: /root/cow/data/yjgg.jsonl（scripts/fetch_yjgg_bootstrap.py 建仓, fetch_yjgg_daily.py 每日12:00增量）
// 口径: 逐股取最新报告期; 同期内 notice>express>preview, 同种取 declare_date 最新; yoy 空值用上下限中值补

const finDataFile = "/root/cow/data/yjgg.jsonl"

var finCache struct {
	sync.Mutex
	mtime int64
	data  map[string]finInfo
}

func finTypeRank(t string) int {
	switch t {
	case "notice":
		return 0
	case "express":
		return 1
	default:
		return 2
	}
}

// loadFinLocal 读业绩数据库合并缓存（mtime 变化时自动重载, 30分钟周期内生效新数据）
func loadFinLocal() map[string]finInfo {
	st, err := os.Stat(finDataFile)
	if err != nil {
		log.Printf("[fin] 业绩缓存不可读: %v", err)
		return map[string]finInfo{}
	}
	finCache.Lock()
	defer finCache.Unlock()
	if st.ModTime().Unix() == finCache.mtime && len(finCache.data) > 0 {
		return finCache.data
	}
	type finRow struct {
		StockCode string   `json:"stock_code"`
		Report    string   `json:"report"`
		Forecast  string   `json:"forecast_type"`
		Declare   string   `json:"declare_date"`
		Yoy       *float64 `json:"parent_holder_net_profit_yoy"`
		LowYoy    *float64 `json:"parent_holder_net_profit_low_bound_yoy"`
		HighYoy   *float64 `json:"parent_holder_net_profit_high_bound_yoy"`
	}
	best := map[string]finRow{}
	f, err := os.Open(finDataFile)
	lines := 0
	if err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
		for sc.Scan() {
			var r finRow
			if json.Unmarshal(sc.Bytes(), &r) != nil || r.StockCode == "" {
				continue
			}
			lines++
			old, ok := best[r.StockCode]
			if !ok {
				best[r.StockCode] = r
				continue
			}
			// 新期必胜; 同期比类型优先级; 再比 declare_date
			if r.Report > old.Report ||
				(r.Report == old.Report &&
					(finTypeRank(r.Forecast) < finTypeRank(old.Forecast) ||
					(finTypeRank(r.Forecast) == finTypeRank(old.Forecast) && r.Declare > old.Declare))) {
				best[r.StockCode] = r
			}
		}
		f.Close()
	}
	out := make(map[string]finInfo, len(best))
	maxR := ""
	for code, r := range best {
		if r.Report > maxR {
			maxR = r.Report
		}
		fi2 := finInfo{}
		if r.Yoy != nil {
			v := *r.Yoy
			fi2.SJLTZ = &v
		} else if r.LowYoy != nil && r.HighYoy != nil {
			v := (*r.LowYoy + *r.HighYoy) / 2
			fi2.SJLTZ = &v
		}
		if len(r.Declare) >= 10 {
			d := r.Declare[:10]
			fi2.Notice = &d
		}
		out[code] = fi2
	}
	finCache.mtime = st.ModTime().Unix()
	finCache.data = out
	log.Printf("[fin] 业绩缓存载入: %d行, 有效股票%d, 最新期=%s", lines, len(out), maxR)
	return out
}

type finInfo struct {
	SJLTZ  *float64
	Notice *string
}

// fetchFinGrowth 东财 RPT_LICO_FN_CPD 路径（已停用, 保留供回滚参考）
func fetchFinGrowth(codes []string) map[string]finInfo {
	result := map[string]finInfo{}
	for i := 0; i < len(codes); i += FIN_BATCH_SIZE {
		end := i + FIN_BATCH_SIZE
		if end > len(codes) {
			end = len(codes)
		}
		quoted := make([]string, end-i)
		for j := i; j < end; j++ {
			quoted[j-i] = `"` + codes[j] + `"`
		}
		codeFilter := "(" + strings.Join(quoted, ",") + ")"
		rows, err := stocklib.FetchDatacenter(map[string]interface{}{
			"sortColumns": "REPORTDATE", "sortTypes": -1,
			"pageSize": 500, "pageNumber": 1,
			"reportName": "RPT_LICO_FN_CPD",
			"columns":    "SECURITY_CODE,REPORTDATE,SJLTZ,NOTICE_DATE",
			"filter":     "(SECURITY_CODE in " + codeFilter + ")",
		})
		if err != nil {
			log.Printf("[fin] 财务批次[%d] 异常: %v", i/FIN_BATCH_SIZE+1, err)
		} else if len(rows) == 0 {
			log.Printf("[fin] 财务批次[%d] %d只→0行", i/FIN_BATCH_SIZE+1, end-i)
		} else {
			for _, item := range rows {
				code, _ := item["SECURITY_CODE"].(string)
				if code == "" {
					continue
				}
				if _, exists := result[code]; !exists {
					fi := finInfo{SJLTZ: toFloatPtr(item["SJLTZ"])}
					if nd, ok := item["NOTICE_DATE"].(string); ok && len(nd) >= 10 {
						d := nd[:10]
						fi.Notice = &d
					}
					result[code] = fi
				}
			}
		}
		if end < len(codes) {
			time.Sleep(300 * time.Millisecond)
		}
	}
	return result
}

// ============ 6. 扫描编排 ============

// ============ 命中跟踪（5/10日历史，共享表 hittrack.json）============
func loadHitTrack() map[string]*HitEntry {
	var m map[string]*HitEntry
	data, err := os.ReadFile(HITTRACK_FILE)
	if err == nil {
		_ = json.Unmarshal(data, &m)
	}
	if m == nil {
		m = map[string]*HitEntry{}
	}
	return m
}

func saveHitTrack(m map[string]*HitEntry) {
	data, _ := json.MarshalIndent(m, "", "  ")
	_ = os.MkdirAll(filepath.Dir(HITTRACK_FILE), 0755)
	_ = os.WriteFile(HITTRACK_FILE, data, 0644)
}

// isHitCond 命中3条件：净利同比>0 且 指标1<=-7 且 行业净额>0且行业涨跌>0
func isHitCond(s SelectStock, indByName map[string]*IndustryInfo) bool {
	if s.NPYoy == nil || *s.NPYoy <= 0 {
		return false
	}
	if s.Ind1 == nil || *s.Ind1 > -7 {
		return false
	}
	sec := ""
	if s.Sector != nil {
		sec = *s.Sector
	}
	in := indByName[sec]
	return in != nil && in.NetIn != nil && *in.NetIn > 0 && in.Change != nil && *in.Change > 0
}

// updateHitTrack 更新命中跟踪表：命中→无条目或基准日非今天则替换；
// 未命中→已跟踪且未满11天则补录今日收盘
func updateHitTrack(stocks []SelectStock, industries []IndustryInfo, klineMap map[string][]KBar) map[string]*HitEntry {
	indByName := map[string]*IndustryInfo{}
	for i := range industries {
		indByName[industries[i].Name] = &industries[i]
	}
	today := time.Now().Format("2006-01-02")
	track := loadHitTrack()
	for _, s := range stocks {
		sec := ""
		if s.Sector != nil {
			sec = *s.Sector
		}
		hit := isHitCond(s, indByName)
		arr, okK := klineMap[MarketSymbol(s.Code)]
		last := 0.0
		if okK && len(arr) > 0 {
			last = arr[len(arr)-1].Clse
		}
		if hit {
			e := track[s.Code]
			// 无条目 或 已满11天 → 新建/重置；未满11天保留基准不替换
			if e == nil || len(e.Close) >= 11 {
				track[s.Code] = &HitEntry{
					Name: s.Name, Sector: sec, HitDate: today,
					Base: round2(last), Close: []HCPoint{{Date: today, Close: round2(last)}},
				}
			}
		}
		// 已在跟踪且未满11天 → 补录今日收盘（命中未重置/未命中 均适用）
		e := track[s.Code]
		if e != nil && len(e.Close) < 11 && last > 0 && today > e.HitDate && e.Close[len(e.Close)-1].Date != today {
			e.Close = append(e.Close, HCPoint{Date: today, Close: round2(last)})
		}
	}
	saveHitTrack(track)
	return track
}

func scanOnce() *Snapshot {
	start := time.Now()
	log.Printf("[scan] 开始一轮扫描")

	boardRows := fetchMainBoard()
	if len(boardRows) == 0 {
		log.Printf("[scan] 主板列表为空，跳过本轮")
		snapMu.RLock()
		s := latest
		snapMu.RUnlock()
		return s
	}
	symbols := make([]string, 0, len(boardRows))
	for _, it := range boardRows {
		code := cleanCode(it["f12"])
		if code == "" {
			continue
		}
		symbols = append(symbols, MarketSymbol(code))
	}

	// K线（8并发批量）与行业板块（单请求）并行
	var industries []IndustryInfo
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		industries = fetchIndustries()
	}()
	klineMap := fetchKlineAll(symbols)
	wg.Wait()
	npMap := loadFinLocal()

	// 组装
	stocks := make([]SelectStock, 0, len(boardRows))
	klineDate := ""
	for _, it := range boardRows {
		code := cleanCode(it["f12"])
		price := toFloatPtr(it["f2"])
		if price == nil || *price <= 0 {
			continue // 停牌/无效
		}
		fi, _ := npMap[code]
		var ind1, ind2 *float64
		if arr, okK := klineMap[MarketSymbol(code)]; okK {
			ind1 = calcInd1(arr)
			ind2 = calcInd2(arr)
			if len(arr) > 0 && arr[len(arr)-1].Date > klineDate {
				klineDate = arr[len(arr)-1].Date
			}
		}
		stocks = append(stocks, SelectStock{
			Code: code, Name: itName(it), Price: price,
			Change: toFloatPtr(it["f3"]), Turnover: toFloatPtr(it["f8"]),
			Sector: toStrPtr(it["f100"]), NPYoy: fi.SJLTZ, PubDate: fi.Notice,
			Ind1: ind1, Ind2: ind2,
		})
	}

	hitTrack := updateHitTrack(stocks, industries, klineMap)

	snap := &Snapshot{
		GeneratedAt: time.Now().Format("2006-01-02 15:04:05"),
		ElapsedSec:   round2(time.Since(start).Seconds()),
		Total:        len(stocks),
		KlineDate:    klineDate,
		Industries:   industries,
		Stocks:       stocks,
		HitTrack:     hitTrack,
	}
	if err := stocklib.CacheWriteJSON(cachePath(), snap); err != nil {
		log.Printf("[scan] 缓存写入失败: %v", err)
	}
	snapMu.Lock()
	latest = snap
	snapMu.Unlock()
	log.Printf("[scan] 完成: %d只, 行业%d个, 耗时%.1fs, kline日期=%s",
		snap.Total, len(industries), snap.ElapsedSec, snap.KlineDate)
	return snap
}

func itName(it map[string]interface{}) string {
	s, _ := it["f14"].(string)
	return s
}

// triggerScan 原子触发一次后台扫描；已有扫描进行中则跳过
func triggerScan() bool {
	scanMu.Lock()
	if scanning {
		scanMu.Unlock()
		return false
	}
	scanning = true
	scanMu.Unlock()
	go func() {
		defer func() {
			scanMu.Lock()
			scanning = false
			scanMu.Unlock()
		}()
		scanOnce()
	}()
	return true
}

// ============ 7. HTTP 服务 ============

func loadCache() {
	var snap Snapshot
	if err := stocklib.CacheReadJSON(cachePath(), &snap); err != nil {
		log.Printf("[cache] 无历史缓存: %v", err)
		return
	}
	snapMu.Lock()
	latest = &snap
	snapMu.Unlock()
	log.Printf("[cache] 载入缓存: %d只 @ %s", snap.Total, snap.GeneratedAt)
}

func apiSnapshot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	snapMu.RLock()
	s := latest
	snapMu.RUnlock()
	if s == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":"scan not finished yet"}`)
		return
	}
	json.NewEncoder(w).Encode(s)
}

func apiRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if triggerScan() {
		fmt.Fprint(w, `{"started":true,"msg":"扫描已触发"}`)
	} else {
		fmt.Fprint(w, `{"started":false,"msg":"已有扫描进行中"}`)
	}
}

func apiStatus(w http.ResponseWriter, r *http.Request) {
	scanMu.Lock()
	inProgress := scanning
	scanMu.Unlock()
	snapMu.RLock()
	var at string
	var total int
	if latest != nil {
		at = latest.GeneratedAt
		total = latest.Total
	}
	snapMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"scanning": inProgress, "generated_at": at, "total": total,
	})
}

func main() {
	loadCache()
	triggerScan()
	go func() {
		ticker := time.NewTicker(time.Duration(SCAN_INTERVAL) * time.Second)
		for range ticker.C {
			triggerScan()
		}
	}()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, pageHTML)
			return
		}
		http.NotFound(w, r)
	})
	http.HandleFunc("/api/snapshot", apiSnapshot)
	http.HandleFunc("/api/refresh", apiRefresh)
	http.HandleFunc("/api/status", apiStatus)

	log.Printf("stock-select listening on :%d", PORT)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", PORT), nil))
}
